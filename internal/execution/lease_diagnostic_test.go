package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	workerruntime "issue-desk-worker/internal/runtime"
)

type lostLeaseAPI struct{ *errorResumeAPI }

func (a *lostLeaseAPI) Renew(context.Context, int64, int64) (intake.Assignment, error) {
	return intake.Assignment{}, &intake.APIError{Status: 409, Code: "lease_lost"}
}

func TestLeaseDiagnosticPreservesPendingResultAndUsageOnRefusal(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch, Generation: 0, Stage: "implement"}
	if _, err := f.Executor.finish(r, "held", "", "saved result", nil); err != nil {
		t.Fatal(err)
	}
	r.Usage = []json.RawMessage{json.RawMessage(`{"unsent":true}`)}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	before, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := &errorResumeAPI{Client: f.API, assignment: intake.Assignment{Job: &f.Job, Lease: &intake.Lease{Epoch: f.Job.LeaseEpoch, ExpiresAt: time.Now().Add(time.Hour)}}}
	a := &lostLeaseAPI{base}
	runner := workerruntime.Runner{API: a, Executor: f.Executor, Capacity: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err = runner.Run(ctx)
	var refusal *intake.APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("runner: %v", err)
	}
	after, err := f.Executor.Store.Load(r.ID)
	if err != nil || !bytes.Equal(before.FinalState, after.FinalState) || after.FinalReported || after.UsageSent != 0 || !bytes.Equal(before.Usage[0], after.Usage[0]) {
		t.Fatalf("pending data changed: %#v %v", after, err)
	}
	if f.merged || f.pull {
		t.Fatal("published after lease refusal")
	}
	if base.states != 0 {
		t.Fatalf("state sent after lease refusal: %d", base.states)
	}
	entries, err := os.ReadDir(diagnosticDir(f.Executor.Store))
	if err != nil || len(entries) != 1 {
		t.Fatalf("diagnostic not retained: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(diagnosticDir(f.Executor.Store), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var d leaseDiagnostic
	if err := json.Unmarshal(data, &d); err != nil || d.Reason != "lease_lost" || d.Epoch != f.Job.LeaseEpoch || d.Stage != "unknown" || d.Millis["merge"] != nil {
		t.Fatalf("diagnostic: %s %v", data, err)
	}
	// A newly created result includes retained diagnostics, but the pending result
	// above is never silently rewritten or acknowledged.
	state, err := f.Executor.finish(after, "held", "", "later result", nil)
	if err != nil || !strings.Contains(string(state.Result), "lease_lost") || !strings.Contains(string(state.Result), "duration_ms") {
		t.Fatalf("summary: %s %v", state.Result, err)
	}
}

func TestLeaseDiagnosticObservedStageAndUnknownResume(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	r := &journal.Record{ID: 99, Epoch: 2, Generation: 4, Stage: "test_report"}
	r.StartStageTiming(4, 2)
	started := r.Timing.StartedAt
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	job := intake.Job{ID: 99, LeaseEpoch: 2, Raw: json.RawMessage(`{"generation":4}`)}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	if err := e.RecordLeaseDiagnostic(job, "lease_expired", time.Now(), started); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(output.String())
	if strings.Count(line, "\n") != 0 || !strings.Contains(line, `"job":99`) || !strings.Contains(line, `"generation":4`) || !strings.Contains(line, `"epoch":2`) || !strings.Contains(line, `"reason":"lease_expired"`) {
		t.Fatalf("invalid one-line diagnostic: %s", line)
	}
	if err := e.RecordLeaseDiagnostic(job, "lease_lost", time.Now().Add(time.Second), started); err != nil {
		t.Fatal(err)
	}
	summary, err := e.leaseDiagnosticSummary(99, "later")
	if err != nil || !strings.Contains(summary, `2/2`) || !strings.Contains(summary, `lease_lost`) || !strings.Contains(summary, `lease_expired`) || !strings.Contains(summary, `test_report`) || !strings.Contains(summary, `"test":`) || !strings.Contains(summary, `"deploy":null`) || !strings.Contains(summary, `"incomplete":true`) {
		t.Fatal(summary, err)
	}
	if err := e.RecordLeaseDiagnostic(job, "unsafe secret", time.Now(), started); err == nil {
		t.Fatal("unsafe reason accepted")
	}
	if _, err := e.leaseDiagnosticSummary(100, "unchanged"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestLeaseDiagnosticLogMatchesRetainedFile(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	r := &journal.Record{ID: 99, Epoch: 2, Generation: 4, Stage: "implement"}
	r.StartStageTiming(4, 2)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	job := intake.Job{ID: 99, LeaseEpoch: 2, Raw: json.RawMessage(`{"generation":4}`)}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	if err := e.RecordLeaseDiagnostic(job, "lease_expired", r.Timing.StartedAt, r.Timing.StartedAt); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(output.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("expected one line: %s", line)
	}
	_, payload, found := strings.Cut(line, "lease_diagnostic ")
	if !found {
		t.Fatalf("missing diagnostic: %s", line)
	}
	entries, err := os.ReadDir(diagnosticDir(store))
	if err != nil || len(entries) != 1 {
		t.Fatalf("retained files: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(diagnosticDir(store), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var logged, retained map[string]any
	if err := json.Unmarshal([]byte(payload), &logged); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &retained); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"job": float64(99), "generation": float64(4), "epoch": float64(2),
		"at": r.Timing.StartedAt.Format(time.RFC3339Nano), "reason": "lease_expired", "stage": "implement",
		"duration_ms": map[string]any{"implementing": float64(0), "test": nil, "review": nil, "publish": nil, "merge": nil, "deploy": nil},
		"incomplete":  false,
	}
	if !reflect.DeepEqual(logged, want) || !reflect.DeepEqual(retained, want) {
		t.Fatalf("log=%v retained=%v want=%v", logged, retained, want)
	}
	// A second real diagnostic with an unobserved stage must not match the first.
	output.Reset()
	if err := e.RecordLeaseDiagnostic(job, "lease_expired", r.Timing.StartedAt.Add(-time.Second), r.Timing.StartedAt); err != nil {
		t.Fatal(err)
	}
	_, payload, found = strings.Cut(strings.TrimSpace(output.String()), "lease_diagnostic ")
	if !found {
		t.Fatal("missing second diagnostic")
	}
	var different map[string]any
	if err := json.Unmarshal([]byte(payload), &different); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(retained, different) || different["duration_ms"].(map[string]any)["implementing"] != nil {
		t.Fatalf("unobserved duration or mismatch lost: %v", different)
	}
}

func TestLeaseDiagnosticBeforeTrackingDoesNotUseOlderStage(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	r := &journal.Record{ID: 88, Epoch: 2, Generation: 4, Stage: "review"}
	r.StartStageTiming(4, 2)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	job := intake.Job{ID: 88, LeaseEpoch: 2, Raw: json.RawMessage(`{"generation":4}`)}
	if err := e.RecordLeaseDiagnostic(job, "lease_expired", r.Timing.StartedAt.Add(-time.Second), r.Timing.StartedAt); err != nil {
		t.Fatal(err)
	}
	summary, err := e.leaseDiagnosticSummary(88, "")
	if err != nil || !strings.Contains(summary, `"stage":"unknown"`) || !strings.Contains(summary, `"review":null`) {
		t.Fatalf("event before tracking used a later stage: %s %v", summary, err)
	}
}

func TestLeaseDiagnosticRetainedWhenJournalIsUnreadable(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	path := filepath.Join(store.Root, "state", "job-88.json")
	broken := []byte("{broken")
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store}
	job := intake.Job{ID: 88, LeaseEpoch: 2, Raw: json.RawMessage(`{"generation":4}`)}
	if err := e.RecordLeaseDiagnostic(job, "lease_lost", time.Now(), time.Now()); err == nil {
		t.Fatal("journal read failure must be reported")
	}
	summary, err := e.leaseDiagnosticSummary(88, "later")
	if err != nil || !strings.Contains(summary, `"reason":"lease_lost"`) || !strings.Contains(summary, `"stage":"unknown"`) || !strings.Contains(summary, `"test":null`) {
		t.Fatalf("missing retained incomplete diagnostic: %s %v", summary, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, broken) {
		t.Fatalf("journal overwritten: %s %v", after, err)
	}
}

type refusingResultAPI struct {
	*errorResumeAPI
	unknown bool
	cancel  context.CancelFunc
	calls   int
}

func (a *refusingResultAPI) State(_ context.Context, _, _ int64, _ intake.State) error {
	a.calls++
	if a.unknown {
		a.cancel()
		return &intake.TransportError{}
	}
	return &intake.APIError{Status: 403}
}

func TestDiagnosticResultRefusalAndInterruptedResponseRemainPending(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "response_unknown"}[unknown], func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := &journal.Record{ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch, Stage: "implement"}
			if err := f.Executor.RecordLeaseDiagnostic(f.Job, "lease_expired", time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Executor.finish(r, "held", "", "pending", nil); err != nil {
				t.Fatal(err)
			}
			r.Usage = []json.RawMessage{json.RawMessage(`{"unsent":true}`)}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			before, err := f.Executor.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			base := &errorResumeAPI{Client: f.API, assignment: intake.Assignment{Job: &f.Job, Lease: &intake.Lease{Epoch: f.Job.LeaseEpoch, ExpiresAt: time.Now().Add(time.Hour)}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &refusingResultAPI{errorResumeAPI: base, unknown: unknown, cancel: cancel}
			_ = (workerruntime.Runner{API: a, Executor: f.Executor, Capacity: 2}).Run(ctx)
			after, err := f.Executor.Store.Load(r.ID)
			if err != nil || a.calls != 1 || after.FinalReported || !bytes.Equal(before.FinalState, after.FinalState) || after.UsageSent != 0 || len(after.Usage) != 1 {
				t.Fatalf("result or usage acknowledged: calls=%d record=%#v err=%v", a.calls, after, err)
			}
			if !strings.Contains(string(after.FinalState), "lease_expired") {
				t.Fatal("retained result lost diagnostic")
			}
			entries, err := os.ReadDir(diagnosticDir(f.Executor.Store))
			if err != nil || len(entries) != 1 {
				t.Fatalf("diagnostic lost: %v %v", entries, err)
			}
		})
	}
}

func TestScopeReleaseDiagnosticSaveFailureStaysReleased(t *testing.T) {
	f := newPipeline(t, "scope-wait")
	var before *journal.Record
	f.ScopeAdd = func(w http.ResponseWriter, _ []string) {
		var err error
		before, err = f.Executor.Store.Load(f.Job.ID)
		if err != nil || before == nil {
			t.Errorf("load before release: %v", err)
		}
		fmt.Fprint(w, `{"result":"wait","job":{"id":1}}`)
	}
	if err := os.WriteFile(diagnosticDir(f.Executor.Store), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if !errors.Is(err, intake.ErrJobReleased) || state.Status != "held" || state.HoldReason != "scope_wait" || f.merged || f.pull || len(f.Calls) != 1 {
		t.Fatalf("scope release treated as still owned: %v", err)
	}
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil || before == nil {
		t.Fatalf("load after release: %v", err)
	}
	if !saved.FinalReported || saved.Stage != "held" || !bytes.Equal(saved.FinalState, encode(state)) || !bytes.Equal(saved.Result, before.Result) || !bytes.Equal(encode(saved.Usage), encode(before.Usage)) || saved.UsageSent != before.UsageSent || saved.Branch != before.Branch || saved.Head != before.Head || saved.Generation != before.Generation || saved.Epoch != before.Epoch {
		t.Fatalf("released record lost result, usage or resume information: before=%+v after=%+v", before, saved)
	}
	logs := output.String()
	if !strings.Contains(logs, "event=diagnostic_save_failed") || strings.Contains(logs, "event=release_result_save_failed") || strings.Contains(logs, diagnosticDir(f.Executor.Store)) {
		t.Fatalf("incorrect or unredacted failure log: %s", logs)
	}
}

func TestScopeReleaseJournalSaveFailureStops(t *testing.T) {
	f := newPipeline(t, "scope-wait")
	f.ScopeAdd = func(w http.ResponseWriter, _ []string) {
		path := filepath.Join(f.Executor.Store.Root, "state", fmt.Sprintf("job-%d.json", f.Job.ID))
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"result":"wait","job":{"id":1}}`)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if !errors.Is(err, intake.ErrJobReleased) || !strings.Contains(output.String(), "event=release_result_save_failed") || f.merged || f.pull || len(f.Calls) != 1 {
		t.Fatalf("journal failure did not stop released job: %v, %s", err, output.String())
	}
}

func TestBrokenDiagnosticDoesNotBlockMergeResume(t *testing.T) {
	for _, kind := range []string{"invalid_json", "directory_is_file"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.MergeFailStatus, f.PullFailAfterPost, f.MergeFailApplied = 503, 503, true
			runErrorResume(t, f)
			dir := diagnosticDir(f.Executor.Store)
			if kind == "invalid_json" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("job-%d-x.json", f.Job.ID)), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
				t.Fatal(err)
			}
			posts := f.MergePosts
			f.MergeFailStatus, f.PullFailAfterPost = 0, 0
			a := runErrorResume(t, f)
			saved, err := f.Executor.Store.Load(f.Job.ID)
			if err != nil || saved == nil || !saved.FinalReported || !hasJSON(saved.FinalState) || a.states != 1 || f.MergePosts != posts {
				t.Fatalf("result not delivered or merge resent: record=%#v err=%v states=%d", saved, err, a.states)
			}
		})
	}
}

func TestLeaseDiagnosticPreservesLongOriginalSummary(t *testing.T) {
	f := newPipeline(t, "approve")
	for i := 0; i < 3; i++ {
		if err := f.Executor.RecordLeaseDiagnostic(f.Job, "lease_lost", time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	original := "利用者の判断: " + strings.Repeat("理", 1880)
	r := &journal.Record{ID: f.Job.ID, Epoch: f.Job.LeaseEpoch}
	state, err := f.Executor.finish(r, "held", "review_held", original, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Summary, original) || len([]rune(result.Summary)) > 2000 {
		t.Fatalf("original summary lost: %q", result.Summary)
	}
}

func TestLeaseDiagnosticDistinguishesResumeGap(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	r := &journal.Record{ID: 1, Generation: 4, Epoch: 2, Stage: "implement"}
	r.StartStageTiming(4, 2)
	if r.Timing.Gaps {
		t.Fatal("fresh tracking marked incomplete")
	}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	job := intake.Job{ID: 1, LeaseEpoch: 2, Raw: json.RawMessage(`{"generation":4}`)}
	if err := e.RecordLeaseDiagnostic(job, "lease_lost", time.Now(), r.Timing.StartedAt); err != nil {
		t.Fatal(err)
	}
	r, err = store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	r.StartStageTiming(4, 2)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordLeaseDiagnostic(job, "lease_lost", time.Now(), r.Timing.StartedAt); err != nil {
		t.Fatal(err)
	}
	summary, err := e.leaseDiagnosticSummary(1, "")
	if err != nil || !strings.Contains(summary, `"incomplete":false`) || !strings.Contains(summary, `"incomplete":true`) {
		t.Fatalf("%s %v", summary, err)
	}
}

func TestLeaseDiagnosticGenerationAndLegacyResume(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     bool
		generation int
		gap        bool
	}{
		{"new generation from held", false, 5, false},
		{"same generation resume", false, 4, true},
		{"legacy resume", true, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			r := &journal.Record{ID: 1, Generation: 4, Epoch: 2, Stage: "implement"}
			if !tc.legacy {
				r.StartStageTiming(4, 2)
				r.Stage = "held"
			}
			if tc.legacy {
				if err := os.WriteFile(filepath.Join(store.Root, "state", "job-1.json"), []byte(`{"id":1,"generation":4,"epoch":2,"stage":"implement"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			r, err = store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			r.StartStageTiming(tc.generation, 2)
			r.Generation = tc.generation
			r.Stage = "implement"
			if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			job := intake.Job{ID: 1, LeaseEpoch: 2, Raw: json.RawMessage(fmt.Sprintf(`{"generation":%d}`, tc.generation))}
			e := &Executor{Store: store}
			if err := e.RecordLeaseDiagnostic(job, "lease_lost", time.Now(), r.Timing.StartedAt); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(diagnosticDir(store))
			if err != nil || len(entries) != 1 {
				t.Fatalf("diagnostics: %v %v", entries, err)
			}
			data, err := os.ReadFile(filepath.Join(diagnosticDir(store), entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			var d leaseDiagnostic
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatal(err)
			}
			if d.Gaps != tc.gap || d.Millis["deploy"] != nil {
				t.Fatalf("diagnostic: %s", data)
			}
		})
	}
}

func TestLeaseDiagnosticRetrySavedBeforeTracking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{{"same execution", false}, {"restart before tracking", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: d.ID, Repo: d.Repo, Issue: d.Issue, Generation: d.Generation, Stage: "implement"}
			if err := f.Executor.resetRetryBase(context.Background(), d, r); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.restart {
				if err = f.Executor.Store.Close(); err != nil {
					t.Fatal(err)
				}
				f.Executor.Store, err = journal.Open(f.Executor.Config.RuntimeDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Executor.Store.Close() })
				r, err = f.Executor.Store.Load(d.ID)
			} else {
				r, err = f.Executor.Store.LoadAfterSave(r)
			}
			if err != nil {
				t.Fatal(err)
			}
			r.StartStageTiming(d.Generation, f.Job.LeaseEpoch)
			if r.Timing.Gaps != tc.restart {
				t.Fatalf("gap = %v", r.Timing.Gaps)
			}
			if err = f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			if err = f.Executor.RecordLeaseDiagnostic(f.Job, "lease_lost", time.Now(), r.Timing.StartedAt); err != nil {
				t.Fatal(err)
			}
			summary, err := f.Executor.leaseDiagnosticSummary(d.ID, "")
			if err != nil || !strings.Contains(summary, fmt.Sprintf(`"incomplete":%t`, tc.restart)) || !strings.Contains(summary, `"deploy":null`) {
				t.Fatalf("diagnostic: %s %v", summary, err)
			}
		})
	}
}

func TestFullTestDirtyTreeDoesNotAttributeObservationToCommit(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	script := "const {test} = require('node:test'); const fs = require('node:fs'); test('change', () => fs.writeFileSync('uncommitted', 'change'));\n"
	if err := os.WriteFile(filepath.Join(source, "dirty.test.js"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "dirty.test.js")
	f.git(source, "commit", "-m", "test fixture")
	f.git(source, "push", f.bare, "main")
	head := f.git(source, "rev-parse", "HEAD")
	f.Executor.Config.Repositories[0].TestCommand = []string{f.Executor.Config.Node, "--test", "dirty.test.js"}
	result, err := f.Executor.fullTestRequest(context.Background(), monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)})
	var observation map[string]any
	if err != nil || json.Unmarshal(encode(result["causeObservation"]), &observation) != nil || result["status"] != "failed" || result["sha"] != head || observation["sha"] != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
}
