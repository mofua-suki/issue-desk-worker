package execution

import (
	"context"
	"errors"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"time"
)

// Keep recent observations bounded independently of the number of retries.
const maxTestSteps = 100

// A missing duration on a started step survives a process crash without inventing zero.
func (e *Executor) saveTiming(r *journal.Record) error {
	if r.Timing == nil || (e.Store == nil && e.stageStore == nil) {
		return nil
	}
	return e.saveStage(r)
}

func (e *Executor) beginTimingSteps(r *journal.Record, names []string) ([]int, error) {
	if r.Timing == nil {
		return nil, nil
	}
	if r.Timing.TestAttempts == nil {
		r.Timing.TestAttempts = make(map[string]int)
	}
	// Older journals do not have counters; recover both groups before eviction.
	for _, step := range r.Timing.TestSteps {
		if (step.Step == "setupCommand" || step.Step == "prettier") && step.Attempt > r.Timing.TestAttempts[step.Step] {
			r.Timing.TestAttempts[step.Step] = step.Attempt
		}
	}
	attempt := r.Timing.TestAttempts[names[0]] + 1
	r.Timing.TestAttempts[names[0]] = attempt
	if excess := len(r.Timing.TestSteps) + len(names) - maxTestSteps; excess > 0 {
		if excess > len(r.Timing.TestSteps) {
			excess = len(r.Timing.TestSteps)
		}
		r.Timing.TestSteps = append([]journal.TestStepTiming(nil), r.Timing.TestSteps[excess:]...)
	}
	indices := make([]int, 0, len(names))
	for _, name := range names {
		indices = append(indices, len(r.Timing.TestSteps))
		r.Timing.TestSteps = append(r.Timing.TestSteps, journal.TestStepTiming{WorkerID: e.Config.Host, Step: name, Attempt: attempt, Status: "not_run"})
	}
	return indices, e.saveTiming(r)
}

func (e *Executor) timedSandbox(ctx context.Context, r *journal.Record, steps []int, index int, command []string, timeout int) (process.Result, error) {
	if len(steps) == 0 {
		return e.sandbox(ctx, r, command, timeout)
	}
	step := &r.Timing.TestSteps[steps[index]]
	step.Status = "missing"
	if err := e.saveTiming(r); err != nil {
		return process.Result{}, err
	}
	started := time.Now()
	out, err := e.sandbox(ctx, r, command, timeout)
	if out.Started {
		elapsed := time.Since(started).Milliseconds()
		step.ElapsedMillis = &elapsed
	}
	switch {
	case out.Interrupted || (!out.Started && ctx.Err() != nil):
		step.Status = "interrupted"
	case !out.Started:
		step.Status = "missing"
	case err != nil || out.Code != 0:
		step.Status = "failed"
	default:
		step.Status = "success"
	}
	if saveErr := e.saveTiming(r); saveErr != nil {
		return out, errors.Join(err, saveErr)
	}
	return out, err
}
