package execution

import (
	"context"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// One report the intake refuses must not stop the whole PC from starting.
func TestRecoverContinuesPastRefusedReport(t *testing.T) {
	for status, wantErr := range map[int]bool{400: false, 404: false, 409: false, 401: true, 403: true, 500: true} {
		f := newPipeline(t, "approve")
		f.StateStatus = status
		state := encode(intake.State{Status: "held", HoldReason: "prerequisite_missing"})
		r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "held", Branch: f.Branch, Provider: "codex", Epoch: 3, FinalState: state}
		if err := f.Executor.Store.Save(r); err != nil {
			t.Fatal(err)
		}
		err := f.Executor.Recover(context.Background())
		if (err != nil) != wantErr {
			t.Fatalf("status=%d err=%v", status, err)
		}
		saved, loadErr := f.Executor.Store.Load(1)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if saved.FinalReported {
			t.Fatalf("status=%d 拒否された報告を送信済みにしました", status)
		}
	}
}
