package runtime

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"issue-desk-worker/internal/intake"
)

type fullTestCompletion struct {
	send func(context.Context) error
	err  error
}

type jobCompletion struct {
	ID   int64
	Repo string
	Err  error
}

// Fatal stops: permanent acquisition refusals (including contract mismatch),
// invalid assignments/epochs, duplicate jobs, repository limit mismatches and
// failed requeues. Drain these for five minutes; only their deadline enables
// the five-second cancellation join. Ordinary stops and temporary acquisition
// failures do not cancel owned jobs.
const shutdownDrainLimit = 5 * time.Minute
const shutdownCancelLimit = 5 * time.Second

// scheduler is the state of one Run: the reported settings and status
// (scheduler_status.go), the maintenance windows and job acquisition
// (scheduler_acquire.go), and the loop, drain and shutdown below.
type scheduler struct {
	r Runner
	// ctx is the caller's context; taskCtx is cancelled when Run returns or a
	// drain expires, and is what jobs and background goroutines run under.
	ctx     context.Context
	taskCtx context.Context
	cancel  context.CancelFunc

	// Settings and status reported in heartbeats.
	checks            map[string]any
	settings          settingsExecutor
	sharedSettings    bool
	settingsVersion   atomic.Value
	heartbeatMu       sync.Mutex
	configuredVersion string
	nextConfigCheck   time.Time
	changedConfig     bool
	// settingsUpdates holds only the newest snapshot not yet applied.
	settingsUpdates    chan intake.ConfigSnapshot
	heartbeatRequested chan struct{}

	running            atomic.Int64
	maintenance        atomic.Bool
	acquisitionStopped atomic.Bool
	// A failed poll check blocks all request windows until a later check succeeds.
	pollMaintenanceFailed bool
	fullTestRepo          string
	fullTestDone          chan fullTestCompletion
	fullTestYieldsToJobs  bool

	// Each attempt after a temporary failure is a new acquisition request.
	retryTimer *time.Timer
	retryC     <-chan time.Time
	retryDelay time.Duration

	// Jobs owned by this run.
	active   map[int64]string
	byRepo   map[string]int
	complete chan jobCompletion
	wg       sync.WaitGroup

	background          sync.WaitGroup
	drainExpired        bool
	closeEventsOnReturn bool
}

func (r Runner) Run(ctx context.Context) error {
	if err := r.applyDefaults(); err != nil {
		return err
	}
	r.leaseEvents = newLeaseEvents(r.leaseEvent, r.Now)
	s := &scheduler{r: r, ctx: ctx, closeEventsOnReturn: true}
	defer s.closeEvents()
	if err := s.loadStartupSettings(); err != nil {
		return err
	}
	s.nextConfigCheck = r.Now().Add(time.Minute)
	s.retryDelay = time.Second
	defer s.stopRetry()
	s.taskCtx, s.cancel = context.WithCancel(ctx)
	if err := s.heartbeat(ctx); err != nil {
		s.cancel()
		return err
	}
	defer s.shutdown()
	s.startSettingsPublisher()
	s.startHeartbeats()
	s.active = map[int64]string{}
	s.byRepo = map[string]int{}
	s.complete = make(chan jobCompletion, r.Capacity)
	return s.loop()
}

// applyDefaults checks the runner and fills the unset intervals and clock.
func (r *Runner) applyDefaults() error {
	if r.Executor == nil || !r.Executor.Ready() {
		return ErrExecutorUnavailable
	}
	if r.API == nil {
		return errors.New("受付APIが必要です")
	}
	if r.Capacity < 1 || r.Capacity > 8 {
		return errors.New("capacityは1から8にしてください")
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.PollInterval <= 0 {
		r.PollInterval = 5 * time.Second
	}
	if r.HeartbeatInterval <= 0 {
		r.HeartbeatInterval = 3 * time.Minute
	}
	if r.RenewInterval <= 0 {
		r.RenewInterval = time.Minute
	}
	for repo, limit := range r.RepoLimits {
		if repo == "" || limit < 1 || limit > r.Capacity {
			return errors.New("リポジトリの実行枠が不正です")
		}
	}
	return nil
}

func (s *scheduler) closeEvents() {
	if s.closeEventsOnReturn {
		s.r.leaseEvents.close()
	}
}

func (s *scheduler) stopRetry() {
	if s.retryTimer != nil {
		s.retryTimer.Stop()
	}
}

// shutdown cancels the run and joins the jobs and background goroutines.
// After an expired drain the join is bounded by shutdownCancelLimit.
func (s *scheduler) shutdown() {
	s.cancel()
	if !s.drainExpired {
		s.wg.Wait()
		if s.fullTestDone != nil {
			_ = s.completeFullTest(s.taskCtx, <-s.fullTestDone)
		}
		s.background.Wait()
		return
	}
	done := make(chan struct{})
	// A timed-out cancellation join may leave producers alive. Transfer
	// queue ownership to the existing waiter, rather than dropping their
	// later events when Run returns. Logging itself is never joined.
	s.closeEventsOnReturn = false
	go func() {
		s.wg.Wait()
		s.background.Wait()
		s.r.leaseEvents.close()
		close(done)
	}()
	timer := time.NewTimer(shutdownCancelLimit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Print("scheduler event=cancel_deadline reason=cleanup_timeout")
	}
}

// loop checks the settings, polls for maintenance and jobs, and waits for the
// next completion, retry, tick or wake.
func (s *scheduler) loop() error {
	ticker := time.NewTicker(s.r.PollInterval)
	defer ticker.Stop()
	configTicker := time.NewTicker(time.Minute)
	defer configTicker.Stop()
	interval := s.r.MaintenanceInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	maintenanceTimer := time.NewTimer(interval)
	defer maintenanceTimer.Stop()
	maintenanceDue := false
	poll := true
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		s.checkConfig()
		if s.changedConfig && len(s.active) == 0 && s.fullTestDone == nil {
			return ErrConfigChanged
		}
		if (poll || maintenanceDue) && !s.changedConfig && s.retryC == nil {
			full := s.occupiedSlots() >= s.r.Capacity
			if err := s.runMaintenance(full, poll); err != nil {
				return err
			}
			// Restart after completion, so request throttling and this timer cannot
			// drift into alternating missed windows.
			if !maintenanceTimer.Stop() {
				select {
				case <-maintenanceTimer.C:
				default:
				}
			}
			maintenanceTimer.Reset(interval)
			maintenanceDue = false
			if poll {
				if err := s.acquire(); err != nil {
					return err
				}
			}
			poll = false
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case tested := <-s.fullTestDone:
			if err := s.completeFullTest(s.taskCtx, tested); err != nil {
				return err
			}
			// Give the released repository an acquisition poll before another full test.
			s.fullTestYieldsToJobs = true
			poll = true
		case finished := <-s.complete:
			delete(s.active, finished.ID)
			s.byRepo[finished.Repo]--
			poll = true
		case <-s.retryC:
			s.retryC = nil
			poll = true
		case <-maintenanceTimer.C:
			maintenanceDue = true
		case <-ticker.C:
			poll = true
		case <-configTicker.C:
		case <-s.r.Wake:
			poll = true
		}
	}
}

// drain stops acquisition and waits up to shutdownDrainLimit for the owned
// jobs. No new jobs are added after entering drain. The capacity-sized
// completion buffer plus consumption here allows every sender to reach
// wg.Done, even when completions arrive while acquisition/validation is still
// returning.
func (s *scheduler) drain(err error, id int64, reason string) error {
	err = &schedulerStopError{cause: err, id: id, reason: reason}
	s.acquisitionStopped.Store(true)
	s.requestHeartbeat()
	log.Printf("scheduler job=%d event=draining reason=%s limit_seconds=%d", id, reason, int(shutdownDrainLimit/time.Second))
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	drainCtx, cancel := context.WithTimeout(s.taskCtx, shutdownDrainLimit)
	defer func() {
		if errors.Is(drainCtx.Err(), context.DeadlineExceeded) && s.ctx.Err() == nil {
			log.Printf("scheduler job=%d event=drain_deadline reason=%s", id, reason)
			s.drainExpired = true
			s.cancel()
		}
		cancel()
	}()
	for {
		select {
		case <-done:
			if s.fullTestDone != nil {
				_ = s.completeFullTest(drainCtx, <-s.fullTestDone)
			}
			return err
		case tested := <-s.fullTestDone:
			_ = s.completeFullTest(drainCtx, tested)
		case <-s.complete:
		case <-drainCtx.Done():
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			return err
		}
	}
}

// startJob records the job against its repository and runs it.
func (s *scheduler) startJob(assignment intake.Assignment) {
	key := strings.ToLower(assignment.Job.Repo)
	s.retryDelay = time.Second
	s.active[assignment.Job.ID] = key
	s.byRepo[key]++
	s.running.Add(1)
	s.wg.Add(1)
	go func(a intake.Assignment) {
		defer s.wg.Done()
		err := s.r.execute(s.taskCtx, a)
		if err != nil {
			log.Printf("scheduler job=%d event=job_error reason=%s", a.Job.ID, jobErrorKind(err))
		}
		s.running.Add(-1)
		s.requestHeartbeat()
		s.complete <- jobCompletion{a.Job.ID, strings.ToLower(a.Job.Repo), err}
	}(assignment)
	if s.acquisitionStopped.Swap(false) {
		s.requestHeartbeat()
	}
}
