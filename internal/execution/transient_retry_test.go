package execution

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
	workerruntime "issue-desk-worker/internal/runtime"
)

func capacityCLI(t *testing.T, req providers.Request) providers.Request {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capacity")
	// Write the script from a child shell, never through a writable descriptor of this
	// process: a parallel test forking while that descriptor is open makes the exec of
	// the script fail with ETXTBSY (golang/go#22315).
	script := "#!/bin/sh\necho 'Selected model is at capacity' >&2\nexit 1\n"
	if out, err := exec.Command("/bin/sh", "-c", `printf '%s' "$1" > "$2" && chmod 700 "$2"`, "sh", script, path).CombinedOutput(); err != nil {
		t.Fatalf("write capacity CLI: %v: %s", err, out)
	}
	req.Codex, req.Claude = path, path
	return req
}

func TestTransientCapacityPipelineEntrypoints(t *testing.T) {
	for _, stage := range []string{"initial", "repair", "review", "report"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "approve")
			f.Executor.transientWait = func(context.Context, time.Duration) error { return nil }
			completionAPI(t, f)
			base := f.Executor.Call
			attempted, implementations := 0, 0
			var first providers.Request
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if !req.Review && !req.ReportOnly {
					implementations++
				}
				target := stage == "review" && req.Review || stage == "report" && req.ReportOnly || stage == "initial" && !req.Review && !req.ReportOnly || stage == "repair" && !req.Review && !req.ReportOnly && implementations >= 2
				if target {
					attempted++
					if attempted == 1 {
						first = req
						return providers.Call(ctx, capacityCLI(t, req))
					}
					if req.Settings != first.Settings || req.Policy.Tree != first.Policy.Tree || (stage == "report" && (req.Session != first.Session || !req.ReportOnly || !req.Policy.ReadOnly)) {
						t.Fatal("retry changed request identity")
					}
				}
				if req.ReportOnly {
					return providers.Answer{Session: req.Session, JSON: implementationFixture(), Outcome: "completed"}, nil
				}
				answer, err := base(ctx, req)
				if !req.Review {
					answer = conversationAnswer(t, req, answer)
					if stage == "repair" && implementations == 1 {
						_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
					}
					if stage == "report" {
						answer.JSON = reportWith("assumptions", []any{})
					}
				}
				return answer, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || attempted != 2 {
				t.Fatalf("state=%+v err=%v attempts=%d", state, err, attempted)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved.UsageSent != len(saved.Usage) {
				t.Fatalf("usage not sent: %+v %v", saved, err)
			}
			found := 0
			for _, row := range saved.Usage {
				if strings.Contains(string(row), `"outcome":"failed"`) {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("missing failed usage row: %s", saved.Usage)
			}
		})
	}
}

func TestReportCompletionTransientExhaustion(t *testing.T) {
	t.Parallel()
	e, r, d, _ := completionFixture(t)
	var delays []time.Duration
	e.transientWait = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	calls := 0
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		return providers.Call(ctx, capacityCLI(t, req))
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state == nil || state.Status != "failed" || calls != 3 || !strings.Contains(string(state.Result), "Selected model is at capacity") || !strings.Contains(string(state.Result), "3回") {
		t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
	}
	if len(r.Usage) != 3 || r.UsageSent != 3 {
		t.Fatalf("usage rows=%d sent=%d", len(r.Usage), r.UsageSent)
	}
	if len(delays) != 2 || delays[0] != 5*time.Second || delays[1] != 10*time.Second {
		t.Fatalf("retry delays=%v", delays)
	}
}

func TestTransientRetryStopsOnCancellationAndUsageFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "usage", "send", "save", "quota", "timeout", "auth"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			e, r, d, tr := completionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				if mode == "cancel" {
					time.AfterFunc(50*time.Millisecond, cancel)
				}
				if mode == "usage" {
					tr.fail = true
				}
				if mode == "send" {
					tr.refuseUsage = true
				}
				if mode == "save" {
					blocked := filepath.Join(r.Tree, "not-a-directory")
					if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
					e.Store.Root = blocked
				}
				a := providers.Answer{Transient: true, Session: req.Session}
				switch mode {
				case "quota":
					return a, &providers.UsageLimit{Provider: req.Settings.Provider}
				case "timeout":
					return a, context.DeadlineExceeded
				case "auth":
					a.Transient = false
					return a, errors.New("authentication failed")
				default:
					return a, errors.New("capacity")
				}
			}
			req, err := e.reportCompletionRequest(ctx, d, r, errors.New("missing"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.callReportCompletion(ctx, d, r, req)
			if err == nil || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestReportCompletionTransientUsageRowsAreSentSeparately(t *testing.T) {
	t.Parallel()
	e, r, d, tr := completionFixture(t)
	e.transientWait = func(context.Context, time.Duration) error { return nil }
	calls := 0
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		input := int64(calls * 11)
		a := providers.Answer{Session: req.Session, Usage: providers.Usage{Input: &input, Output: &input}}
		if calls == 1 {
			a.Transient = true
			a.Outcome = "failed"
			return a, errors.New("capacity")
		}
		a.Outcome = "completed"
		a.JSON = implementationFixture()
		return a, nil
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state != nil || calls != 2 || len(tr.usageRequests) != 2 || r.UsageSent != 2 {
		t.Fatalf("state=%+v err=%v calls=%d sends=%d", state, err, calls, len(tr.usageRequests))
	}
	if !strings.Contains(string(tr.usageRequests[0]), `"inputTokens":11`) || !strings.Contains(string(tr.usageRequests[1]), `"inputTokens":22`) {
		t.Fatalf("usage requests: %s", tr.usageRequests)
	}
	saved, err := e.Store.Load(r.ID)
	if err != nil || len(saved.Usage) != 2 || !strings.Contains(string(saved.Usage[0]), `"inputTokens":11`) || !strings.Contains(string(saved.Usage[1]), `"inputTokens":22`) {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

func TestReviewTransientExhaustionFailsJob(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	f.Executor.transientWait = func(context.Context, time.Duration) error { return nil }
	completionAPI(t, f)
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if req.Review {
			calls++
			return providers.Call(ctx, capacityCLI(t, req))
		}
		return base(ctx, req)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || calls != 3 || !strings.Contains(string(state.Result), "Selected model is at capacity") || !strings.Contains(string(state.Result), "3回") {
		t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
	}
}

type retryLeaseLossAPI struct {
	*errorResumeAPI
	callReturned <-chan struct{}
}

func (a *retryLeaseLossAPI) Renew(ctx context.Context, id, epoch int64) (intake.Assignment, error) {
	select {
	case <-a.callReturned:
		return intake.Assignment{}, &intake.APIError{Status: 409, Code: "lease_lost"}
	default:
		return a.assignment, nil
	}
}

func TestTransientWaitStopsWhenLeaseIsLost(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	returned := make(chan struct{})
	var calls atomic.Int32
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if calls.Add(1) == 1 {
			close(returned)
		}
		return providers.Answer{Transient: true, Outcome: "failed"}, errors.New("capacity")
	}
	base := &errorResumeAPI{Client: f.API, assignment: intake.Assignment{Job: &f.Job, Lease: &intake.Lease{Epoch: f.Job.LeaseEpoch, ExpiresAt: time.Now().Add(time.Hour)}}}
	runner := workerruntime.Runner{API: &retryLeaseLossAPI{errorResumeAPI: base, callReturned: returned}, Executor: f.Executor, Capacity: 2, RenewInterval: 100 * time.Millisecond}
	err := runner.Run(context.Background())
	var refusal *intake.APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden || calls.Load() != 1 || f.merged || f.pull {
		t.Fatalf("err=%v calls=%d merged=%v pull=%v", err, calls.Load(), f.merged, f.pull)
	}
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil || saved.FinalReported || hasJSON(saved.FinalState) {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}

func TestReportCompletionTransientReportOnlyErrorExhaustion(t *testing.T) {
	t.Parallel()
	e, r, d, _ := completionFixture(t)
	e.transientWait = func(context.Context, time.Duration) error { return nil }
	calls := 0
	e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{Session: req.Session, Transient: true, Outcome: "failed"}, &providers.ReportOnlyError{Err: errors.New("temporary structured failure")}
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state == nil || state.Status != "failed" || calls != 3 || !strings.Contains(string(state.Result), "3回") {
		t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
	}
}

func TestTransientRetryRechecksClaudeKeyAfterWait(t *testing.T) {
	for _, entry := range []string{"implementation", "review", "completion"} {
		t.Run(entry, func(t *testing.T) {
			f, d, r, key := claudeKeyCallFixture(t)
			e := f.Executor
			completionAPI(t, f)
			setClaudeCallHold(t, e, true)
			if entry == "review" {
				r.Provider = "codex"
			}
			e.transientWait = func(ctx context.Context, delay time.Duration) error {
				setClaudeCallHold(t, e, false)
				return ctx.Err()
			}
			calls := 0
			e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				if calls == 1 {
					if req.APIKey != key {
						t.Fatal("first call did not use available key")
					}
					return providers.Answer{Session: req.Session, Transient: true}, errors.New("capacity")
				}
				if req.APIKey != "" {
					t.Fatal("retry used key after subscription hold expired")
				}
				return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
			}
			var err error
			if entry == "completion" {
				if err = e.receiveImplementationReport(r, implementationFixture(), "implement_report"); err != nil {
					t.Fatal(err)
				}
				var req providers.Request
				req, err = e.reportCompletionRequest(context.Background(), d, r, errors.New("missing"))
				if err == nil {
					_, err = e.callReportCompletion(context.Background(), d, r, req)
				}
			} else {
				_, err = e.callAIOnce(context.Background(), d, r, "fixture", entry == "review", nil)
			}
			if err != nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
