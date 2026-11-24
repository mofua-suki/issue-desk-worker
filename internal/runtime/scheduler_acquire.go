package runtime

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"issue-desk-worker/internal/intake"
)

// runMaintenance opens the maintenance windows of one poll: idle
// repositories (or the whole PC when nothing runs) unless the PC is full,
// then the busy repositories. Only a cancelled run stops the loop.
func (s *scheduler) runMaintenance(full bool, heavy bool) error {
	r := s.r
	idle := []string{}
	for repo := range r.RepoLimits {
		if key := strings.ToLower(repo); key != s.fullTestRepo && s.byRepo[key] == 0 {
			idle = append(idle, repo)
		}
	}
	if heavy && !full && r.PollMaintenanceFor != nil && len(idle) > 0 {
		s.maintenance.Store(true)
		err := r.PollMaintenanceFor(s.taskCtx, idle)
		s.maintenance.Store(s.fullTestDone != nil)
		s.pollMaintenanceFailed = err != nil
		if err != nil {
			return s.maintenanceFailed(err)
		}
	}
	if s.pollMaintenanceFailed {
		return nil
	}
	if !full && ((r.MaintenanceWithFullTest != nil || r.MaintenanceFor != nil) && len(idle) > 0 || heavy && r.Maintenance != nil && s.occupiedSlots() == 0) {
		s.maintenance.Store(true)
		var err error
		if r.MaintenanceWithFullTest != nil && len(idle) > 0 {
			err = r.MaintenanceWithFullTest(s.taskCtx, idle, s.fullTestDone == nil && !s.fullTestYieldsToJobs, s.startFullTest)
		} else if r.MaintenanceFor != nil {
			err = r.MaintenanceFor(s.taskCtx, idle)
		} else {
			err = r.Maintenance(s.taskCtx)
		}
		s.maintenance.Store(s.fullTestDone != nil)
		if err := s.maintenanceFailed(err); err != nil {
			return err
		}
	}
	if r.BusyMaintenanceFor != nil {
		full = full || s.occupiedSlots() >= r.Capacity
		busy := []string{}
		for repo := range r.RepoLimits {
			// 枠が埋まったPCでは上の窓が開かないので、ジョブの無いリポジトリもここで受ける。
			if key := strings.ToLower(repo); key != s.fullTestRepo && (full || s.byRepo[key] > 0) {
				busy = append(busy, repo)
			}
		}
		if len(busy) > 0 {
			s.maintenance.Store(true)
			err := r.BusyMaintenanceFor(s.taskCtx, busy)
			s.maintenance.Store(s.fullTestDone != nil)
			if err := s.maintenanceFailed(err); err != nil {
				return err
			}
		}
	}
	return nil
}

// startFullTest reserves a slot and excludes the repository from acquisition
// until the result is saved and its delivery is attempted.
func (s *scheduler) startFullTest(repo string, run func(context.Context) (func(context.Context) error, error)) bool {
	key := strings.ToLower(repo)
	if s.fullTestDone != nil || s.r.Capacity-len(s.active) < 2 || s.byRepo[key] != 0 {
		return false
	}
	if _, ok := s.r.API.(interface {
		NextFor(context.Context, int, []string) (intake.Assignment, error)
	}); !ok || s.r.RepoLimits == nil {
		return false
	}
	done := make(chan fullTestCompletion, 1)
	s.fullTestRepo, s.fullTestDone = key, done
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		send, err := run(s.taskCtx)
		done <- fullTestCompletion{send: send, err: err}
	}()
	return true
}

func (s *scheduler) completeFullTest(ctx context.Context, tested fullTestCompletion) error {
	err := tested.err
	if tested.send != nil {
		if err = ctx.Err(); err == nil {
			err = tested.send(ctx)
		}
	}
	s.fullTestRepo, s.fullTestDone = "", nil
	s.maintenance.Store(false)
	return s.maintenanceFailed(err)
}

func (s *scheduler) occupiedSlots() int {
	slots := len(s.active)
	if s.fullTestDone != nil {
		slots++
	}
	return slots
}

// maintenanceFailed reports a maintenance error, and returns the context
// error when the run was cancelled.
func (s *scheduler) maintenanceFailed(err error) error {
	if err == nil {
		return nil
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if s.r.OnMaintenanceError != nil {
		s.r.OnMaintenanceError(err)
	}
	return nil
}

// acquire requests jobs while the PC and a repository have room. A temporary
// failure schedules a retry; any other refusal or invalid assignment drains.
func (s *scheduler) acquire() error {
	r := s.r
	eligible := []string{}
	for s.occupiedSlots() < r.Capacity {
		if r.CanAcquire != nil && !r.CanAcquire() {
			s.fullTestYieldsToJobs = false
			return nil
		}
		eligible = eligible[:0]
		for repo, limit := range r.RepoLimits {
			if key := strings.ToLower(repo); key != s.fullTestRepo && s.byRepo[key] < limit {
				eligible = append(eligible, repo)
			}
		}
		if r.RepoLimits != nil && len(eligible) == 0 {
			return nil
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
		assignment, err := s.next(eligible)
		if err != nil {
			if intake.TemporaryFailureReason(err) != "" {
				s.waitAfterTemporaryFailure()
				return nil
			}
			return s.drain(err, 0, "acquisition_rejected")
		}
		if err := assignment.Validate(r.Now()); err != nil {
			return s.drain(err, 0, "invalid_assignment")
		}
		s.fullTestYieldsToJobs = false
		if assignment.Job == nil {
			s.retryDelay = time.Second
			if s.acquisitionStopped.Swap(false) {
				s.requestHeartbeat()
			}
			return nil
		}
		if err := s.admit(assignment); err != nil {
			return err
		}
		s.startJob(assignment)
	}
	return nil
}

// next requests one assignment, limited to the eligible repositories when the
// API supports it.
func (s *scheduler) next(eligible []string) (intake.Assignment, error) {
	if filtered, ok := s.r.API.(interface {
		NextFor(context.Context, int, []string) (intake.Assignment, error)
	}); ok && s.r.RepoLimits != nil {
		return filtered.NextFor(s.taskCtx, s.r.Capacity, eligible)
	}
	return s.r.API.Next(s.taskCtx, s.r.Capacity)
}

// waitAfterTemporaryFailure stops acquisition until the backed-off retry.
func (s *scheduler) waitAfterTemporaryFailure() {
	s.acquisitionStopped.Store(true)
	log.Print("scheduler job=0 event=acquisition_wait reason=temporary_acquisition_failure")
	s.retryTimer = time.NewTimer(s.retryDelay)
	s.retryC = s.retryTimer.C
	s.retryDelay = min(s.retryDelay*2, 10*time.Second)
	s.requestHeartbeat()
}

// admit checks an assignment's epoch, duplication and repository limit. A job
// outside the repository limits is requeued before draining.
func (s *scheduler) admit(assignment intake.Assignment) error {
	job := assignment.Job
	if job.LeaseEpoch != 0 && job.LeaseEpoch != assignment.Lease.Epoch {
		return s.drain(errors.New("配布の仕事とleaseのepochが一致しません"), job.ID, "assignment_epoch_mismatch")
	}
	if _, duplicate := s.active[job.ID]; duplicate {
		return s.drain(errors.New("実行中の仕事が重複して配布されました"), job.ID, "duplicate_assignment")
	}
	if s.r.RepoLimits == nil {
		return nil
	}
	key := strings.ToLower(job.Repo)
	limit := s.r.Capacity
	found := false
	for repo, n := range s.r.RepoLimits {
		if strings.EqualFold(repo, job.Repo) {
			limit = n
			found = true
			break
		}
	}
	if !found || key == s.fullTestRepo || s.byRepo[key] >= limit {
		if err := s.r.API.State(s.taskCtx, job.ID, assignment.Lease.Epoch, intake.State{Status: "queued"}); err != nil {
			return s.drain(err, job.ID, "requeue_failed")
		}
		return s.drain(errors.New("受付の配布が設定のリポジトリ枠と一致しません"), job.ID, "repository_mismatch")
	}
	return nil
}
