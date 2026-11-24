// Package runtime owns job lifetime and cancellation. The API client owns no business decisions.
package runtime

import (
	"context"
	"errors"
	"log"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/requeststore"
)

var ErrExecutorUnavailable = errors.New("実行器が利用できないため、仕事の取得とheartbeat送信を開始しません")

// ErrConfigChanged ends Run once the jobs of an old configuration have ended,
// so the worker restarts with the new one. It is not a startup failure.
var ErrConfigChanged = errors.New("実行設定が更新されたため、新しい設定で再起動します")

type API interface {
	Config(context.Context) (intake.ConfigSnapshot, error)
	Heartbeat(context.Context, intake.HostStatus) error
	Next(context.Context, int) (intake.Assignment, error)
	Renew(context.Context, int64, int64) (intake.Assignment, error)
	State(context.Context, int64, int64, intake.State) error
}

type Executor interface {
	Ready() bool
	Execute(context.Context, intake.Job, func(context.Context) error) (intake.State, error)
}

// UnavailableExecutor intentionally cannot turn a real job into a pretend successful result.
type UnavailableExecutor struct{}

func (UnavailableExecutor) Ready() bool { return false }
func (UnavailableExecutor) Execute(context.Context, intake.Job, func(context.Context) error) (intake.State, error) {
	return intake.State{}, ErrExecutorUnavailable
}

type Runner struct {
	CanAcquire           func() bool
	Status               func() intake.HostStatus
	StartupChecks        map[string]any
	ConfigurationVersion string
	RepoLimits           map[string]int
	PollMaintenanceFor   func(context.Context, []string) error
	MaintenanceInterval  time.Duration
	MaintenanceFor       func(context.Context, []string) error
	// MaintenanceWithFullTest keeps request handling on the scheduler and
	// allows only full-test execution to run in a reserved slot.
	MaintenanceWithFullTest func(context.Context, []string, bool, requeststore.StartFullTest) error
	// BusyMaintenanceFor は、ジョブが動いているリポジトリと、枠が埋まったPCの全リポジトリで、待たせない依頼だけを処理する。
	BusyMaintenanceFor func(context.Context, []string) error
	Revision           *string
	ImageRevision      *string
	OnMaintenanceError func(error)
	// OnHeartbeatError receives background heartbeat failures, thinned while the
	// same failure continues.
	OnHeartbeatError  func(error)
	Wake              <-chan struct{}
	Maintenance       func(context.Context) error
	API               API
	Executor          Executor
	Capacity          int
	Capabilities      map[string]any
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	RenewInterval     time.Duration
	Now               func() time.Time
	leaseEvent        func(int64, int64, string, string)
	leaseEvents       *leaseEvents
}

func (r Runner) execute(ctx context.Context, a intake.Assignment) (returned error) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loopCtx, stopRenew := context.WithCancel(jobCtx)
	defer stopRenew()
	guard := newLeaseGuard(r, jobCtx, cancel, a)
	startedAt := time.Now().UTC()
	defer func() {
		cancel()
		<-guard.done
		if err := r.recordLeaseFailure(guard, a, startedAt); err != nil && returned == nil {
			returned = err
		}
	}()
	fence := func(checkCtx context.Context) error {
		err := guard.renew(checkCtx)
		if err != nil {
			cancel()
		}
		return err
	}
	done := make(chan struct{})
	renewErr := make(chan error, 1)
	go func() {
		defer close(done)
		if err := r.renewUntilDone(loopCtx, jobCtx, cancel, guard); err != nil {
			renewErr <- err
		}
	}()
	executionJob := *a.Job
	executionJob.LeaseEpoch = a.Lease.Epoch
	result, err := r.Executor.Execute(jobCtx, executionJob, fence)
	failedAt := r.Now()
	failurePhase := "execution"
	defer func() {
		if returned != nil {
			log.Printf("scheduler job=%d event=result_unavailable phase=%s reason=%s", a.Job.ID, failurePhase, jobErrorKind(returned))
		}
	}()
	// An execution error is not a final failed result. Leave the journal intact
	// for reconciliation/resumption after lease expiry and redistribution.
	if err != nil && !errors.Is(err, intake.ErrJobReleased) {
		detail := executionErrorDetail(err)
		logExecutionError(a.Job.ID, detail)
	}
	// Join periodic renewal before the final ownership check, so normal shutdown cannot
	// cancel a renewal between that check and the state report.
	stopRenew()
	<-done
	if errors.Is(err, intake.ErrJobReleased) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, leaseErr := guard.current(); leaseErr != nil {
		return leaseErr
	}
	select {
	case e := <-renewErr:
		return e
	default:
	}
	if err != nil {
		if jobCtx.Err() == nil {
			r.reportExecutionError(jobCtx, guard, a.Job.ID, err, failedAt)
		}
		return err
	}
	if jobCtx.Err() != nil {
		return jobCtx.Err()
	}
	switch result.Status {
	case "held", "failed", "merged", "stopped", "queued":
	default:
		return errors.New("実行器が引き渡せる状態を返しませんでした")
	}
	failurePhase = "ownership_check"
	if err := fence(jobCtx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.reportState(ctx, jobCtx, guard, a, result, &failurePhase)
}

// recordLeaseFailure saves why the lease guard stopped the job, if it did.
func (r Runner) recordLeaseFailure(guard *leaseGuard, a intake.Assignment, startedAt time.Time) error {
	guard.mu.Lock()
	reason, at := guard.failureReason, guard.failureAt
	guard.mu.Unlock()
	if reason == "" {
		return nil
	}
	recorder, ok := r.Executor.(interface {
		RecordLeaseDiagnostic(intake.Job, string, time.Time, time.Time) error
	})
	if !ok {
		return nil
	}
	job := *a.Job
	job.LeaseEpoch = a.Lease.Epoch
	if err := recorder.RecordLeaseDiagnostic(job, reason, at, startedAt); err != nil {
		log.Printf("lease job=%d epoch=%d event=diagnostic_save_failed", job.ID, job.LeaseEpoch)
		return err
	}
	return nil
}

// renewUntilDone renews the lease every RenewInterval, or a third of the time
// left if sooner, until loopCtx ends. It returns the error that stopped the
// job, or nil when the loop was simply stopped.
func (r Runner) renewUntilDone(loopCtx, jobCtx context.Context, cancel context.CancelFunc, guard *leaseGuard) error {
	for {
		if loopCtx.Err() != nil {
			return nil
		}
		lease, err := guard.current()
		if err != nil {
			return err
		}
		left := lease.ExpiresAt.Sub(r.Now())
		wait := r.RenewInterval
		if left/3 < wait {
			wait = left / 3
		}
		if wait <= 0 {
			wait = time.Nanosecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-loopCtx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			if loopCtx.Err() != nil {
				return nil
			}
			err := guard.renew(loopCtx)
			if err != nil {
				// Stopping this loop after successful execution is not a job failure. A real lease
				// refusal, timeout or parent cancellation still stops the job and prevents reporting.
				if loopCtx.Err() != nil && jobCtx.Err() == nil && errors.Is(err, context.Canceled) {
					return nil
				}
				cancel()
				return err
			}
		}
	}
}

// reportState sends the final state, retrying temporary failures within the
// lease, and marks it reported. phase names the step a failure belongs to.
func (r Runner) reportState(ctx, jobCtx context.Context, guard *leaseGuard, a intake.Assignment, result intake.State, phase *string) error {
	// The executor must flush usage and finish required review before returning a final state.
	lease, leaseErr := guard.current()
	if leaseErr != nil {
		return leaseErr
	}
	reportCtx, stopReport := context.WithTimeout(jobCtx, min(5*time.Minute, lease.ExpiresAt.Sub(r.Now())))
	defer stopReport()
	reportError := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := guard.current(); err != nil {
			return err
		}
		return reportCtx.Err()
	}
	for attempt := 0; attempt < 13; attempt++ {
		*phase = "state_report"
		err := r.API.State(reportCtx, a.Job.ID, lease.Epoch, result)
		if err == nil {
			if observer, ok := r.Executor.(interface{ Reported(int64, int64) error }); ok {
				*phase = "mark_reported"
				return observer.Reported(a.Job.ID, lease.Epoch)
			}
			return nil
		}
		if reportCtx.Err() != nil {
			return reportError()
		}
		// Only a temporary failure (transport, timeout, a 5xx other than a
		// contract mismatch) can succeed when sent again; the saved result is
		// sent again by recovery otherwise.
		if intake.TemporaryFailureReason(err) == "" {
			var refusal *intake.APIError
			if errors.As(err, &refusal) && refusal.LeaseLost() {
				return guard.lost(err)
			}
			return err
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-reportCtx.Done():
			timer.Stop()
			return reportError()
		case <-timer.C:
		}
	}
	return errors.New("最終結果の送信を確認できません。保存した同じ結果から再開します")
}
