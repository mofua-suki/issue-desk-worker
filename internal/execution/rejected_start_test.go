package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/jobspec"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestRejectedCheckpointResumeAcrossGenerations(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 4
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			answer.JSON = reportWith("status", "question")
		}
		return answer, err
	}
	for generation := 0; generation < 3; generation++ {
		for n := 0; n < 3; n++ {
			f.StateStatus = 409
			_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			var refusal *intake.APIError
			if !errors.As(err, &refusal) {
				t.Fatalf("generation %d refusal %d: %v", generation, n, err)
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil || !rejectedOpenRun(r) || r.InterruptedRuns != 0 || r.UnansweredRuns != 0 || r.Runs != generation+1 {
				t.Fatalf("generation %d refusal %d: %+v %v", generation, n, r, err)
			}
			if len(f.Calls) != generation {
				t.Fatal("provider was called before checkpoint", f.Calls)
			}
			f.Job.LeaseEpoch++ // Reacquisition must not replace the epoch of the recorded start.
		}
		f.StateStatus = 0
		f.Job.LeaseEpoch = 3 // The fixture accepts epoch 3 on successful state reports.
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.HoldReason != "question" || len(f.Calls) != generation+1 {
			t.Fatalf("generation %d: %+v %v calls=%v", generation, state, err, f.Calls)
		}
		r, err := f.Executor.Store.Load(1)
		if err != nil || r.InterruptedRuns != 0 || r.UnansweredRuns != 0 || r.Runs != generation+1 || r.RejectedStart != nil {
			t.Fatal(r, err)
		}
		nextGeneration(t, f, state)
	}
}

func TestRejectedStartMatchesOnlyLocalOpenRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*journal.Record)
		want   bool
	}{
		{"matching", func(*journal.Record) {}, true},
		{"other job", func(r *journal.Record) { r.RejectedStart.JobID++ }, false},
		{"other generation", func(r *journal.Record) { r.RejectedStart.Generation++ }, false},
		{"other start", func(r *journal.Record) { r.RejectedStart.Run++ }, false},
		{"other epoch", func(r *journal.Record) { r.RejectedStart.Epoch++ }, false},
		{"no evidence", func(r *journal.Record) { r.RejectedStart = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &journal.Record{ID: 1, Stage: "implement", Runs: 1, OpenRun: 1, OpenRunGeneration: 0, OpenRunEpoch: 3, InterruptedRuns: 2, RejectedStart: &journal.RejectedStart{JobID: 1, Generation: 0, Run: 1, Epoch: 3}}
			tc.change(r)
			reclaimed := reclaimInterruptedRun(Descriptor{Descriptor: jobspec.Descriptor{Generation: 0}}, r, startedExecution{})
			if reclaimed != tc.want || (r.UnansweredRuns == 0) != tc.want || r.RejectedStart != nil {
				t.Fatal(reclaimed, r)
			}
			// Reapplying the same journal cannot recover the start twice.
			before, _ := json.Marshal(r)
			reclaimInterruptedRun(Descriptor{Descriptor: jobspec.Descriptor{Generation: 0}}, r, startedExecution{})
			after, _ := json.Marshal(r)
			if string(before) != string(after) {
				t.Fatal(string(before), string(after))
			}
		})
	}
}

func TestAnswerBeforeLaterCheckpointRejectionStillCounts(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			f.StateStatus = 409
		}
		return answer, err
	}
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	var refusal *intake.APIError
	if !errors.As(err, &refusal) {
		t.Fatal(err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil || r.Runs != 1 || r.OpenRun != 0 || r.RejectedStart != nil || !hasJSON(r.Result) {
		t.Fatal(r, err)
	}
}

func TestRejectedStartPreservesLegacyCounts(t *testing.T) {
	for _, unanswered := range []int{0, 2} {
		r := &journal.Record{ID: 1, Stage: "implement", Runs: 3 + unanswered + 1, OpenRun: 3 + unanswered + 1, OpenRunGeneration: 2, OpenRunEpoch: 3, InterruptedRuns: 2, UnansweredRuns: unanswered, ReportCompletionRuns: 1}
		r.RejectedStart = &journal.RejectedStart{JobID: 1, Generation: 2, Run: r.OpenRun, Epoch: 3}
		if !reclaimInterruptedRun(Descriptor{Descriptor: jobspec.Descriptor{Generation: 2}}, r, startedExecution{}) || r.UnansweredRuns != unanswered || r.ReportCompletionRuns != 1 || r.InterruptedRuns != 2 {
			t.Fatal(r)
		}
		count := syncImplementationRuns(Descriptor{Descriptor: jobspec.Descriptor{Generation: 2}}, r, startedExecution{}, false)
		if count != 3 || count+nonGenerationRuns(r) != 4+unanswered {
			t.Fatal(r, count)
		}
	}
}

func TestAnswerImmediatelyBeforeCancellationKeepsRun(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := f.Executor.Call
	answered := false
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			answered = err == nil && hasJSON(answer.JSON)
			cancel()
		}
		return answer, err
	}
	_, _ = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	r, err := f.Executor.Store.Load(1)
	if !answered || ctx.Err() != context.Canceled || err != nil || r == nil || r.Runs != 1 || r.RejectedStart != nil {
		t.Fatalf("record=%+v err=%v", r, err)
	}
}
