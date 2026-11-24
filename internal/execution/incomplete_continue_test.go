package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

type incompleteLeaseTransport struct{ next http.RoundTripper }

type incompleteMissingDecisionTransport struct{ next http.RoundTripper }

type incompleteHighUsageTransport struct{ next http.RoundTripper }

func (tr incompleteHighUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/events") {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"job":{"id":1,"view":{"implementationWeightedUsage":6400000}},"usageLimitReason":null}`)), Request: req}, nil
	}
	return tr.next.RoundTrip(req)
}

func (tr incompleteMissingDecisionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/events") {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"job":{"id":1}}`)), Request: req}, nil
	}
	return tr.next.RoundTrip(req)
}

func (tr incompleteLeaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/state") && req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		if strings.Contains(string(data), `"stage":"implement_report"`) {
			return &http.Response{StatusCode: 409, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"lease_lost"}}`)), Request: req}, nil
		}
	}
	return tr.next.RoundTrip(req)
}

func incompleteFixture() json.RawMessage {
	var v map[string]any
	_ = json.Unmarshal(implementationFixture(), &v)
	v["status"] = "failed"
	v["summary"] = strings.Repeat("長", 2100)
	v["incomplete"] = map[string]any{"reason": "未完了", "completed": []string{"調査"}, "remaining": []string{"実装"}}
	return encode(v)
}

func TestContinuationAndRepairSurviveUsageFailure(t *testing.T) {
	for _, kind := range []string{"continuation", "repair"} {
		for _, status := range []string{"completed", "question"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				f := newPipeline(t, "approve")
				f.AllowUsage = true
				tr := completionAPI(t, f)
				base := f.Executor.Call
				calls := 0
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					a, err := base(ctx, req)
					if req.Review {
						return a, err
					}
					calls++
					one := int64(1)
					a.Usage.Input, a.Usage.Output = &one, &one
					if calls == 1 {
						if kind == "continuation" {
							a.JSON = incompleteFixture()
						} else {
							_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
						}
					} else if calls == 2 {
						if status == "question" {
							a.JSON = reportWith("status", "question")
							var report map[string]any
							_ = json.Unmarshal(a.JSON, &report)
							report["questions"] = []string{"判断してください"}
							a.JSON = encode(report)
						}
						tr.refuseUsage = true
					}
					return a, err
				}
				ctx := context.Background()
				fence := func(context.Context) error { return nil }
				_, err := f.Executor.Execute(ctx, f.Job, fence)
				if err == nil || calls != 2 {
					t.Fatalf("first err=%v calls=%d", err, calls)
				}
				saved, err := f.Executor.Store.Load(1)
				wantStage := "implement_report"
				if kind == "repair" {
					wantStage = "test_report"
				}
				var report Implementation
				if err != nil || saved == nil || json.Unmarshal(saved.Result, &report) != nil || report.Status != status || saved.Stage != wantStage || saved.UsageSent == len(saved.Usage) {
					t.Fatalf("saved=%+v report=%+v err=%v", saved, report, err)
				}
				tr.refuseUsage = false
				f.Job.LeaseEpoch++
				f.CurrentLeaseEpoch = f.Job.LeaseEpoch
				state, err := f.Executor.Execute(ctx, f.Job, fence)
				want := "merged"
				if status == "question" {
					want = "held"
				}
				if err != nil || state.Status != want || calls != 2 || (status == "question" && !strings.Contains(string(state.Result), "判断してください")) {
					t.Fatalf("resume state=%+v err=%v calls=%d", state, err, calls)
				}
			})
		}
	}
}

func TestIncompleteContinuationPipeline(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		repair, again, question bool
	}{
		{name: "completed"}, {name: "still incomplete", again: true}, {name: "question", question: true}, {name: "repair", repair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.Executor.Config.Assignments.ImplementationRunLimit = 1
			tr := completionAPI(t, f)
			base := f.Executor.Call
			calls := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review {
					return a, err
				}
				calls++
				z := int64(1)
				a.Usage.Input = &z
				a.Usage.Output = &z
				if tc.repair && calls == 1 {
					_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
					return a, err
				}
				initial := 1
				if tc.repair {
					initial = 2
				}
				if calls == initial {
					a.JSON = incompleteFixture()
				} else if calls == initial+1 {
					saved, loadErr := f.Executor.Store.Load(1)
					if loadErr != nil || saved.IncompleteRun == nil || !saved.IncompleteRun.Attempted || saved.Runs != 1 {
						t.Fatalf("slot not saved: %+v %v", saved, loadErr)
					}
					if len(tr.checks) == 0 || saved.UsageSent != len(saved.Usage) {
						t.Fatal("usage was not checked and sent before call")
					}
					if tc.again {
						a.JSON = incompleteFixture()
					}
					if tc.question {
						var v map[string]any
						_ = json.Unmarshal(incompleteFixture(), &v)
						v["status"] = "question"
						v["questions"] = []string{"判断してください"}
						a.JSON = encode(v)
					}
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if tc.repair {
				want = 3
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
			saved, _ := f.Executor.Store.Load(1)
			if saved.Runs != 1 || saved.ReportCompletionRuns != 0 || saved.UnansweredRuns != 0 || saved.UsageSent != len(saved.Usage) {
				t.Fatalf("counts/usage: %+v", saved)
			}
			if tc.again || tc.question {
				if state.Status != "held" || !strings.Contains(string(state.Result), "続行を1回求めました") {
					t.Fatalf("hold: %+v", state)
				}
				if tc.question && !strings.Contains(string(state.Result), "判断してください") {
					t.Fatal("lost question")
				}
			} else if state.Status != "merged" || !f.merged {
				t.Fatalf("did not pass test and review: %+v", state)
			}
		})
	}
}

func TestIncompleteContinuationBoundariesAndReload(t *testing.T) {
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &journal.Record{ID: 1, Epoch: 3, Generation: 0, Runs: 1, Result: incompleteFixture(), Stage: "implement_report", IncompleteRun: &journal.IncompleteRun{StartedAt: now}}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Generation: 0}}
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		usage   float64
		mutate  func(*journal.Record)
	}{
		{name: "half", elapsed: 15 * time.Second}, {name: "over half", elapsed: 16 * time.Second},
		{name: "usage equal", usage: 6400000}, {name: "usage over", usage: 6400001},
		{name: "question", mutate: func(r *journal.Record) {
			var v map[string]any
			_ = json.Unmarshal(r.Result, &v)
			v["questions"] = []string{"質問"}
			r.Result = encode(v)
		}},
		{name: "prerequisite", mutate: func(r *journal.Record) {
			var v map[string]any
			_ = json.Unmarshal(r.Result, &v)
			v["prerequisiteMissing"] = map[string]any{"reason": "不足"}
			r.Result = encode(v)
		}},
		{name: "empty remaining", mutate: func(r *journal.Record) {
			var v map[string]any
			_ = json.Unmarshal(r.Result, &v)
			v["incomplete"] = map[string]any{"remaining": []string{"  "}}
			r.Result = encode(v)
		}},
		{name: "old journal", mutate: func(r *journal.Record) { r.IncompleteRun = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *r
			copy.WeightedUsage = tc.usage
			if tc.mutate != nil {
				tc.mutate(&copy)
			}
			called, err := f.Executor.continueIncomplete(context.Background(), d, &copy, "", func() time.Time { return now.Add(tc.elapsed) })
			if err != nil || called {
				t.Fatalf("unexpected continuation: %v %v", called, err)
			}
		})
	}
	// Just below the reservation boundary reaches the Server, whose refusal stops it.
	r.WeightedUsage = 6399999
	tr.reason = "limit"
	called, err := f.Executor.continueIncomplete(context.Background(), d, r, "", func() time.Time { return now.Add(time.Second) })
	if err != nil || called || len(tr.checks) == 0 {
		t.Fatalf("server refusal: %v %v", called, err)
	}
	tr.reason = ""
	tr.fail = true
	called, err = f.Executor.continueIncomplete(context.Background(), d, r, "", func() time.Time { return now.Add(time.Second) })
	if err != nil || called {
		t.Fatal("unknown usage permitted", err)
	}
	tr.fail = false
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), incompleteMissingDecisionTransport{next: tr})
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	called, err = f.Executor.continueIncomplete(context.Background(), d, r, "", func() time.Time { return now.Add(time.Second) })
	if err != nil || called {
		t.Fatal("missing server decision permitted", err)
	}
	api, err = intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), incompleteHighUsageTransport{next: tr})
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	called, err = f.Executor.continueIncomplete(context.Background(), d, r, "", func() time.Time { return now.Add(time.Second) })
	if err != nil || called || r.WeightedUsage != 6400000 {
		t.Fatal("server usage was not rechecked", err)
	}
	// An already dispatched slot stays consumed after reloading and through repair.
	r.IncompleteRun.Attempted = true
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Stage = "test_report"
	before := len(tr.checks)
	called, err = f.Executor.continueIncomplete(context.Background(), d, loaded, "", func() time.Time { return now.Add(time.Second) })
	if err != nil || called || len(tr.checks) != before {
		t.Fatal("reloaded repair revived slot", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.Executor.continueIncomplete(ctx, d, loaded, "", time.Now)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestIncompleteContinuationInterruptedAndRepairShareSlot(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair", true: "interrupted"}[interrupted], func(t *testing.T) {
			f := newPipeline(t, "approve")
			completionAPI(t, f)
			base := f.Executor.Call
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f.Executor.Call = func(callCtx context.Context, req providers.Request) (providers.Answer, error) {
				if req.Review {
					return base(callCtx, req)
				}
				calls++
				if calls == 2 && interrupted {
					cancel()
					return providers.Answer{}, context.Canceled
				}
				a, err := base(callCtx, req)
				z := int64(1)
				a.Usage.Input = &z
				a.Usage.Output = &z
				if calls == 1 || calls == 3 {
					a.JSON = incompleteFixture()
				}
				if calls == 2 && !interrupted {
					_ = os.Remove(filepath.Join(req.Policy.Tree, "feature.txt"))
				}
				return a, err
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if interrupted {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				saved, loadErr := f.Executor.Store.Load(1)
				if loadErr != nil || saved.IncompleteRun == nil || !saved.IncompleteRun.Attempted {
					t.Fatal("slot not saved", loadErr)
				}
				state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			}
			if err != nil || state.Status != "held" || !strings.Contains(string(state.Result), "続行を1回求めました") {
				t.Fatalf("result: %+v %v", state, err)
			}
			want := 3
			if interrupted {
				want = 2
			}
			if calls != want {
				t.Fatalf("unexpected repeated continuation: %d", calls)
			}
			saved, _ := f.Executor.Store.Load(1)
			if saved.Runs != 1 || !saved.IncompleteRun.Attempted {
				t.Fatal("lost saved slot or count")
			}
		})
	}
}

func TestIncompleteQuestionDoesNotContinue(t *testing.T) {
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := base(ctx, req)
		if !req.Review {
			calls++
			var v map[string]any
			_ = json.Unmarshal(incompleteFixture(), &v)
			v["status"] = "question"
			v["questions"] = []string{"判断が必要"}
			a.JSON = encode(v)
			z := int64(1)
			a.Usage.Input = &z
			a.Usage.Output = &z
		}
		return a, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || calls != 1 || len(tr.checks) != 0 || !strings.Contains(string(state.Result), "判断が必要") {
		t.Fatalf("unexpected result: %+v %v calls=%d", state, err, calls)
	}
}

func TestIncompleteLeaseLossBeforeContinuation(t *testing.T) {
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), incompleteLeaseTransport{next: tr})
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := base(ctx, req)
		if !req.Review {
			calls++
			a.JSON = incompleteFixture()
			z := int64(1)
			a.Usage.Input = &z
			a.Usage.Output = &z
		}
		return a, err
	}
	_, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err == nil || calls != 1 {
		t.Fatalf("lease failure did not prevent continuation: calls=%d err=%v", calls, err)
	}
	saved, _ := f.Executor.Store.Load(1)
	if saved == nil || saved.IncompleteRun == nil || !saved.IncompleteRun.Attempted || f.merged {
		t.Fatal("lease failure lost the slot or published")
	}
}
