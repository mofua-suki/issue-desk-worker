package execution

import (
	"errors"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// saveRejectedStart is called only after the initial checkpoint failed, before
// setup or the provider call. Other failures and ambiguous transport errors
// must continue through the ordinary interrupted-start allowance.
func (e *Executor) saveRejectedStart(r *journal.Record, cause error) error {
	var refusal *intake.APIError
	if !errors.As(cause, &refusal) || refusal == nil {
		return cause
	}
	r.RejectedStart = &journal.RejectedStart{JobID: r.ID, Generation: r.OpenRunGeneration, Run: r.OpenRun, Epoch: r.OpenRunEpoch}
	if err := e.Store.Save(r); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Compare the epoch saved with the open start, not the current lease epoch.
// Reacquiring a lease updates r.Epoch before the interrupted start is reclaimed.
func rejectedOpenRun(r *journal.Record) bool {
	e := r.RejectedStart
	return e != nil && e.JobID == r.ID && e.Generation == r.OpenRunGeneration && e.Run > 0 && e.Run == r.OpenRun && e.Epoch > 0 && e.Epoch == r.OpenRunEpoch
}
