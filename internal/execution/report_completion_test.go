package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func completionFixture(t *testing.T) (*Executor, *journal.Record, Descriptor, *completionTransport) {
	t.Helper()
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	var d Descriptor
	_ = json.Unmarshal(f.Job.Raw, &d)
	r := &journal.Record{ID: 1, Epoch: 3, Repo: d.Repo, Issue: d.Issue, Provider: "codex", Session: "implementation-session", Runs: 1, Tree: t.TempDir()}
	if err := f.Executor.receiveImplementationReport(r, reportWith("assumptions", []any{}), "implement_report"); err != nil {
		t.Fatal(err)
	}
	return f.Executor, r, d, tr
}

func TestReportCompletionBudgetAndIsolationGuards(t *testing.T) {
	for _, kind := range []string{"runs", "approved-runs", "usage", "missing", "unavailable", "stop", "session", "antigravity", "disabled", "session-change", "source-session", "source-provider"} {
		t.Run(kind, func(t *testing.T) {
			e, r, d, tr := completionFixture(t)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "runs":
				e.Config.Assignments.ImplementationRunLimit = 1
			case "approved-runs":
				e.Config.Assignments.ImplementationRunLimit = 1
				d.View["overLimitApprovals"] = encode([]any{map[string]any{"id": "approval", "statement": "1回追加", "implementationRuns": 1}})
			case "usage":
				tr.reason = "usage limit"
			case "missing":
				tr.reason = "missing usage"
			case "unavailable":
				tr.fail = true
			case "stop":
				cancel()
			case "session":
				r.Session = ""
			case "antigravity":
				r.Provider = "antigravity"
			case "disabled":
				tr.disabled = true
			case "source-session":
				r.Session = "replaced-session"
			case "source-provider":
				r.Provider = "claude"
			}
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				if !req.Policy.ReadOnly || req.Review || !req.ReportOnly {
					t.Fatal("wrong contract/policy")
				}
				if err := e.applyRequests(ctx, r); err == nil {
					t.Fatal("controller edited during completion")
				}
				session := req.Session
				if kind == "session-change" {
					session = "other"
				}
				return providers.Answer{Session: session, JSON: implementationFixture()}, nil
			}
			state, err := e.completeImplementationReport(ctx, d, r, true)
			if kind == "stop" {
				if !errors.Is(err, context.Canceled) || calls != 0 {
					t.Fatal(state, err, calls)
				}
				return
			}
			if kind == "approved-runs" {
				if err != nil || state != nil || calls != 1 || r.Runs != 2 {
					t.Fatal(state, err, calls)
				}
				return
			}
			expected := 0
			if kind == "session-change" {
				expected = 1
			}
			if err != nil || state == nil || calls != expected {
				t.Fatal(state, err, calls)
			}
			if kind == "missing" && !strings.Contains(string(state.Result), "missing usage") {
				t.Fatal(state)
			}
			if expected == 0 && r.Runs != 1 {
				t.Fatal("count changed before dispatch")
			}
		})
	}
}

func TestReportCompletionDistinctReportsAndSuccessfulReplay(t *testing.T) {
	e, r, d, _ := completionFixture(t)
	calls := 0
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
	}
	for i := 0; i < 2; i++ {
		if state, err := e.completeImplementationReport(context.Background(), d, r, true); err != nil || state != nil {
			t.Fatal(state, err)
		}
		saved, err := e.Store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		r = saved
		if state, err := e.completeImplementationReport(context.Background(), d, r, true); err != nil || state != nil {
			t.Fatal(state, err)
		}
		if calls != i+1 || r.Runs != i+2 {
			t.Fatal(calls, r.Runs)
		}
		if i == 0 {
			id := r.ReportCompletion.ID
			if err := e.receiveImplementationReport(r, reportWith("assumptions", []any{}), "test_report"); err != nil {
				t.Fatal(err)
			}
			if r.ReportCompletion.ID == id || r.ReportCompletion.Attempted {
				t.Fatal("new report inherited budget")
			}
		}
	}
}

func TestReportCompletionRetainsControllerPlanAndClaudeStageOnce(t *testing.T) {
	e, r, d, _ := completionFixture(t)
	r.Provider = "claude"
	// The dependency fixture executes only the controller sandbox command and logs adds.
	root := t.TempDir()
	log := filepath.Join(root, "adds")
	sandbox := filepath.Join(root, "sandbox")
	pnpm := filepath.Join(root, "pnpm")
	writeFixture(t, sandbox, "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 9\n")
	writeFixture(t, pnpm, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+log+"' || exit 1\n")
	_ = os.Chmod(sandbox, 0700)
	_ = os.Chmod(pnpm, 0700)
	e.Config.Codex = sandbox
	e.Config.PNPM = pnpm
	writeFixture(t, filepath.Join(r.Tree, "original-delete"), "remove")
	writeFixture(t, filepath.Join(r.Tree, "keep"), "keep")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "hooks", "original"), "original stage")
	var before map[string]any
	_ = json.Unmarshal(r.Result, &before)
	before["deleteFiles"] = []string{"original-delete"}
	before["addDependencies"] = []any{map[string]any{"name": "original-dep", "version": "1.2.3", "dev": true}}
	if err := e.receiveImplementationReport(r, encode(before), "implement_report"); err != nil {
		t.Fatal(err)
	}
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if req.Settings.Provider != "claude" || !req.Policy.ReadOnly {
			t.Fatal(req)
		}
		if err := e.applyRequests(ctx, r); err == nil {
			t.Fatal("controller action allowed in report call")
		}
		raw, _ := os.ReadFile(filepath.Join(r.Tree, ".claude-staged", "hooks", "original"))
		if string(raw) != "original stage" {
			t.Fatal("stage lost")
		}
		var after map[string]any
		_ = json.Unmarshal(implementationFixture(), &after)
		after["deleteFiles"] = []string{"keep"}
		after["addDependencies"] = []any{map[string]any{"name": "new-dep", "version": "9.9.9", "dev": false}}
		return providers.Answer{Session: req.Session, JSON: encode(after)}, nil
	}
	if state, err := e.completeImplementationReport(context.Background(), d, r, true); state != nil || err != nil {
		t.Fatal(state, err)
	}
	for i := 0; i < 2; i++ {
		if err := e.applyRequests(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		loaded, err := e.Store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		r = loaded
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "original-delete")); !os.IsNotExist(err) {
		t.Fatal("original deletion lost")
	}
	raw, err := os.ReadFile(filepath.Join(r.Tree, "keep"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("supplemental deletion applied")
	}
	raw, err = os.ReadFile(filepath.Join(r.Tree, ".claude", "hooks", "original"))
	if err != nil || string(raw) != "original stage" {
		t.Fatal("original stage lost")
	}
	raw, err = os.ReadFile(log)
	if err != nil || strings.Count(string(raw), "original-dep@1.2.3") != 1 || strings.Contains(string(raw), "new-dep") {
		t.Fatal("dependency plan replaced/replayed", string(raw), err)
	}
	if len(r.ControllerChanges) != 4 {
		t.Fatal("controller stage/deletion applied twice", len(r.ControllerChanges))
	}
}

func TestReportCompletionDropsNewRequestsWhenOriginalOmittedThem(t *testing.T) {
	after := reportWith("deleteFiles", []string{"new"})
	merged, err := retainImplementationEdits(implementationFixture(), after)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(merged, &result)
	if _, exists := result["deleteFiles"]; exists {
		t.Fatal("new delete request retained")
	}
	after = reportWith("addDependencies", []any{map[string]any{"name": "new", "version": "1", "dev": false}})
	merged, err = retainImplementationEdits(implementationFixture(), after)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(merged, &result)
	if _, exists := result["addDependencies"]; exists {
		t.Fatal("new dependency request retained")
	}
}

func TestReportCompletionAmbiguousControllerOutcomeDoesNotReplay(t *testing.T) {
	e, r, _, _ := completionFixture(t)
	r.ReportCompletion.Attempted = true
	r.ReportCompletion.Validated = true
	r.ReportCompletion.EditsStarted = true
	writeFixture(t, filepath.Join(r.Tree, "keep"), "keep")
	r.Result = reportWith("deleteFiles", []string{"keep"})
	_ = e.Store.Save(r)
	r, _ = e.Store.Load(1)
	if err := e.applyRequests(context.Background(), r); err == nil {
		t.Fatal("unknown controller outcome replayed")
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "keep")); err != nil {
		t.Fatal(err)
	}
}

func TestReportCompletionCheckpointFailureConsumesAttemptBeforeCall(t *testing.T) {
	e, r, d, tr := completionFixture(t)
	tr.failCheckpoint = true
	calls := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err == nil || state != nil || calls != 0 {
		t.Fatal("called after failed checkpoint", state, err, calls)
	}
	r, err = e.Store.Load(1)
	if err != nil || r.Runs != 2 || !r.ReportCompletion.Attempted {
		t.Fatal(r, err)
	}
	tr.failCheckpoint = false
	state, err = e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state == nil || state.HoldReason != "review_held" || calls != 0 {
		t.Fatal("attempt restored", state, err, calls)
	}
}

func TestReportCompletionUnavailableUsageIsSavedAsMissing(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "zero"}[measured], func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a := providers.Answer{Session: req.Session, JSON: implementationFixture()}
				if measured {
					zero := int64(0)
					a.Usage.Input = &zero
					a.Usage.Output = &zero
				}
				return a, nil
			}
			state, err := e.completeImplementationReport(context.Background(), d, r, true)
			if err != nil || state != nil {
				t.Fatal(state, err)
			}
			r, _ = e.Store.Load(1)
			if len(r.Usage) != 1 || r.UsageSent != 1 {
				t.Fatal("usage missing")
			}
			var usage map[string]any
			_ = json.Unmarshal(r.Usage[0], &usage)
			if measured {
				if usage["inputTokens"] != float64(0) || usage["usageMissingReason"] != nil {
					t.Fatal(usage)
				}
			} else if usage["inputTokens"] != nil || usage["usageMissingReason"] != "usage-unavailable" {
				t.Fatal(usage)
			}
		})
	}
}

func TestReportCompletionFailureKindsNeverTriggerFallbackOrAnotherCall(t *testing.T) {
	for _, kind := range []string{"transport", "quota", "structure", "question", "failed"} {
		t.Run(kind, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			calls := 0
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				answer := providers.Answer{Session: req.Session, JSON: implementationFixture(), Transient: kind == "quota", Denied: []json.RawMessage{json.RawMessage(`{"tool":"Read"}`)}}
				switch kind {
				case "transport":
					return answer, errors.New("communication failed")
				case "quota":
					return answer, &providers.UsageLimit{Provider: req.Settings.Provider}
				case "structure":
					answer.JSON = json.RawMessage(`{"status":"completed"}`)
				case "question", "failed":
					answer.JSON = reportWith("status", kind)
				}
				return answer, nil
			}
			state, err := e.completeImplementationReport(context.Background(), d, r, true)
			if err != nil || calls != 1 {
				t.Fatal(state, err, calls)
			}
			if kind == "question" || kind == "failed" {
				if state != nil {
					t.Fatal(state)
				}
				var report Implementation
				_ = json.Unmarshal(r.Result, &report)
				if report.Status != kind {
					t.Fatal(report)
				}
			} else {
				if state == nil {
					t.Fatal("no terminal state")
				}
				if kind == "structure" && state.HoldReason != "review_held" {
					t.Fatal(state)
				}
				if kind != "structure" && state.Status != "failed" {
					t.Fatal(state)
				}
			}
			if r.Runs != 2 || r.Provider != "codex" || r.Session != "implementation-session" {
				t.Fatal("provider/session/run changed", r)
			}
		})
	}
}

func TestReportCompletionAmbiguousControllerOutcomeHoldsBeforeFixedRepair(t *testing.T) {
	e, r, d, _ := completionFixture(t)
	r.Stage = "test"
	r.Result = implementationFixture()
	r.ReportCompletion.Attempted = true
	r.ReportCompletion.Validated = true
	r.ReportCompletion.EditsStarted = true
	calls := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{"never-run"}}, r, "repair")
	if err != nil || state == nil || state.HoldReason != "review_held" || calls != 0 {
		t.Fatal(state, err, calls)
	}
}

func TestReportCompletionRepairsOtherReportDeficiencies(t *testing.T) {
	for _, field := range []string{"selfCheck", "prevention", "similar", "assumptions"} {
		t.Run(field, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			var value any
			switch field {
			case "selfCheck":
				value = []any{}
			case "prevention":
				value = map[string]any{"kind": "test", "detail": "none"}
			case "similar":
				value = map[string]any{"searched": "none", "found": []any{}, "handled": "fixture", "guard": "fixture"}
			case "assumptions":
				value = []any{map[string]any{"assumption": "fixture", "evidence": "none", "verified": false}}
			}
			if err := e.receiveImplementationReport(r, reportWith(field, value), "test_report"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
			}
			state, err := e.completeImplementationReport(context.Background(), d, r, true)
			if err != nil || state != nil || calls != 1 || !r.ReportCompletion.Validated {
				t.Fatal(state, err, calls)
			}
		})
	}
}

func TestReportCompletionCannotReuseOriginalUsageAdmission(t *testing.T) {
	e, r, d, tr := completionFixture(t)
	d.View["reimplementExpected"] = encode(map[string]any{"generation": 0, "usageAdmission": true})
	r.AdmissionID = "original-admission"
	r.AdmissionRun = 1
	r.AdmissionGeneration = 0
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state != nil || len(tr.checks) != 1 {
		t.Fatal(state, err)
	}
	var check struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(tr.checks[0], &check)
	if _, exists := check.Data["admissionId"]; exists {
		t.Fatal("original admission reused")
	}
	if _, exists := check.Data["admissionRecall"]; exists {
		t.Fatal("original admission recalled")
	}
}

func TestReportCompletionConfigFailureHoldsWithoutConsumingAttempt(t *testing.T) {
	for _, unavailable := range []bool{true, false} {
		t.Run(map[bool]string{true: "unavailable", false: "invalid"}[unavailable], func(t *testing.T) {
			e, r, d, tr := completionFixture(t)
			tr.failConfig, tr.invalidConfig = unavailable, !unavailable
			calls := 0
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, errors.New("unexpected call")
			}
			state, err := e.completeImplementationReport(context.Background(), d, r, false)
			if err != nil || state == nil || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "前提と根拠") {
				t.Fatal(state, err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil || saved.Runs != 1 || saved.ReportCompletionRuns != 0 || saved.ReportCompletion.Attempted || calls != 0 || len(tr.checks) != 0 {
				t.Fatal("config failure dispatched or consumed budget", saved, err, calls, tr.checks)
			}
		})
	}
}

func TestReportCompletionReportsAllMissingFields(t *testing.T) {
	for _, repair := range []bool{true, false} {
		t.Run(fmt.Sprint(repair), func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			var report map[string]any
			if err := json.Unmarshal(implementationFixture(), &report); err != nil {
				t.Fatal(err)
			}
			assumption := report["assumptions"].([]any)[0].(map[string]any)
			assumption["assumption"], assumption["evidence"] = "", ""
			check := report["selfCheck"].([]any)[0].(map[string]any)
			check["criterion"] = ""
			check["production"].(map[string]any)["origin"] = ""
			check["similarLocations"] = []any{}
			if err := providers.Validate(encode(report), false); err != nil {
				t.Fatalf("invalid receipt schema: %v", err)
			}
			if err := providers.ValidateReport(encode(report), true); err == nil {
				t.Fatal("expected semantic validation failure")
			}
			if err := e.receiveImplementationReport(r, encode(report), "implement_report"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				for _, field := range []string{"assumptions[0].assumption", "assumptions[0].evidence", "selfCheck[0].criterion", "selfCheck[0].production.origin", "selfCheck[0].similarLocations"} {
					if !strings.Contains(req.Prompt, field) {
						t.Errorf("missing diagnostic %s", field)
					}
				}
				if req.Session != r.Session || !req.Policy.ReadOnly || !req.ReportOnly {
					t.Fatal("completion contract changed")
				}
				answer := implementationFixture()
				if !repair {
					answer = encode(report)
				}
				return providers.Answer{Session: req.Session, JSON: answer}, nil
			}
			state, err := e.completeImplementationReport(context.Background(), d, r, true)
			if err != nil || calls != 1 || (state == nil) != repair || (!repair && state.HoldReason != "review_held") {
				t.Fatalf("state=%v err=%v calls=%d", state, err, calls)
			}
		})
	}
}
