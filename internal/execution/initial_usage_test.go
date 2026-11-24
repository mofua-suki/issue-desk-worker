package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

type usageDecisionTransport struct {
	next   *intake.Client
	checks int
}

func TestReportCompletionPreservesLaterImplementationUsageAdmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		generation int
		resumes    int
		pending    string
		admitted   bool
		remote     string
	}{
		{name: "next-generation", generation: 1, admitted: true},
		{name: "resumed-generation", generation: 2, resumes: 1, admitted: true},
		{name: "missing-prior-start", generation: 2, admitted: true},
		{name: "same-generation-is-automatic"},
		{name: "review-is-automatic", generation: 1, pending: "review:head"},
		{name: "provider-switch-is-automatic", generation: 1, pending: "provider-switch:head"},
		{name: "environment-retry-is-automatic", generation: 1, pending: "environment-retry:head"},
		{name: "permission-retry-is-automatic", generation: 1, pending: "permission-retry:head"},
		{name: "usage-limit-retry-is-automatic", generation: 1, pending: "usage-limit-retry:head"},
		{name: "conflict-from-base-is-automatic", generation: 1, pending: "conflict-from-base:head"},
		{name: "remote-final", generation: 1, admitted: true, remote: "final"},
		{name: "remote-checkpoint", generation: 1, admitted: true, remote: "checkpoint"},
		{name: "remote-resumed-generation", generation: 2, resumes: 1, admitted: true, remote: "final"},
		{name: "remote-same-generation-is-automatic", remote: "checkpoint"},
		{name: "remote-review-is-automatic", generation: 1, pending: "review:head", remote: "final"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			tr := completionAPI(t, f)
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			d.View["reimplementExpected"] = encode(map[string]any{"generation": 0, "usageAdmission": true, "usageAdmissionUsed": "initial-admission"})
			f.Job.Raw, f.CurrentView = encode(d), d.View
			base := f.Executor.Call
			normal, supplements := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.ReportOnly {
					supplements++
					zero := int64(0)
					return providers.Answer{Session: req.Session, JSON: reportWith("status", "question"), Outcome: "completed", Usage: providers.Usage{Input: &zero, Output: &zero}}, nil
				}
				answer, err := base(ctx, req)
				normal++
				answer = conversationAnswer(t, req, answer)
				if normal == 1 {
					answer.JSON = reportWith("assumptions", []any{})
				} else {
					answer.JSON = reportWith("status", "question")
				}
				return answer, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.HoldReason != "question" || normal != 1 || supplements != 1 {
				t.Fatal(state, err, normal, supplements)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved.Runs != 2 {
				t.Fatal(saved, err)
			}
			// Simulate acknowledgement and another user generation (or automatic re-entry).
			// Execute reloads the persisted counts, including the supplemented report.
			saved.FinalReported = true
			if err := f.Executor.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			d.Generation = tc.generation
			d.View["implementationResumes"] = encode(tc.resumes)
			var view map[string]json.RawMessage
			if err := json.Unmarshal(state.View, &view); err != nil {
				t.Fatal(err)
			}
			if tc.remote == "checkpoint" {
				// Use the state actually sent before the report-only call, rather
				// than constructing a convenient remote counter in the test.
				var checkpoint intake.State
				if err := json.Unmarshal(tr.checkpoints[len(tr.checkpoints)-1], &checkpoint); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(checkpoint.View, &view); err != nil {
					t.Fatal(err)
				}
			}
			if tc.remote != "" {
				d.HeadSHA = publishContinuationFixture(t, f)
				if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
					t.Fatal(err)
				}
				if record, err := f.Executor.Store.Load(1); err != nil || record != nil {
					t.Fatal("journal was not removed", record, err)
				}
			}
			d.View["workerExecution"] = view["workerExecution"]
			d.View["reimplementExpected"] = encode(map[string]any{"generation": tc.generation, "usageAdmission": true, "usageAdmissionUsed": "next-admission"})
			if tc.pending != "" {
				d.View["pending"] = encode([]any{map[string]any{"id": tc.pending}})
			}
			f.Job.Raw, f.CurrentView = encode(d), d.View
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.HoldReason != "question" || normal != 2 || supplements != 1 {
				t.Fatal(state, err, normal, supplements)
			}
			if len(tr.checks) != 3 {
				t.Fatalf("expected initial, supplement, and later usage checks: %s", tr.checks)
			}
			for i, raw := range tr.checks {
				var event struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(raw, &event); err != nil {
					t.Fatal(err)
				}
				id := field[string](event.Data, "admissionId")
				want := ""
				if i == 0 {
					want = "initial-admission"
				} else if i == 2 && tc.admitted {
					want = "next-admission"
				}
				if id != want {
					t.Fatalf("usage check %d admission = %q, want %q: %s", i, id, want, raw)
				}
			}
			saved, err = f.Executor.Store.Load(1)
			wantRuns := 3
			if tc.name == "missing-prior-start" {
				wantRuns = 4
			}
			// A restart of the same generation adds no run; the supplement still counts.
			if tc.generation == 0 {
				wantRuns = 2
			}
			if err := json.Unmarshal(state.View, &view); err != nil {
				t.Fatal(err)
			}
			if err != nil || saved.Runs != wantRuns || saved.ReportCompletionRuns != 1 || field[struct {
				Runs int `json:"implementationRuns"`
			}](view, "workerExecution").Runs != wantRuns || field[struct {
				Completions int `json:"reportCompletionRuns"`
			}](view, "workerExecution").Completions != 1 {
				t.Fatal("supplement must still count towards the run limit", saved, state, err)
			}
		})
	}
}

func (t *usageDecisionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/api/v1/jobs/1/events" {
		var body struct {
			Kind string `json:"kind"`
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(data, &body)
		if body.Kind == "implementation_usage_check" {
			t.checks++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"job":{"id":1},"usageLimitReason":"missing usage"}`)), Request: r}, nil
		}
		r.Body = io.NopCloser(strings.NewReader(string(data)))
	}
	var data []byte
	var err error
	if r.Body != nil {
		data, err = io.ReadAll(r.Body)
	}
	if err != nil {
		return nil, err
	}
	out, err := t.next.Explicit(r.Context(), r.Method, r.URL.RequestURI(), data)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: out.Status, Body: io.NopCloser(strings.NewReader(string(out.Body))), Request: r}, nil
}

func TestInitialDescriptorRunCountDoesNotMeanImplementationStarted(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	// The real Server includes the current generation in this value, even before a call.
	d.View["implementationRuns"] = json.RawMessage("1")
	f.Job.Raw = encode(d)
	f.CurrentView = d.View
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("first implementation blocked: %#v %v", state, err)
	}
	record, err := f.Executor.Store.Load(1)
	if err != nil || record.Runs != 1 {
		t.Fatalf("first implementation counted incorrectly: %#v %v", record, err)
	}
	if strings.Join(f.Calls, ",") != "codex,claude" {
		t.Fatal("implementation/review were not called once", f.Calls)
	}
}

func TestRecordedUnmeasuredStartIsStillHeldBeforeAnotherCall(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.HeadSHA = publishContinuationFixture(t, f)
	d.View["implementationRuns"] = json.RawMessage("1")
	f.Job.Raw = encode(d)
	f.CurrentView = d.View
	record := &journal.Record{ID: 1, Repo: d.Repo, Issue: d.Issue, Generation: 0, Epoch: 3, Stage: "implement", Branch: f.Branch, Head: d.HeadSHA, Runs: 1}
	if err := f.Executor.Store.Save(record); err != nil {
		t.Fatal(err)
	}
	transport := &usageDecisionTransport{next: f.API}
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || !strings.Contains(string(state.Result), "missing usage") {
		t.Fatalf("unmeasured start was not held: %#v %v", state, err)
	}
	if len(f.Calls) != 0 || transport.checks != 1 {
		t.Fatalf("repeated unmeasured call: calls=%v checks=%d", f.Calls, transport.checks)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != 1 {
		t.Fatalf("recorded start was lost: %#v %v", saved, err)
	}
}

func TestRemoteStartedCheckpointStillRequiresKnownUsage(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View["implementationRuns"] = json.RawMessage("1")
	d.HeadSHA = publishContinuationFixture(t, f)
	d.View["workerExecution"] = json.RawMessage(`{"stage":"implement","implementationRuns":1}`)
	f.Job.Raw = encode(d)
	f.CurrentView = d.View
	transport := &usageDecisionTransport{next: f.API}
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || !strings.Contains(string(state.Result), "missing usage") {
		t.Fatalf("remote started checkpoint was not held: %#v %v", state, err)
	}
	if len(f.Calls) != 0 || transport.checks != 1 {
		t.Fatalf("remote checkpoint repeated call: %v checks=%d", f.Calls, transport.checks)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != 1 {
		t.Fatalf("remote checkpoint lost starts: %#v %v", saved, err)
	}
}

func TestReportCompletionRemoteCountsStillEnforceRunLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 2
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		if req.ReportOnly {
			return providers.Answer{Session: req.Session, JSON: reportWith("status", "question")}, nil
		}
		answer, err := base(ctx, req)
		answer = conversationAnswer(t, req, answer)
		answer.JSON = reportWith("assumptions", []any{})
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.HoldReason != "question" || calls != 2 {
		t.Fatal(state, err, calls)
	}
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state.View, &d.View); err != nil {
		t.Fatal(err)
	}
	d.Generation = 1
	d.HeadSHA = publishContinuationFixture(t, f)
	d.View["reimplementExpected"] = encode(map[string]any{"generation": 1, "usageAdmission": true, "usageAdmissionUsed": "next-admission"})
	f.Job.Raw, f.CurrentView = encode(d), d.View
	if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
		t.Fatal(err)
	}
	checks := len(tr.checks)
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) || calls != 2 || len(tr.checks) != checks {
		t.Fatal("remote completion counts must not relax the total run limit or consume admission", state, err, calls, tr.checks)
	}
	saved, err := f.Executor.Store.Load(1)
	// Two generations and one supplement: the refused total an over-limit approval starts from.
	if err != nil || saved.Runs != 3 || saved.ReportCompletionRuns != 1 {
		t.Fatal("remote total and completion counts were not restored", saved, err)
	}
}
