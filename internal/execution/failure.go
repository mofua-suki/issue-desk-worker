package execution

import (
	"context"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

// ExecutionError retains the original error and the current execution record.
type ExecutionError struct {
	Err   error
	Stage string
}

func (e *ExecutionError) Error() string          { return e.Err.Error() }
func (e *ExecutionError) Unwrap() error          { return e.Err }
func (e *ExecutionError) ExecutionStage() string { return e.Stage }

// SafeExecutionReason only permits complete, locally authored fixed messages.
func (e *ExecutionError) SafeExecutionReason() string {
	if push, ok := e.Err.(*gitwork.PushError); ok {
		switch push.Error() {
		case gitwork.PushBranchReason, gitwork.PushAuthReason, gitwork.PushHookReason, gitwork.PushConnectionReason, gitwork.PushOtherReason:
			return push.Error()
		}
	}
	for _, reason := range publicationErrors {
		if e.Err.Error() == reason.Error() {
			return reason.Error()
		}
	}
	return ""
}

type failureRecordKey struct{}
type failureRecord struct {
	record     *journal.Record
	started    bool
	generation int
	epoch      int64
}

func trackFailureRecord(ctx context.Context, r *journal.Record) {
	if tracker, ok := ctx.Value(failureRecordKey{}).(*failureRecord); ok {
		if !tracker.started && r != nil {
			r.StartStageTiming(tracker.generation, tracker.epoch)
			tracker.started = true
		}
		tracker.record = r
	}
}
func failureStage(r *journal.Record) string {
	if r == nil {
		return ""
	}
	switch r.Stage {
	case "implement", "implement_report", "test", "test_report", "review", "publish", "merge", "deploy", "done", "held", "failed", "stopped":
		return r.Stage
	}
	return ""
}
