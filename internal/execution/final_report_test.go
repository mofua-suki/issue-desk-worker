package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestFinalReportContract(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	result := json.RawMessage(`{"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"schema","repo":"a/b","issue":1}]},"acceptanceTransfers":[],"followupIssues":[]}`)
	for _, reason := range []string{"prerequisite_missing", "question"} {
		r := &journal.Record{ID: 1, Result: result}
		state, err := e.finish(r, "held", reason, "summary", nil)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(state.Result, &out); err != nil {
			t.Fatal(err)
		}
		if state.Status != "held" || state.HoldReason != reason {
			t.Fatal(state)
		}
		if reason == "prerequisite_missing" {
			if string(out["status"]) != `"failed"` || string(out["questions"]) != `[]` || out["incomplete"] != nil {
				t.Fatal(string(state.Result))
			}
			var missing map[string]json.RawMessage
			if json.Unmarshal(out["prerequisiteMissing"], &missing) != nil || len(missing) != 2 || missing["reason"] == nil || missing["artifacts"] == nil {
				t.Fatal(string(state.Result))
			}
		} else if out["prerequisiteMissing"] != nil {
			t.Fatal(string(state.Result))
		}
		if out["acceptanceTransfers"] != nil || out["followupIssues"] != nil {
			t.Fatal(string(state.Result))
		}
	}
}

func TestTransferReportValidation(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	valid := `{"acceptanceTransfers":[{"criterion":"条件","artifacts":["file"],"followupNumbers":[1]}],"followupIssues":[{"title":"title","purpose":"purpose","acceptance":["done"],"changePaths":[]}]}`
	cases := []struct {
		name, raw string
		valid     bool
	}{
		{"valid", valid, true},
		{"duplicate", strings.Replace(valid, `[1]`, `[1,1]`, 1), false},
		{"range", strings.Replace(valid, `[1]`, `[2]`, 1), false},
		{"kind", strings.Replace(valid, `"title":"title"`, `"kind":"other","title":"title"`, 1), false},
		{"extra transfer field", strings.Replace(valid, `"criterion"`, `"extra":1,"criterion"`, 1), false},
		{"empty acceptance", strings.Replace(valid, `["done"]`, `[]`, 1), false},
		{"long title", strings.Replace(valid, `"title":"title"`, `"title":"`+strings.Repeat("a", 201)+`"`, 1), false},
		{"size", strings.Replace(valid, `"purpose"`, `"padding":"`+strings.Repeat("x", 25000)+`","purpose"`, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, err := e.finish(&journal.Record{ID: 1, Result: json.RawMessage(tc.raw)}, "held", "question", "summary", nil)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]json.RawMessage
			_ = json.Unmarshal(state.Result, &out)
			if tc.valid {
				if out["acceptanceTransfers"] == nil || out["followupIssues"] == nil {
					t.Fatal(string(state.Result))
				}
			} else if state.Status != "held" || state.HoldReason != "review_held" || out["acceptanceTransfers"] != nil || !strings.Contains(string(out["summary"]), "移送") {
				t.Fatal(string(state.Result))
			}
		})
	}
}

func TestMergedFinalStateAndRecoverRejection(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sent intake.State
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			intake.State
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body.State
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":"invalid"}`))
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, API: api}
	r := &journal.Record{ID: 4, Epoch: 8, PR: 2, Stage: "merge", Head: strings.Repeat("a", 40), Result: json.RawMessage(`{}`)}
	state, err := e.finish(r, "merged", "", "merged", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(state.Result, &out)
	if string(out["mergeCommit"]) != `"`+r.Head+`"` {
		t.Fatal(string(state.Result))
	}
	// A refused report no longer stops the start, but it stays unreported.
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	saved, err := store.Load(4)
	if err != nil || saved.FinalReported {
		t.Fatalf("saved: %+v, %v", saved, err)
	}
	var persisted intake.State
	_ = json.Unmarshal(saved.FinalState, &persisted)
	if string(persisted.Result) != string(state.Result) {
		t.Fatal("persisted result changed")
	}
	if string(sent.Result) != string(state.Result) {
		t.Fatalf("resent result differs: %s", sent.Result)
	}
}

func TestRecoverCarriedMergedFinalReportWithPendingNotice(t *testing.T) {
	f := newPipeline(t, "approve")
	f.CurrentLeaseEpoch = f.Job.LeaseEpoch
	e := f.Executor
	ctx := context.Background()
	head := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	r := &journal.Record{
		ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch - 1,
		Branch: f.Branch, Head: head, Stage: "done",
		FinalState: encode(intake.State{Status: "merged", HeadSHA: head, Result: encode(map[string]any{
			"status": "merged", "summary": "完了", "mergeCommit": head,
		})}),
	}
	if err := e.Store.Save(r); err != nil {
		t.Fatal(err)
	}

	noticePosts := f.NoticePosts
	f.NoticeReadStatus = http.StatusServiceUnavailable
	state, err := e.Execute(ctx, f.Job, func(context.Context) error { return nil })
	var result struct {
		NotificationPending bool
	}
	if err != nil || state.Status != "merged" || json.Unmarshal(state.Result, &result) != nil || !result.NotificationPending {
		t.Fatalf("merged report with pending notice: %+v, %v", state, err)
	}
	// Simulate a restart before the final state request.
	before := len(f.ReceivedStates)
	if err := e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// The fixture checks the epoch in the HTTP request against CurrentLeaseEpoch.
	if len(f.ReceivedStates) != before+1 {
		t.Fatalf("recovered state requests: %d", len(f.ReceivedStates)-before)
	}
	sent := f.ReceivedStates[before]
	var recovered struct {
		MergeCommit string
	}
	if sent.Status != "merged" || !bytes.Equal(sent.Result, state.Result) || json.Unmarshal(sent.Result, &recovered) != nil || recovered.MergeCommit != head {
		t.Fatalf("recovered merged report: %s %s", sent.Status, sent.Result)
	}
	if f.NoticePosts != noticePosts {
		t.Fatalf("posted a notice under the new lease: %d", f.NoticePosts-noticePosts)
	}

	// The original notice becomes visible before the next delivery.
	sum := sha256.Sum256([]byte("完了"))
	marker := fmt.Sprintf("<!-- issue-desk-notice:%d:%d:%d:%s:result-merged--%x -->", r.ID, r.Generation, r.Epoch, e.Config.Host, sum[:8])
	f.NoticeReadStatus = 0
	f.Comments = append(f.Comments, map[string]any{
		"id": 1, "body": "作業結果: merged\n\n完了\n\n" + marker,
		"user": map[string]any{"login": "worker"},
	})
	for range 2 {
		f.Job.LeaseEpoch++
		f.CurrentLeaseEpoch = f.Job.LeaseEpoch
		state, err := e.Execute(ctx, f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "merged" {
			t.Fatalf("redelivered merged report at epoch %d: %+v, %v", f.Job.LeaseEpoch, state, err)
		}
		// Record a successful normal send in the same order as Runtime.
		before := len(f.ReceivedStates)
		if err := e.API.State(ctx, f.Job.ID, f.Job.LeaseEpoch, state); err != nil {
			t.Fatal(err)
		}
		if len(f.ReceivedStates) != before+1 {
			t.Fatalf("redelivered state requests: %d", len(f.ReceivedStates)-before)
		}
		sent := f.ReceivedStates[before]
		var result map[string]json.RawMessage
		if json.Unmarshal(sent.Result, &result) != nil || result["notificationPending"] != nil {
			t.Fatalf("redelivered report after notice confirmation: %s", sent.Result)
		}
		if err := e.Reported(f.Job.ID, f.Job.LeaseEpoch); err != nil {
			t.Fatal(err)
		}
		saved, err := e.Store.Load(r.ID)
		if err != nil || saved == nil || !saved.FinalReported || saved.FinalReportEpoch != f.Job.LeaseEpoch {
			t.Fatalf("report success at epoch %d was not saved: %+v, %v", f.Job.LeaseEpoch, saved, err)
		}
	}
	if f.NoticePosts != noticePosts || len(f.Comments) != 1 || !strings.Contains(f.Comments[0]["body"].(string), marker) {
		t.Fatalf("redelivery changed the notices: posts=%d, comments=%v", f.NoticePosts-noticePosts, f.Comments)
	}
	saved, err := e.Store.Load(r.ID)
	if err != nil || saved == nil || saved.Epoch != r.Epoch {
		t.Fatalf("original notification epoch %d was not retained: %+v, %v", r.Epoch, saved, err)
	}
}

func TestRecoveredMergedPullFinalReport(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	record, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	record.Result = invalidTransferFixture()
	if err := f.Executor.Store.Save(record); err != nil {
		t.Fatal(err)
	}
	f.merged = true
	f.git(f.root, "--git-dir", f.bare, "update-ref", "refs/heads/main", record.Head)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(state.Result, &result) != nil || state.Status != "merged" || string(result["mergeCommit"]) != `"`+record.Head+`"` || result["acceptanceTransfers"] != nil || result["followupIssues"] != nil || !strings.Contains(string(result["summary"]), "移送欄を省略") {
		t.Fatalf("result: %s", state.Result)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Stage != "done" || !strings.Contains(string(saved.FinalState), "mergeCommit") {
		t.Fatalf("saved: %+v %v", saved, err)
	}
}

func TestMergedInvalidCommitDoesNotBecomeHeld(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := (&Executor{Store: store}).finish(&journal.Record{ID: 1, PR: 1, Head: "invalid", Result: json.RawMessage(`{}`)}, "merged", "", "merged", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(state.Result, &result) != nil || state.Status != "merged" || state.HoldReason != "" || result["mergeCommit"] != nil || !strings.Contains(string(result["summary"]), "形式が不正") {
		t.Fatalf("state: %+v", state)
	}
}

func TestMergedFinalReportResultLimit(t *testing.T) {
	for _, mode := range []string{"new", "recover", "recover-required-overflow", "recover-archived-review-overflow"} {
		t.Run(mode, func(t *testing.T) {
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var body []byte
			transport := turnTransport(func(req *http.Request) (*http.Response, error) {
				defer req.Body.Close()
				var err error
				body, err = io.ReadAll(req.Body)
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, err
			})
			api, err := intake.New(configBootstrap("https://intake.example.invalid"), "idk_"+strings.Repeat("a", 43), transport)
			if err != nil {
				t.Fatal(err)
			}
			e := &Executor{Store: store, API: api, Config: Configuration{RuntimeDir: t.TempDir()}}
			detail := strings.Repeat("😀", 17000)
			r := &journal.Record{ID: 1, Epoch: 8, PR: 2, Stage: "merge", Head: strings.Repeat("a", 40)}
			r.Result = encode(map[string]any{"status": "completed", "detail": detail})
			if mode == "new" {
				state, err := e.finish(r, "merged", "", "完了", nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := api.State(context.Background(), r.ID, r.Epoch, state); err != nil {
					t.Fatal(err)
				}
			} else {
				result := map[string]any{"status": "merged", "summary": "完了", "mergeCommit": r.Head, "implementationResult": r.Result}
				switch mode {
				case "recover-required-overflow":
					result["controllerFileActions"] = []string{detail}
				case "recover-archived-review-overflow":
					item := followupProposalItem{Problem: detail, Expected: "期待", Test: "確認", InterruptReason: "停止理由"}
					archived, required, err := archiveRetainedReview(encode(Review{
						Decision:            "hold",
						DeferredReviewItems: []followupProposalItem{item},
						DeferredReviewDeliveries: []deferredReviewDelivery{{
							Author: "worker", Part: 1, Source: "review", Item: item,
							Proposal: journal.FollowupProposal{ID: "proposal", Status: "sent"},
						}},
					}))
					if err != nil || !required {
						t.Fatalf("could not archive the retained review: %v", err)
					}
					result["previousReview"] = archived
				}
				r.Stage = "done"
				r.FinalState = encode(intake.State{Status: "merged", Result: encode(result)})
				if err := store.Save(r); err != nil {
					t.Fatal(err)
				}
				if err := e.Recover(context.Background()); err != nil {
					t.Fatal(err)
				}
				saved, err := store.Load(r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "recover-required-overflow" || mode == "recover-archived-review-overflow" {
					if len(body) != 0 || saved.FinalReported || !bytes.Equal(saved.FinalState, r.FinalState) {
						t.Fatal("oversized required data was sent or the original record changed")
					}
					return
				}
				if !saved.FinalReported {
					t.Fatal("recovered report was not marked sent")
				}
			}
			var sent struct {
				intake.State
				Epoch int64 `json:"epoch"`
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status, Summary, MergeCommit string
				ImplementationResult         struct {
					Omitted bool
					Detail  string
				}
			}
			if err := json.Unmarshal(sent.Result, &result); err != nil {
				t.Fatal(err)
			}
			if sent.Epoch != r.Epoch || sent.Status != "merged" || result.Status != "merged" || result.Summary != "完了" || result.MergeCommit != r.Head {
				t.Fatal("merged status, epoch, summary or merge commit lost")
			}
			if units := len(utf16.Encode([]rune(string(sent.Result)))); units > 32768 || !result.ImplementationResult.Omitted || result.ImplementationResult.Detail != "" {
				t.Fatalf("optional detail was not omitted within the result limit: %d units", units)
			}
		})
	}
}

func invalidTransferFixture() json.RawMessage {
	var result map[string]any
	_ = json.Unmarshal(implementationFixture(), &result)
	result["acceptanceTransfers"] = []any{map[string]any{"criterion": "condition", "artifacts": []string{"file"}, "followupNumbers": []int{2}}}
	result["followupIssues"] = []any{map[string]any{"title": "title", "purpose": "purpose", "acceptance": []string{"done"}}}
	return encode(result)
}

func TestRepairedInvalidTransferStopsBeforePublication(t *testing.T) {
	f := newPipeline(t, "approve")
	antigravityUsageAPI(t, f, true)
	original := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, request)
		if !request.Review {
			calls++
			if calls == 1 {
				if err := os.WriteFile(filepath.Join(request.Policy.Tree, "feature.txt"), []byte("broken\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				answer.JSON = invalidTransferFixture()
			}
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || calls != 2 || state.Status != "held" || state.HoldReason != "review_held" || f.CreatePosts != 0 || f.MergePosts != 0 || !strings.Contains(string(state.Result), "移送") {
		t.Fatalf("state=%+v calls=%d create=%d merge=%d err=%v", state, calls, f.CreatePosts, f.MergePosts, err)
	}
}

// 受付へ送る最終結果では、AIの報告・レビュー・要約にある資格情報を伏せる。
func TestFinishHidesCredentialsInResult(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	key := "sk-" + strings.Repeat("k", 30)
	r := &journal.Record{ID: 1, Result: json.RawMessage(`{"status":"completed","summary":"used ` + key + `","acceptanceTransfers":[],"followupIssues":[]}`), Review: json.RawMessage(`{"summary":"saw ` + key + `"}`)}
	state, err := e.finish(r, "held", "question", "要約 "+key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state.Result), strings.Repeat("k", 30)) || !json.Valid(state.Result) {
		t.Fatalf("資格情報が残りました: %s", state.Result)
	}
}

// 伏せ字は資格情報の形だけに当たり、SHAなどは伏せない。置き換えた結果はいつもJSONとして正しい。
func TestFinishRedactionKeepsOtherValues(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	head := strings.Repeat("a", 40)
	r := &journal.Record{ID: 1, PR: 3, Head: head, Result: json.RawMessage(`{"status":"completed","summary":"x","acceptanceTransfers":[],"followupIssues":[]}`)}
	state, err := e.finish(r, "merged", "", "merged", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state.Result), head) {
		t.Fatalf("SHAを伏せました: %s", state.Result)
	}
	for _, s := range []string{`"a\"idk_` + strings.Repeat("b", 30) + `\\"`, `"sk-` + strings.Repeat("c", 30) + `"`} {
		if hidden := e.redactResult(json.RawMessage(`{"v":` + s + `}`)); !json.Valid(hidden) || strings.Contains(string(hidden), strings.Repeat("b", 30)) || strings.Contains(string(hidden), strings.Repeat("c", 30)) {
			t.Fatalf("伏せ字の後のJSON: %s", hidden)
		}
	}
}

// Exercise finish, persistence and the actual intake request, including a result
// that exceeds its own limit while the complete request remains small enough.
func TestPermissionHoldResultLimits(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		detail                   string
		hugeRequired, permission bool
	}{
		{"ordinary", "small", false, true},
		{"result-only", strings.Repeat("😀\\\"\n", 7000), false, true},
		{"required-overflow", "small", true, true},
		{"other-hold", strings.Repeat("😀\\\"\n", 7000), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var body []byte
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var err error
				body, err = io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			e := &Executor{Store: store, API: api}
			head := strings.Repeat("a", 40)
			files := []string{".claude/settings.json", "dir/😀\".json"}
			r := &journal.Record{ID: 1, Epoch: 2, PR: 17, Head: head, Stage: "publish", ExecutionPlanSHA: "plan", Test: encode(TestResult{Passed: true, Observation: map[string]any{"proof": "separate"}}), Result: encode(map[string]any{"status": "completed", "detail": tc.detail})}
			secret := "sk-" + strings.Repeat("k", 30)
			r.ControllerChanges = []json.RawMessage{encode(secret)}
			if tc.hugeRequired {
				r.ControllerChanges = []json.RawMessage{encode(strings.Repeat("x", 40000))}
			}
			if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			view := map[string]any{"releasePending": true}
			if tc.permission {
				view["permissionHold"] = map[string]any{"v": 1, "head": head, "base": "base", "pr": 17, "files": files}
			}
			state, err := e.finish(r, "held", "review_held", "権限の変更は利用者の承認を待っています", view)
			if tc.hugeRequired {
				if err == nil {
					t.Fatal("required data must not be omitted")
				}
				saved, loadErr := store.Load(1)
				if loadErr != nil || saved == nil || saved.Stage != "publish" || hasJSON(saved.FinalState) {
					t.Fatalf("journal lost or changed: %v", loadErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var persisted intake.State
			if json.Unmarshal(saved.FinalState, &persisted) != nil || !bytes.Equal(persisted.Result, state.Result) {
				t.Fatal("final state not persisted")
			}
			if err := api.State(context.Background(), r.ID, r.Epoch, state); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte(secret)) {
				t.Fatal("credential exposed in final report")
			}
			if len(body) > 128*1024 {
				t.Fatalf("body size %d", len(body))
			}
			var sent struct {
				intake.State
				Epoch int64 `json:"epoch"`
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(sent.Result, &decoded); err != nil {
				t.Fatal(err)
			}
			var canonical bytes.Buffer
			enc := json.NewEncoder(&canonical)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(decoded); err != nil {
				t.Fatal(err)
			}
			units := len(utf16.Encode([]rune(strings.TrimSuffix(canonical.String(), "\n"))))
			if units > 32768 {
				t.Fatalf("result units %d", units)
			}
			if tc.name == "other-hold" {
				// 受付と同じ数え方: エスケープと補助文字を含む本文の単位数。
				full := resultUnits(map[string]any{"detail": tc.detail})
				empty := resultUnits(map[string]any{"detail": ""})
				if full-empty != 7000*8 {
					t.Fatalf("escaped and supplementary units: got %d", full-empty)
				}
			}

			var result map[string]json.RawMessage
			_ = json.Unmarshal(sent.Result, &result)
			var impl map[string]any
			_ = json.Unmarshal(result["implementationResult"], &impl)
			// 権限承認待ちに限らず、上限を超える最終報告は実装結果を省いて収める。
			if tc.name == "result-only" || tc.name == "other-hold" {
				original := encode(map[string]any{"implementationResult": map[string]any{"status": "completed", "detail": tc.detail}})
				if len(original) >= 128*1024 {
					t.Fatal("fixture exceeds body limit")
				}
				if impl["omitted"] != true || impl["reason"] == nil {
					t.Fatal("missing omission marker")
				}
			} else if impl["detail"] != tc.detail {
				t.Fatal("unexpected omission")
			}
			// 実装結果を省いても、保留の状態・理由・要約は受付へ届く。
			var sentSummary, sentStatus string
			_ = json.Unmarshal(result["summary"], &sentSummary)
			_ = json.Unmarshal(result["status"], &sentStatus)
			if sent.Status != "held" || sent.HoldReason != "review_held" || sentStatus != "held" || sentSummary != "権限の変更は利用者の承認を待っています" {
				t.Fatalf("hold lost: status=%q reason=%q result=%q summary=%q", sent.Status, sent.HoldReason, sentStatus, sentSummary)
			}
			if tc.permission {
				var sentView struct {
					PermissionHold struct {
						Head  string   `json:"head"`
						PR    int      `json:"pr"`
						Files []string `json:"files"`
					} `json:"permissionHold"`
				}
				_ = json.Unmarshal(sent.View, &sentView)
				var summary string
				_ = json.Unmarshal(result["summary"], &summary)
				if summary != "権限の変更は利用者の承認を待っています" {
					t.Fatal("hold reason lost")
				}
				if sentView.PermissionHold.Head != head || sentView.PermissionHold.PR != 17 || !reflect.DeepEqual(sentView.PermissionHold.Files, files) || sent.PR == nil || *sent.PR != 17 || sent.HeadSHA != head || sent.HoldReason != "review_held" || len(sent.ControllerTest) == 0 {
					t.Fatalf("hold or evidence lost: %+v", sent)
				}
			}
		})
	}
	for _, limit := range []int{32768, 32769} {
		t.Run(fmt.Sprintf("boundary-%d", limit), func(t *testing.T) {
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			e := &Executor{Store: store}
			prefix := "😀\\\"\n<>&"
			r := &journal.Record{ID: 1, Epoch: 1, Result: encode(map[string]any{"detail": prefix, "number": json.Number("1e9")})}
			baseline, err := e.finish(r, "held", "review_held", "hold", nil)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(baseline.Result, &decoded); err != nil {
				t.Fatal(err)
			}
			var canonical bytes.Buffer
			enc := json.NewEncoder(&canonical)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(decoded)
			baseUnits := len(utf16.Encode([]rune(strings.TrimSuffix(canonical.String(), "\n"))))
			r.Result = encode(map[string]any{"detail": prefix + strings.Repeat("x", limit-baseUnits), "number": json.Number("1e9")})
			state, err := e.finish(r, "held", "review_held", "hold", map[string]any{"permissionHold": map[string]any{"files": []string{"protected"}}})
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]json.RawMessage
			_ = json.Unmarshal(state.Result, &out)
			var impl map[string]any
			_ = json.Unmarshal(out["implementationResult"], &impl)
			if (impl["omitted"] == true) != (limit > 32768) {
				t.Fatalf("boundary %d: omission=%v", limit, impl["omitted"])
			}
		})
	}

}
