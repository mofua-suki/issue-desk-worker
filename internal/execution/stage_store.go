package execution

import "issue-desk-worker/internal/journal"

func (e *Executor) saveStage(r *journal.Record) error {
	if e.stageStore != nil {
		return e.stageStore.Save(r)
	}
	return e.Store.Save(r)
}
