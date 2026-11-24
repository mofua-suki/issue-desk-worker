package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// Forward real fixture requests while supplying the Server usage-decision contract.
type completionTransport struct {
	disabled       bool
	failCheckpoint bool
	failConfig     bool
	invalidConfig  bool
	next           *intake.Client
	reason         string
	fail           bool
	checks         []json.RawMessage
	checkpoints    []json.RawMessage
	usageRequests  []json.RawMessage
	refuseUsage    bool
	usageResponse  string
}

func (tr *completionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/api/v1/client-config" {
		if tr.failConfig {
			return nil, errors.New("config temporarily unavailable")
		}
		if tr.invalidConfig {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"settings":{}}`)), Request: r}, nil
		}
	}
	var data []byte
	if r.Body != nil {
		data, _ = io.ReadAll(r.Body)
	}
	if r.URL.Path == "/api/v1/usage" {
		tr.usageRequests = append(tr.usageRequests, append(json.RawMessage(nil), data...))
		if tr.refuseUsage {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
		}
		if tr.usageResponse != "" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tr.usageResponse)), Request: r}, nil
		}
	}
	if strings.HasSuffix(r.URL.Path, "/state") {
		tr.checkpoints = append(tr.checkpoints, append(json.RawMessage(nil), data...))
		if tr.failCheckpoint && strings.Contains(string(data), `"implementationRuns":2`) {
			return nil, errors.New("checkpoint unavailable")
		}
	}
	if strings.HasSuffix(r.URL.Path, "/events") {
		tr.checks = append(tr.checks, data)
		if tr.fail {
			return nil, errors.New("usage decision unavailable")
		}
		var reason any
		if tr.reason != "" {
			reason = tr.reason
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(encode(map[string]any{"job": map[string]any{"id": 1}, "usageLimitReason": reason})))), Request: r}, nil
	}
	out, err := tr.next.Explicit(r.Context(), r.Method, r.URL.RequestURI(), data)
	if err != nil {
		return nil, err
	}
	if tr.disabled && r.URL.Path == "/api/v1/client-config" {
		var snapshot intake.ConfigSnapshot
		_ = json.Unmarshal(out.Body, &snapshot)
		snapshot.Settings = encode(map[string]any{"providerDisable.codex": map[string]any{"until": "2099-01-01T00:00:00Z"}})
		out.Body = encode(snapshot)
	}
	return &http.Response{StatusCode: out.Status, Body: io.NopCloser(strings.NewReader(string(out.Body))), Request: r}, nil
}
func completionAPI(t *testing.T, f *pipelineFixture) *completionTransport {
	t.Helper()
	tr := &completionTransport{next: f.API}
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), tr)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	return tr
}
func reportWith(key string, value any) json.RawMessage {
	var result map[string]any
	_ = json.Unmarshal(implementationFixture(), &result)
	result[key] = value
	return encode(result)
}
func conversationAnswer(t *testing.T, req providers.Request, answer providers.Answer) providers.Answer {
	t.Helper()
	answer.Session = "implementation-session"
	if req.OnSession != nil {
		if err := req.OnSession(answer.Session); err != nil {
			t.Fatal(err)
		}
	}
	return answer
}

func TestImplementationReportUsageRetry(t *testing.T) {
	for _, scenario := range []string{"retry", "cancel", "failure", "failed-answer", "supplement", "rejected", "malformed", "generation", "other-pc"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPipeline(t, "approve")
			tr := completionAPI(t, f)
			tr.refuseUsage = true
			marker := filepath.Join(f.root, "fixed-test-ran")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "touch '"+marker+"'")
			base := f.Executor.Call
			calls, reviews, supplements := 0, 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.ReportOnly {
					supplements++
					saved, err := f.Executor.Store.Load(1)
					if err != nil || saved.UsageSent != len(saved.Usage) {
						t.Fatalf("supplement before usage receipt: %#v %v", saved, err)
					}
					return providers.Answer{JSON: implementationFixture(), Session: req.Session}, nil
				}
				if req.Review {
					reviews++
					return base(ctx, req)
				}
				calls++
				if scenario == "other-pc" && calls == 2 {
					saved, err := f.Executor.Store.Load(1)
					if err != nil || saved.ReportCompletion != nil || len(saved.Usage) != 0 {
						t.Fatalf("other PC reused local report or usage: %#v %v", saved, err)
					}
				}
				if scenario == "generation" && calls == 2 {
					saved, err := f.Executor.Store.Load(1)
					if err != nil || saved.Generation != 1 || saved.ReportCompletion != nil || saved.UsageSent != len(saved.Usage) {
						t.Fatalf("old generation report reused or usage pending: %#v %v", saved, err)
					}
				}
				a, err := base(ctx, req)
				if scenario == "supplement" {
					a = conversationAnswer(t, req, a)
					a.JSON = reportWith("assumptions", []any{})
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "failure" {
					return a, errors.New("implementation failed")
				}
				if scenario == "failed-answer" {
					a.JSON = reportWith("status", "failed")
				}
				return a, err
			}
			_, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err == nil || calls != 1 || reviews != 0 || f.MergePosts != 0 {
				t.Fatalf("first execution: %v calls=%d reviews=%d", err, calls, reviews)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("fixed test ran before usage receipt: %v", err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancel" || scenario == "failure" || scenario == "failed-answer" {
				if saved.Stage == "implement_report" || saved.ReportCompletion != nil {
					t.Fatalf("unsuccessful call saved as report: %#v", saved)
				}
				return
			}
			if saved.Stage != "implement_report" || saved.ReportCompletion == nil || saved.UsageSent >= len(saved.Usage) {
				t.Fatalf("report or pending usage missing: %#v", saved)
			}
			tr.refuseUsage = false
			if scenario == "other-pc" {
				var checkpoint intake.State
				if len(tr.checkpoints) == 0 || json.Unmarshal(tr.checkpoints[len(tr.checkpoints)-1], &checkpoint) != nil {
					t.Fatal("missing actual execution checkpoint")
				}
				var view map[string]json.RawMessage
				if json.Unmarshal(checkpoint.View, &view) != nil || len(view["workerExecution"]) == 0 {
					t.Fatal("missing worker execution in checkpoint")
				}
				d := continuationDescriptor(t, f, checkpoint.HeadSHA)
				d.View["workerExecution"] = view["workerExecution"]
				d.View["continuation"] = encode(map[string]any{"host": "fixture", "to": "other-pc", "pending": false})
				f.Job.Raw = encode(d)
				root := t.TempDir()
				store, err := journal.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
				f.Executor.Store = store
				f.Executor.Config.RuntimeDir = root
				f.Executor.Config.Host = "other-pc"
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || calls != 2 {
					t.Fatalf("other PC must not reuse the local report: status=%s error=%v calls=%d", state.Status, err, calls)
				}
				return
			}
			if scenario == "generation" {
				d := continuationDescriptor(t, f, f.git(saved.Tree, "rev-parse", "HEAD"))
				d.Generation++
				f.Job.Raw = encode(d)
			}
			if scenario == "rejected" || scenario == "malformed" {
				tr.usageResponse = `{"added":0,"duplicate":0,"rejected":1}`
				if scenario == "malformed" {
					tr.usageResponse = `{"added":1}`
				}
				_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				pending, loadErr := f.Executor.Store.Load(1)
				if err == nil || loadErr != nil || pending.UsageSent != 0 || calls != 1 || reviews != 0 || supplements != 0 || f.MergePosts != 0 {
					t.Fatalf("invalid receipt advanced: %v %#v %v", err, pending, loadErr)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("fixed test ran before valid receipt: %v", err)
				}
				tr.usageResponse = ""
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if scenario == "generation" {
				if err != nil || calls != 2 || state.Status != "merged" {
					t.Fatalf("new generation reused old report: %#v %v calls=%d", state, err, calls)
				}
				return
			}
			if err != nil || calls != 1 || state.Status != "merged" {
				t.Fatalf("resume: %#v %v calls=%d", state, err, calls)
			}
			saved, err = f.Executor.Store.Load(1)
			if err != nil || saved.UsageSent != len(saved.Usage) {
				t.Fatalf("usage not received: %#v %v", saved, err)
			}
			if len(tr.usageRequests) < 2 || !reflect.DeepEqual(tr.usageRequests[0], tr.usageRequests[1]) {
				t.Fatalf("original usage row was not retried unchanged: %s", tr.usageRequests)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("fixed test not reached: %v", err)
			}
			if scenario == "supplement" && (supplements != 1 || saved.ReportCompletion == nil || !saved.ReportCompletion.Validated) {
				t.Fatalf("report was not supplemented: %d %#v", supplements, saved.ReportCompletion)
			}
		})
	}
}

func TestReportCompletionPipelineEntrypoints(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			name := map[bool]string{false: "initial", true: "fixed-repair"}[repair] + "/" + map[bool]string{false: "invalid", true: "valid"}[valid]
			t.Run(name, func(t *testing.T) {
				f := newPipeline(t, "approve")
				tr := completionAPI(t, f)
				base := f.Executor.Call
				implementations, completions := 0, 0
				extensions := []any{map[string]any{"path": "feature.txt", "reason": "機能に必要"}}
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if req.ReportOnly {
						completions++
						if req.Review || !req.Policy.ReadOnly || req.Session != "implementation-session" || req.Settings.Provider != "codex" {
							t.Fatalf("unfixed completion request: %#v", req)
						}
						saved, err := f.Executor.Store.Load(1)
						if err != nil || !saved.ReportCompletion.Attempted || saved.Runs != 2 {
							t.Fatalf("dispatch not durably counted: %#v %v", saved, err)
						}
						var original map[string]any
						if json.Unmarshal(saved.ReportCompletion.Original, &original) != nil || !reflect.DeepEqual(original["scopeExtensions"], extensions) {
							t.Fatal("original report lost extensions", original)
						}
						// Report supplementation is implementation work, including after
						// a fixed-test repair. It keeps the public running phase.
						if f.Executor.HeartbeatStatus().Publishing != 0 {
							t.Fatal("report supplementation counted as publishing")
						}
						var checkpoint struct {
							Status string `json:"status"`
							View   struct {
								Phase string `json:"phase"`
							} `json:"view"`
						}
						if err := json.Unmarshal(tr.checkpoints[len(tr.checkpoints)-1], &checkpoint); err != nil || checkpoint.Status != "running" || checkpoint.View.Phase != "running" {
							t.Fatal("report stages changed the public running contract", checkpoint, err)
						}
						if !strings.Contains(req.Prompt, "前提と根拠") || !strings.Contains(req.Prompt, "previousReport") {
							t.Fatal("missing validation context")
						}
						answer := providers.Answer{JSON: implementationFixture(), Session: req.Session, Outcome: "completed"}
						zero := int64(0)
						answer.Usage = providers.Usage{Input: &zero, Output: &zero}
						if !valid {
							answer.JSON = reportWith("selfCheck", []any{})
						}
						return answer, nil
					}
					answer, err := base(ctx, req)
					if !req.Review {
						implementations++
						answer = conversationAnswer(t, req, answer)
						if repair && implementations == 1 {
							_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
						} else {
							var original map[string]any
							_ = json.Unmarshal(reportWith("assumptions", []any{}), &original)
							original["scopeExtensions"] = extensions
							answer.JSON = encode(original)
						}
					}
					return answer, err
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				if completions != 1 || implementations != map[bool]int{false: 1, true: 2}[repair] {
					t.Fatalf("unexpected calls: %d %d", implementations, completions)
				}
				if valid {
					saved, err := f.Executor.Store.Load(1)
					if err != nil {
						t.Fatal(err)
					}
					var result map[string]any
					if json.Unmarshal(saved.Result, &result) != nil || !reflect.DeepEqual(result["scopeExtensions"], extensions) {
						t.Fatal("supplement lost extensions", result)
					}
				}
				if valid && state.Status != "merged" {
					t.Fatalf("completion did not proceed: %#v", state)
				}
				if !valid && (state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "自己点検")) {
					t.Fatalf("remaining error missing: %#v", state)
				}
				saved, _ := f.Executor.Store.Load(1)
				if saved.Runs != 2 || !saved.ReportCompletion.Attempted {
					t.Fatalf("lost run: %#v", saved)
				}
				if len(tr.checks) == 0 || !strings.Contains(string(state.View), `"implementationRuns":2`) {
					t.Fatal("missing usage/checkpoint/final run")
				}
				checkpointFound := false
				for _, raw := range tr.checkpoints {
					if strings.Contains(string(raw), `"implementationRuns":2`) {
						checkpointFound = true
					}
				}
				if !checkpointFound {
					t.Fatal("missing counted checkpoint")
				}
				if valid && (len(saved.Usage) != map[bool]int{false: 3, true: 4}[repair] || saved.UsageSent != len(saved.Usage)) {
					t.Fatal("completion usage not saved/sent", len(saved.Usage))
				}
			})
		}
	}
}

func TestReportCompletionDoesNotRetryNormalOrTransportFailures(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, status := range []string{"completed", "question", "failed", "transport", "auth", "stopped"} {
			t.Run(map[bool]string{false: "initial", true: "fixed-repair"}[repair]+"/"+status, func(t *testing.T) {
				f := newPipeline(t, "approve")
				completionAPI(t, f)
				base := f.Executor.Call
				extras, normal := 0, 0
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if req.ReportOnly {
						extras++
						t.Fatal("unexpected completion")
					}
					answer, err := base(ctx, req)
					if !req.Review {
						normal++
						if repair && normal == 1 {
							_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
							return answer, err
						}
						switch status {
						case "transport", "auth":
							return answer, errors.New(status + " failure")
						case "stopped":
							return answer, context.Canceled
						default:
							answer.JSON = reportWith("status", status)
						}
					}
					return answer, err
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || extras != 0 || normal != map[bool]int{false: 1, true: 2}[repair] {
					t.Fatal(state, err, extras, normal)
				}
				if status == "completed" && state.Status != "merged" {
					t.Fatal(state)
				}
				if (status == "question" || status == "failed") && state.HoldReason != "question" {
					t.Fatal(state)
				}
				if (status == "transport" || status == "auth" || status == "stopped") && state.Status != "failed" {
					t.Fatal(state)
				}
			})
		}
	}
}

func TestReportCompletionInterruptedDispatchReloadDoesNotRetry(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "fixed-repair"}[repair], func(t *testing.T) {
			f := newPipeline(t, "approve")
			completionAPI(t, f)
			base := f.Executor.Call
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			normal, completions := 0, 0
			f.Executor.Call = func(callCtx context.Context, req providers.Request) (providers.Answer, error) {
				if req.ReportOnly {
					completions++
					cancel()
					return providers.Answer{}, context.Canceled
				}
				a, err := base(callCtx, req)
				if !req.Review {
					normal++
					a = conversationAnswer(t, req, a)
					if repair && normal == 1 {
						_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
					} else {
						a.JSON = reportWith("assumptions", []any{})
					}
				}
				return a, err
			}
			_, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			saved, _ := f.Executor.Store.Load(1)
			id := saved.ReportCompletion.ID
			if saved.Runs != 2 || !saved.ReportCompletion.Attempted {
				t.Fatal(saved)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.HoldReason != "review_held" || completions != 1 || normal != map[bool]int{false: 1, true: 2}[repair] {
				t.Fatalf("retry after restart: %#v %v calls %d %d", state, err, normal, completions)
			}
			saved, _ = f.Executor.Store.Load(1)
			if saved.Runs != 2 || saved.ReportCompletion.ID != id {
				t.Fatal("receipt replaced")
			}
		})
	}
}

func TestReportCompletionPreservesFixedRepairFailureLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	base := f.Executor.Call
	implementations, completions := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if req.ReportOnly {
			completions++
			return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
		}
		answer, err := base(ctx, req)
		if !req.Review {
			implementations++
			answer = conversationAnswer(t, req, answer)
			_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
			answer.JSON = reportWith("assumptions", []any{})
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || !strings.Contains(string(state.Result), "固定テストが不合格") || implementations != 3 || completions != 3 {
		t.Fatal(state, err, implementations, completions)
	}
}

func TestReportCompletionReloadPreservesFixedRepairLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	base := f.Executor.Call
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	normal, completions := 0, 0
	f.Executor.Call = func(callCtx context.Context, req providers.Request) (providers.Answer, error) {
		if req.ReportOnly {
			completions++
			if completions == 1 {
				cancel()
				return providers.Answer{}, context.Canceled
			}
			return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
		}
		a, err := base(callCtx, req)
		if !req.Review {
			normal++
			a = conversationAnswer(t, req, a)
			_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
			if normal == 3 {
				a.JSON = reportWith("assumptions", []any{})
			}
		}
		return a, err
	}
	_, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.FixedTestFailures != 2 {
		t.Fatal(saved, err)
	}
	// Simulate a completed supplement saved just before the next fixed-test checkpoint.
	saved.Result = implementationFixture()
	saved.ReportCompletion.Validated = true
	if err := f.Executor.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || normal != 3 || completions != 1 {
		t.Fatal("repair budget reset on report re-entry", state, err, normal, completions)
	}
}

func TestReportCompletionFixedRepairBudgetAfterBaseChange(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"publish", "merge"} {
		for _, repaired := range []bool{true, false} {
			t.Run(stage+"/"+map[bool]string{true: "repaired", false: "exhausted"}[repaired], func(t *testing.T) {
				f := newPipeline(t, "approve")
				completionAPI(t, f)
				// This fixed test requires the feature to match the current base.
				// Advancing main therefore creates a new failing test cycle.
				f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, `test "$(cat feature.txt)" = "$(git show refs/remotes/origin/main:README.md)"`)
				base := f.Executor.Call
				normal, reviews := 0, 0
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if req.ReportOnly {
						t.Fatal("valid repair reports must not need supplementation")
					}
					answer, err := base(ctx, req)
					if !req.Review {
						normal++
						if normal == 3 || normal == 5 && repaired {
							content := f.git(req.Policy.Tree, "show", "refs/remotes/origin/main:README.md")
							if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte(content+"\n"), 0600); err != nil {
								t.Fatal(err)
							}
						}
					} else {
						reviews++
						if reviews == 2 {
							// Stop after the new base passed tests and reached review.
							answer.JSON = encode(map[string]any{"decision": "hold", "summary": "fixture review hold", "breaking": true})
						}
					}
					return answer, err
				}
				prepareMergeRecovery(t, f)
				saved, err := f.Executor.Store.Load(1)
				if err != nil || saved.FixedTestFailures != 0 || normal != 3 {
					t.Fatal("successful test cycle did not durably reset its repair budget", saved, err, normal)
				}
				oldBase := saved.Base
				saved.Stage = stage
				if err := f.Executor.Store.Save(saved); err != nil {
					t.Fatal(err)
				}
				f.CurrentPolicy = nil
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("advanced main\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "README.md")
				f.git(source, "commit", "-m", "advance main")
				f.git(source, "push", f.bare, "main")
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || normal != 5 {
					t.Fatal("new base must allow the original two repair calls", state, err, normal)
				}
				saved, err = f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				if repaired {
					var tested TestResult
					if err := json.Unmarshal(saved.Test, &tested); err != nil || !tested.Passed || saved.FixedTestFailures != 0 || saved.Base == oldBase || reviews != 2 || state.HoldReason != "review_held" {
						t.Fatal("new base did not reach review with a reset repair budget", state, saved, err)
					}
				} else if state.Status != "failed" || !strings.Contains(string(state.Result), "固定テストが不合格") || saved.FixedTestFailures != 3 || reviews != 1 {
					t.Fatal("new test cycle exceeded the original repair limit", state, saved)
				}
			})
		}
	}
}

func TestScopeExtensionsPipeline(t *testing.T) {
	t.Parallel()
	for _, supplement := range []bool{false, true} {
		for _, presence := range []string{"entry", "empty", "omitted"} {
			t.Run(fmt.Sprintf("supplement=%v/%s", supplement, presence), func(t *testing.T) {
				f := newPipeline(t, "scope-granted")
				completionAPI(t, f)
				var d Descriptor
				if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
					t.Fatal(err)
				}
				previous := []any{map[string]any{"path": "previous.txt", "reason": "前回の受入条件に必要"}}
				d.Result = encode(map[string]any{"implementationResult": reportWith("scopeExtensions", previous)})
				f.Job.Raw = encode(d)
				base := f.Executor.Call
				var expected any
				if presence == "entry" {
					expected = []any{map[string]any{"path": "feature.txt", "reason": "受入条件の機能に必要"}}
				} else if presence == "empty" {
					expected = []any{}
				}
				seenReview := false
				get := func(raw json.RawMessage) (any, bool) {
					var m map[string]any
					if err := json.Unmarshal(raw, &m); err != nil {
						t.Fatal(err)
					}
					v, ok := m["scopeExtensions"]
					return v, ok
				}
				check := func(raw json.RawMessage) {
					t.Helper()
					v, ok := get(raw)
					if ok != (presence != "omitted") || !reflect.DeepEqual(v, expected) {
						t.Fatalf("scopeExtensions: %#v %v expected %#v", v, ok, expected)
					}
				}
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if req.ReportOnly {
						saved, err := f.Executor.Store.Load(1)
						if err != nil {
							t.Fatal(err)
						}
						check(saved.Result)
						check(saved.ReportCompletion.Original)
						return providers.Answer{JSON: reportWith("scopeExtensions", []any{map[string]any{"path": "wrong", "reason": "wrong"}}), Session: req.Session}, nil
					}
					if req.Review {
						seenReview = true
						var data map[string]json.RawMessage
						part := strings.SplitN(req.Prompt, "根拠(データ): ", 2)
						if len(part) != 2 || json.Unmarshal([]byte(part[1]), &data) != nil {
							t.Fatal("review data")
						}
						check(data["implementation"])
						var prior map[string]any
						if json.Unmarshal(data["previousImplementation"], &prior) != nil || !reflect.DeepEqual(prior["scopeExtensions"], previous) {
							t.Fatalf("previous implementation lost: %#v", prior)
						}
						if presence == "entry" {
							content, err := os.ReadFile(filepath.Join(req.Policy.Tree, "feature.txt"))
							if err != nil || string(content) != "accepted feature\n" {
								t.Fatalf("reported change does not match fixture: %q %v", content, err)
							}
						}
						saved, err := f.Executor.Store.Load(1)
						if err != nil {
							t.Fatal(err)
						}
						check(saved.Result)
						check(saved.ReportCompletion.Original)
					}
					a, err := base(ctx, req)
					if !req.Review {
						parts := strings.SplitN(req.Prompt, "対象(データ): ", 2)
						var data struct {
							Scope []string `json:"scope"`
						}
						if len(parts) != 2 || json.Unmarshal([]byte(parts[1]), &data) != nil || !reflect.DeepEqual(data.Scope, []string{"README.md"}) {
							t.Fatalf("scope: %#v", data.Scope)
						}
						a = conversationAnswer(t, req, a)
						var m map[string]any
						_ = json.Unmarshal(a.JSON, &m)
						if presence != "omitted" {
							m["scopeExtensions"] = expected
						}
						if supplement {
							m["assumptions"] = []any{}
						}
						a.JSON = encode(m)
					}
					return a, err
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || state.Status != "merged" || !seenReview {
					t.Fatalf("pipeline: %#v %v review=%v", state, err, seenReview)
				}
			})
		}
	}
}

func TestScopeExtensionsClaudeStagedActualChanges(t *testing.T) {
	e, r, _ := fileExecutor(t)
	writeFixture(t, filepath.Join(r.Tree, ".claude", "update.txt"), "before")
	writeFixture(t, filepath.Join(r.Tree, ".claude", "delete.txt"), "before")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "add.txt"), "added")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "update.txt"), "updated")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", ".delete"), ".claude/delete.txt\n")
	extensions := []any{
		map[string]any{"path": ".claude/add.txt", "reason": "追加が必要"},
		map[string]any{"path": ".claude/update.txt", "reason": "更新が必要"},
		map[string]any{"path": ".claude/delete.txt", "reason": "削除が必要"},
		map[string]any{"path": ".claude/report-only.txt", "reason": "報告のみ"},
	}
	r.Result = reportWith("scopeExtensions", extensions)
	if err := e.applyRequests(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(r.Result, &report); err != nil || !reflect.DeepEqual(report["scopeExtensions"], extensions) {
		t.Fatalf("report: %#v %v", report, err)
	}
	for _, item := range []struct {
		index   int
		content string
		exists  bool
	}{{0, "added", true}, {1, "updated", true}, {2, "", false}, {3, "", false}} {
		entry := extensions[item.index].(map[string]any)
		path, reason := entry["path"].(string), entry["reason"].(string)
		if reason == "" {
			t.Fatalf("missing reason for %s", path)
		}
		content, err := os.ReadFile(filepath.Join(r.Tree, path))
		if item.exists && (err != nil || string(content) != item.content) {
			t.Fatalf("%s: %q %v", path, content, err)
		}
		if !item.exists && !os.IsNotExist(err) {
			t.Fatalf("%s unexpectedly exists: %v", path, err)
		}
	}
}
