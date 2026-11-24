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

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func refuseOnlyProposalPosts(t *testing.T, f *pipelineFixture) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var body struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(raw, &body)
			if strings.Contains(body.Body, "<!-- issue-desk-proposal ") {
				w.WriteHeader(403)
				return
			}
		}
		f.server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.Gitea = client
}

func proposalBodies(t *testing.T, f *pipelineFixture) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	for _, comment := range f.Comments {
		body, _ := comment["body"].(string)
		if !strings.Contains(body, "<!-- issue-desk-proposal ") {
			continue
		}
		if _, err := parseProposalComment(body); err != nil {
			t.Fatalf("invalid wire proposal: %v\n%s", err, body)
		}
		var p map[string]json.RawMessage
		if err := json.Unmarshal([]byte(proposalMarker.FindStringSubmatch(body)[1]), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func useMinorReview(f *pipelineFixture) {
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := base(ctx, req)
		if req.Review {
			a.JSON = minorReview4553()
		}
		return a, err
	}
}

func TestFollowupPipelinePostsBeforeMergeAndEveryReleaseHold(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"approve", "manual", "permission", "sql-delete", "post-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, mode)
			useMinorReview(f)
			if mode == "manual" {
				f.Executor.Config.Repositories[0].AutoMerge = false
			}
			if mode == "post-failed" {
				refuseOnlyProposalPosts(t, f)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			want := "held"
			if mode == "approve" || mode == "post-failed" {
				want = "merged"
			}
			if err != nil || state.Status != want {
				t.Fatalf("%s: %+v %v", mode, state, err)
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil || len(r.FollowupProposals) != 2 {
				t.Fatal(r, err)
			}
			var result struct {
				FollowupProposals []journal.FollowupProposal `json:"followupProposals"`
			}
			if err := json.Unmarshal(state.Result, &result); err != nil || !reflect.DeepEqual(result.FollowupProposals, r.FollowupProposals) {
				t.Fatal("result lost delivery state", err)
			}
			if mode == "post-failed" {
				for _, proposal := range r.FollowupProposals {
					if proposal.Status != "failed" || proposal.Error == "" {
						t.Fatal(proposal)
					}
				}
				if f.MergePosts != 1 {
					t.Fatal("proposal failure stopped merge")
				}
				return
			}
			bodies := proposalBodies(t, f)
			if len(bodies) != 2 {
				t.Fatal("expected 5+1 items", bodies)
			}
			var all []followupProposalItem
			for _, body := range bodies {
				if string(body["source"]) != `"owner/project#7"` || string(body["pr"]) != fmt.Sprintf("%q", f.server.URL+"/owner/project/pulls/1") || len(body) != 10 {
					t.Fatal(body)
				}
				var items []followupProposalItem
				_ = json.Unmarshal(body["items"], &items)
				all = append(all, items...)
			}
			var original Review
			_ = json.Unmarshal(minorReview4553(), &original)
			if !reflect.DeepEqual(all, reviewProposalItems(original)) {
				t.Fatal("proposal details differ", all)
			}
			if want == "held" && f.MergePosts != 0 {
				t.Fatal("release hold changed")
			}
			// The manual request merger only receives a PR, not Review. Delivery
			// must already exist before that independent entry point is used.
			if mode == "manual" {
				before := f.NoticePosts
				progress := &requestProgress{Stage: "claimed", Head: r.Head, Branch: r.Branch, Base: "main", Request: monitorRequest{ID: "123-aabbccdd", Repo: r.Repo, Payload: encode(map[string]any{"number": r.PR, "head": r.Head, "approval": "PR 1 をマージしてください"})}}
				outcome, err := f.Executor.mergeRequestedPull(context.Background(), progress)
				if err != nil || outcome["status"] != "merged" || f.NoticePosts != before {
					t.Fatal(outcome, err)
				}
			}
		})
	}
}

const fixtureEvidence = "確認済み根拠: abc123 handler.go:42 で入力を処理する\n"
const fixtureVerification = "go test ./internal/handler; 再現入力: 空文字; 期待結果: 拒否"

func fixtureProposal(problem string) followupProposalItem {
	return followupProposalItem{Problem: problem, Expected: fixtureEvidence + "問題を修正する", Test: fixtureVerification}
}

func proposalRecord() *journal.Record {
	return &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, PR: 1, Head: strings.Repeat("a", 40), Epoch: 3, Provider: "codex", Review: encode(Review{Decision: "approve", Improvements: []string{"改善"}, ImprovementSpecs: []followupProposalItem{fixtureProposal("改善")}})}
}

func TestFollowupOutboxSurvivesEpochLeaseAndRestartWithoutDuplicate(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := proposalRecord()
			f.DropNoticeResponse, f.HideNotices = unknown, unknown
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if f.NoticePosts != 1 {
				t.Fatal(f.NoticePosts)
			}
			if unknown && (r.FollowupProposals[0].Status != "failed" || r.FollowupProposals[0].Error == "") {
				t.Fatal(r.FollowupProposals)
			}
			var err error
			r, err = f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			r.Epoch = 4
			f.CurrentLeaseEpoch = 4
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 {
				t.Fatal("reposted across epoch", err)
			}
			f.NoticeReadStatus = 503
			if err := f.Executor.recoverNotifications(context.Background()); err != nil {
				t.Fatal("proposal failure blocked recovery", err)
			}
			f.NoticeReadStatus, f.HideNotices = 0, false
			if err := f.Executor.recoverNotifications(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || r.FollowupProposals[0].Status != "sent" || f.NoticePosts != 1 {
				t.Fatal(r.FollowupProposals, err)
			}
			// Simulate a different PC having no local outbox: reconcile the same
			// stable marker from the original issue's comments before POST.
			n, _ := f.Executor.followupProposalNotice(r, 0, reviewProposalItems(Review{Improvements: []string{"改善"}}))
			if err := os.Remove(f.Executor.notificationFile(n)); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 {
				t.Fatal("remote marker was ignored", err)
			}
		})
	}
}

func TestFollowupPreparedNoticeRequiresCurrentLease(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	f.CurrentLeaseEpoch = 4
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 0 || r.FollowupProposals[0].Status != "failed" {
		t.Fatal(r, err)
	}
	r.Epoch = 4
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 || r.FollowupProposals[0].Status != "sent" {
		t.Fatal(r, err)
	}
}

func TestFollowupNoDiffApprovalRecordsMissingPRWithoutBlockingCompletion(t *testing.T) {
	f := newPipeline(t, "approve")
	base := f.Executor.Call
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "true")
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := base(ctx, req)
		if req.Review {
			a.JSON = minorReview4553()
		} else if err := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); err != nil {
			t.Fatal(err)
		}
		return a, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || f.CreatePosts != 0 || len(proposalBodies(t, f)) != 0 {
		t.Fatal(state, err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil || len(r.FollowupProposals) != 2 || !strings.Contains(r.FollowupProposals[0].Error, "元PRがありません") || !strings.Contains(string(state.Result), "followupProposals") {
		t.Fatal(r, err)
	}
}

func TestFollowupLongUnicodeAndHTMLEscapeRemainValidWithoutLoss(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	problem := strings.Repeat("😀<&>", 700)
	want := followupProposalItem{Problem: problem, Expected: fixtureEvidence + strings.Repeat("期", 4500), Test: fixtureVerification + strings.Repeat("試", 2200)}
	r.Review = encode(Review{Decision: "approve", Improvements: []string{problem}, ImprovementSpecs: []followupProposalItem{want}})
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var got followupProposalItem
	for _, body := range proposalBodies(t, f) {
		var items []followupProposalItem
		_ = json.Unmarshal(body["items"], &items)
		for _, item := range items {
			got.Problem += item.Problem
			got.Expected += item.Expected
			got.Test += item.Test
		}
	}
	if got != want {
		t.Fatal("long review was truncated")
	}
}

func TestFollowupRefusalAndMissingMetadataRemainRecoverable(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	f.NoticePostStatus = 403
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || r.FollowupProposals[0].Status != "failed" {
		t.Fatal(r, err)
	}
	f.NoticePostStatus = 0
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || r.FollowupProposals[0].Status != "sent" || f.NoticePosts != 2 {
		t.Fatal(r, err)
	}
	items := reviewProposalItems(Review{Decision: "approve", Improvements: []string{"旧レビューの改善"}})
	if len(items) != 1 || !strings.Contains(items[0].Expected, "旧レビューの改善") || !strings.Contains(items[0].Test, "旧レビューの改善") {
		t.Fatal(items)
	}
}

func TestFollowupHeldLegacyReviewResumesWithoutAnotherReviewOrPost(t *testing.T) {
	f := newPipeline(t, "approve")
	useMinorReview(f)
	f.Executor.Config.Repositories[0].AutoMerge = false
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatal(state, err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	// Before this change, this raw review would have been held, retaining its
	// findingSpecs. Resume must use Approved and preserve the same proposal IDs.
	r.Review = minorReview4553()
	// A held result is resumed only after the previous result was acknowledged.
	r.FinalReported = true
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Repositories[0].AutoMerge = true
	calls := len(f.Calls)
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || len(f.Calls) != calls || len(proposalBodies(t, f)) != 2 {
		t.Fatal(state, err)
	}
}

func TestFollowupSecretsAreRecordedWithoutPosting(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.secret = "fixture-secret"
	r := proposalRecord()
	r.Review = encode(Review{Decision: "approve", Improvements: []string{"fixture-secretを確認"}, ImprovementSpecs: []followupProposalItem{fixtureProposal("fixture-secretを確認")}})
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 0 || r.FollowupProposals[0].Status != "failed" || strings.Contains(r.FollowupProposals[0].Error, f.Executor.secret) {
		t.Fatal(r.FollowupProposals, err)
	}
}

func TestRecordOnlyNormalizedReviewPreservesNotesAndFiltersProposals(t *testing.T) {
	f := newPipeline(t, "approve")
	long := strings.Repeat("長い項目 ", 700)
	review := Review{Decision: "approve", Improvements: []string{"改善\t項目", "残す改善", long}, Findings: []string{"指摘\n項目", "残す指摘"}, Kinds: []string{"other", "other"}, Severities: []string{"minor", "minor"}, ResolvableUncertainties: []string{"不確実  項目", "残す不確実"}, RecordOnly: []string{"改善 項目", "指摘 項目", "不確実 項目", long}}
	for _, problem := range append(append(append([]string{}, review.Improvements...), review.Findings...), review.ResolvableUncertainties...) {
		item := fixtureProposal(problem)
		review.FindingSpecs = append(review.FindingSpecs, reviewFindingSpec{Problem: problem, Expected: item.Expected, Test: item.Test})
	}
	_, normalized := normalizeReview(encode(review))
	r := proposalRecord()
	r.Review = normalized
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got Review
	if err := json.Unmarshal(saved.Review, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RecordOnly, review.RecordOnly) || !reflect.DeepEqual(got.Improvements, []string{"改善\t項目", "残す改善", long, "指摘\n項目", "残す指摘", "不確実  項目", "残す不確実"}) {
		t.Fatal(got)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	bodies := proposalBodies(t, f)
	if len(bodies) != 1 {
		t.Fatal(bodies)
	}
	var items []followupProposalItem
	if err := json.Unmarshal(bodies[0]["items"], &items); err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, item := range items {
		problems = append(problems, item.Problem)
	}
	if !reflect.DeepEqual(problems, []string{"残す改善", "残す指摘", "残す不確実"}) {
		t.Fatal(problems)
	}
	if err := f.Executor.sendReviewNotes(context.Background(), saved, func(context.Context) error { return nil }, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"改善\t項目", long, "指摘\n項目", "不確実  項目"} {
		found := false
		for _, note := range f.Notes {
			data, _ := note["data"].(map[string]any)
			if note["kind"] == "improvement" && data["text"] == clip(want, 2000) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing improvement note %q", want[:min(len(want), 30)])
		}
	}
}

func TestAllRecordOnlyDoesNotPostOrChangeMerge(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			f := newPipeline(t, "approve")
			base := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review {
					var review map[string]any
					_ = json.Unmarshal(a.JSON, &review)
					review["improvements"] = []string{"記録だけ"}
					item := fixtureProposal("記録だけ")
					review["findingSpecs"] = []reviewFindingSpec{{Problem: item.Problem, Expected: item.Expected, Test: item.Test}}
					if all {
						review["recordOnly"] = []string{"記録だけ"}
					}
					a.JSON = encode(review)
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || f.MergePosts != 1 {
				t.Fatal(state, err)
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if all {
				if len(proposalBodies(t, f)) != 0 || len(r.FollowupProposals) != 0 {
					t.Fatal(f.NoticePosts, r.FollowupProposals)
				}
			} else if len(proposalBodies(t, f)) != 1 || len(r.FollowupProposals) != 1 {
				t.Fatal(r.FollowupProposals)
			}
		})
	}
}

// These reviews are synthetic provider-shaped inputs, not the saved job3506 review.
func TestMinorRecordOnlyBatchesPreserveMerge(t *testing.T) {
	t.Parallel()
	findings := []string{"指摘1", "指摘2", "指摘3", "指摘4", "指摘5", "指摘6"}
	var specs []reviewFindingSpec
	var kinds, severities []string
	for _, problem := range findings {
		kinds = append(kinds, "other")
		severities = append(severities, "minor")
		specs = append(specs, reviewFindingSpec{Group: "other", Problem: problem, Expected: fixtureEvidence + "期待 " + problem, Test: fixtureVerification + " 検査 " + problem})
	}
	for _, tc := range []struct {
		name      string
		excluded  []string
		wantPosts int
	}{
		{"none", []string{}, 2}, {"one", findings[:1], 1}, {"all", findings, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(minorReview4553(), &raw); err != nil {
				t.Fatal(err)
			}
			raw["findings"], raw["findingKinds"], raw["findingSeverities"], raw["findingSpecs"] = findings, kinds, severities, specs
			raw["improvements"], raw["resolvableUncertainties"], raw["recordOnly"] = []string{}, []string{}, tc.excluded
			data := encode(raw)
			if err := providers.Validate(data, true); err != nil {
				t.Fatal(err)
			}
			f := newPipeline(t, "approve")
			base := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review {
					a.JSON = data
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || f.MergePosts != 1 {
				t.Fatal(state, err, f.MergePosts)
			}
			bodies := proposalBodies(t, f)
			if len(bodies) != tc.wantPosts {
				t.Fatalf("posts: %d", len(bodies))
			}
			var got []followupProposalItem
			for _, body := range bodies {
				var items []followupProposalItem
				if err := json.Unmarshal(body["items"], &items); err != nil {
					t.Fatal(err)
				}
				got = append(got, items...)
			}
			var want []followupProposalItem
			for _, spec := range specs[len(tc.excluded):] {
				want = append(want, followupProposalItem{Problem: spec.Problem, Expected: spec.Expected, Test: spec.Test})
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("items: %#v, want %#v", got, want)
			}
		})
	}
}

func TestRecordOnlyEmptyKeysDoNotExcludeEligibleProblem(t *testing.T) {
	var baseline []followupProposalItem
	for _, tc := range []struct {
		name     string
		excluded []string
	}{{"empty array", []string{}}, {"blank keys", []string{"", " \t\n"}}} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(minorReview4553(), &raw); err != nil {
				t.Fatal(err)
			}
			raw["findings"], raw["findingKinds"], raw["findingSeverities"], raw["findingSpecs"] = []string{}, []string{}, []string{}, []any{}
			raw["improvements"], raw["resolvableUncertainties"], raw["recordOnly"] = []string{"通常項目", "", " \t"}, []string{}, tc.excluded
			item := fixtureProposal("通常項目")
			raw["findingSpecs"] = []reviewFindingSpec{{Problem: item.Problem, Expected: item.Expected, Test: item.Test}}
			data := encode(raw)
			if err := providers.Validate(data, true); err != nil {
				t.Fatal(err)
			}
			f := newPipeline(t, "approve")
			base := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review {
					a.JSON = data
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || f.MergePosts != 1 {
				t.Fatal(state, err, f.MergePosts)
			}
			bodies := proposalBodies(t, f)
			if len(bodies) != 1 {
				t.Fatal(bodies)
			}
			var got []followupProposalItem
			if err := json.Unmarshal(bodies[0]["items"], &got); err != nil {
				t.Fatal(err)
			}
			if tc.name == "empty array" {
				baseline = got
			}
			if !reflect.DeepEqual(got, baseline) || len(got) != 1 || got[0].Problem != "通常項目" {
				t.Fatal(got, baseline)
			}
		})
	}
}

// job3506 result.review, saved by intake for issue #183 / PR #213, was
// presented in conversation 19348d9c-1356-43b2-837b-9149b23cc6b3,
// message 07284e83-9566-4599-8d45-1628cf83a2ef (2026-11-06T16:04:58.318Z).
// This is the saved object, not CLI stdout. The controller reports that only
// JSON serialization changed and supplied compact SHA256 8d4d08d86c59eac99b7a2abcae1c6b9b9634d3999af43b25798b44626145c911.
const savedJob3506Review = `{"breaking":false,"decision":"approve","findingKinds":[],"findingSeverities":[],"findingSpecs":[],"findings":[],"followupSpecs":[],"followups":[],"improvements":["findingSpecs.problem の一致だけで同じ指摘と判定するため、レビュアーが「テスト不足」のような汎用的で短い problem を別々の問題に使い回すと、別の重大指摘が「修正後も残る指摘」と誤って判定され、保留になるおそれがあります。problem が一定の長さに満たない場合は照合キーにしない、group も合わせて比べる、などの抑制を検討してください。","照合キーは、本文同士・problem 同士に加えて、本文と problem の間でも一致させています（review_decision.go:89-100 の全組合せ比較）。意図した仕様であれば、コメントにそう明記するとよいです。"],"postMergeChecks":["実装工程でコミットが増えず head が変わらない場合、同じ head での再レビューでは重大指摘の再照合を行わないため、実装とレビューの往復は実装回数・使用量の上限でしか止まりません。配備後に、該当するジョブの往復回数と使用量の推移を確認してください。"],"recordOnly":["照合キーは、本文同士・problem 同士に加えて、本文と problem の間でも一致させています（review_decision.go:89-100 の全組合せ比較）。意図した仕様であれば、コメントにそう明記するとよいです。"],"resolvableUncertainties":[],"summary":"受入条件3項目は実コードで満たされています。①重大指摘の照合は、本文と findingSpecs.problem のどちらかが空白正規化後に一致すれば同じ指摘とします。言い換えで本文が変わっても problem が同じなら検出でき、TestRepeatedHeavyFindingStructuredProblem で確認されています。②照合は種類によらず行い、その旨と見逃しの限界（言い換えで本文も problem も変わる場合は、実装回数・使用量の上限が停止条件）を README に書いています。③execute.go:491-494 で ` + "`" + `r.ReviewHead != \"\" && r.ReviewHead != r.Head` + "`" + ` のときだけ再照合します。このため、UserJudgments で保留になったあと同じ head で再開した場合は実装へ差し戻されます。TestSameHeadResumeAfterUserJudgmentReturnsToImplementation がこの経路を通り、修正前の実装なら保留になるため回帰検出として成立しています。ReviewHead を更新するのは非承認時の execute.go:506-508 だけで、retry_base.go:46,52 は Review と ReviewHead を同時に消すため、比較対象の取り違えは見当たりません。宣言範囲外の README の変更は、受入条件2の記載のためと報告にあり、妥当です。宣言範囲にある antigravity_switch_test.go は変更されていませんが、問題ありません。コントローラーの固定テストでは go test ./... がすべて成功しています。公開API・設定・DB形式の変更や、セキュリティ・データ損失の懸念はありません。","tooLarge":false,"userJudgments":[]}`

func TestSavedJob3506RecordOnlyFiltersProposal(t *testing.T) {
	data := []byte(savedJob3506Review)
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != "8d4d08d86c59eac99b7a2abcae1c6b9b9634d3999af43b25798b44626145c911" {
		t.Fatalf("fixture SHA256: %s", got)
	}
	if err := providers.Validate(data, true); err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(data, &original); err != nil {
		t.Fatal(err)
	}
	var problems, excluded []string
	if err := json.Unmarshal(original["improvements"], &problems); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(original["recordOnly"], &excluded); err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 || len(excluded) != 1 || excluded[0] != problems[1] || !strings.HasPrefix(problems[0], "findingSpecs.problem の一致だけで同じ指摘と判定するため、") || !strings.HasPrefix(problems[1], "照合キーは、本文同士・problem 同士に加えて、本文と problem の間でも一致させています") {
		t.Fatal(problems, excluded)
	}
	// Keep the saved object intact and add candidate details to both derived
	// reviews, so the recordOnly comparison exercises eligible proposals.
	derived := make(map[string]json.RawMessage, len(original))
	for k, v := range original {
		derived[k] = v
	}
	var specs []reviewFindingSpec
	for _, problem := range problems {
		item := fixtureProposal(problem)
		specs = append(specs, reviewFindingSpec{Problem: problem, Expected: item.Expected, Test: item.Test})
	}
	derived["findingSpecs"] = encode(specs)
	qualified, err := json.Marshal(derived)
	if err != nil {
		t.Fatal(err)
	}
	derived["recordOnly"] = json.RawMessage(`[]`)
	counterfactual, err := json.Marshal(derived)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want []string
	}{
		{"counterfactual", counterfactual, problems}, {"saved", qualified, problems[:1]}, {"unqualified saved", data, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := providers.Validate(tc.data, true); err != nil {
				t.Fatal(err)
			}
			f := newPipeline(t, "approve")
			base := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review {
					a.JSON = tc.data
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || f.MergePosts != 1 {
				t.Fatal(state, err, f.MergePosts)
			}
			bodies := proposalBodies(t, f)
			if tc.want == nil {
				if len(bodies) != 0 {
					t.Fatalf("unqualified proposal posted: %v", bodies)
				}
				saved, err := f.Executor.Store.Load(1)
				if err != nil || !strings.Contains(string(saved.Review), problems[0]) || !strings.Contains(string(saved.Review), problems[1]) {
					t.Fatalf("saved findings lost: %v %v", saved, err)
				}
				return
			}
			if len(bodies) != 1 {
				t.Fatalf("proposal posts: %d", len(bodies))
			}
			var items []followupProposalItem
			if err := json.Unmarshal(bodies[0]["items"], &items); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, item := range items {
				got = append(got, item.Problem)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems: %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLegacyExternalCheckAndActionableReviewDelivery(t *testing.T) {
	const check = "外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う"
	const fix = "エラー処理を修正する"
	const mixed = "配備先で確認し、エラー処理を修正する"
	for _, problems := range [][]string{{check}, {check, fix, mixed}} {
		f := newPipeline(t, "approve")
		r := proposalRecord()
		review := Review{Decision: "approve", Improvements: problems}
		for _, problem := range problems {
			if problem != check {
				item := fixtureProposal(problem)
				review.FindingSpecs = append(review.FindingSpecs, reviewFindingSpec{Problem: item.Problem, Expected: item.Expected, Test: item.Test})
			}
		}
		_, r.Review = normalizeReview(encode(review))
		if err := f.Executor.Store.Save(r); err != nil {
			t.Fatal(err)
		}
		r, err := f.Executor.Store.Load(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		var saved Review
		if err := json.Unmarshal(r.Review, &saved); err != nil || !reflect.DeepEqual(saved.Improvements, problems) {
			t.Fatalf("review lost: %+v %v", saved, err)
		}
		bodies := proposalBodies(t, f)
		if len(problems) == 1 {
			if len(bodies) != 0 {
				t.Fatalf("unexpected proposal: %v", bodies)
			}
			continue
		}
		var items []followupProposalItem
		if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || len(items) != 2 || items[0].Problem != fix || items[1].Problem != mixed {
			t.Fatalf("wrong proposals: %v", bodies)
		}
	}
}

func TestDeferredExternalCheckSurvivesResumeWithoutProposal(t *testing.T) {
	const check = "外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う"
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.ReviewHolds = 1
	r.Review = nil
	r.PreviousReview = secondReviewResponse("other", "先行する重大な指摘")
	r.PreviousReview = changeSecondReview(r.PreviousReview, "deferredReviewItems", []followupProposalItem{{Problem: check}})
	current := encode(map[string]any{"decision": "hold"})
	_, r.Review, _ = decideReview(r, current)
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	rememberReview(resumed)
	resumed.Review = mergeRetainedReview(encode(Review{Decision: "approve"}), resumed.PreviousReview)
	if err := f.Executor.postFollowupProposals(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	var review Review
	if err := json.Unmarshal(resumed.Review, &review); err != nil || len(review.DeferredReviewItems) != 1 || review.DeferredReviewItems[0].Problem != check {
		t.Fatalf("lost deferred check: %+v %v", review, err)
	}
	if bodies := proposalBodies(t, f); len(bodies) != 0 {
		t.Fatalf("unexpected proposal: %v", bodies)
	}
}

func TestLegacyDeliveryRetainedBeforeExternalCheckFiltering(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.Review = encode(Review{Decision: "approve", Improvements: []string{"外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う", "実装を修正する"}})
	state := journal.FollowupProposal{ID: followupProposalID(r, 0), Generation: r.Generation, Head: r.Head, Status: "sent"}
	r.FollowupProposals = []journal.FollowupProposal{state}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saved Review
	if err := json.Unmarshal(resumed.Review, &saved); err != nil || len(saved.DeferredReviewDeliveries) != 2 || saved.DeferredReviewDeliveries[0].Proposal.ID != state.ID {
		t.Fatalf("lost delivery history: %+v %v", saved, err)
	}
	if bodies := proposalBodies(t, f); len(bodies) != 0 {
		t.Fatalf("reposted: %v", bodies)
	}
}

func TestFollowupPriorityPostingAndResume(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := proposalRecord()
			problems := []string{"通常", "優先1", "空白", "優先2", "長すぎ", "境界"}
			specs := make([]followupProposalItem, len(problems))
			reasons := []string{"", "  急ぐ理由　", "　 ", "急ぐ理由", strings.Repeat("界", 501), strings.Repeat("界", 500)}
			for i := range problems {
				specs[i] = followupProposalItem{Problem: problems[i], Expected: fixtureEvidence + "期待", Test: fixtureVerification, InterruptReason: reasons[i]}
			}
			r.Review = encode(Review{Decision: "approve", Improvements: problems, ImprovementSpecs: specs})
			f.DropNoticeResponse, f.HideNotices = unknown, unknown
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			bodies := proposalBodies(t, f)
			if len(bodies) != 3 {
				t.Fatalf("batches: %d", len(bodies))
			}
			got := map[string][]string{}
			for _, body := range bodies {
				var priority struct {
					Level  string `json:"level"`
					Reason string `json:"reason"`
				}
				if raw, ok := body["priority"]; ok {
					if err := json.Unmarshal(raw, &priority); err != nil || priority.Level != "high" {
						t.Fatal(string(raw), err)
					}
				}
				var items []followupProposalItem
				if err := json.Unmarshal(body["items"], &items); err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					got[priority.Reason] = append(got[priority.Reason], item.Problem)
				}
			}
			if !reflect.DeepEqual(got[""], []string{"通常", "空白", "長すぎ"}) || !reflect.DeepEqual(got["急ぐ理由"], []string{"優先1", "優先2"}) || !reflect.DeepEqual(got[strings.Repeat("界", 500)], []string{"境界"}) {
				t.Fatal(got)
			}
			posts := f.NoticePosts
			var err error
			r, err = f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			f.DropNoticeResponse, f.HideNotices = false, false
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != posts {
				t.Fatal("resume", err, f.NoticePosts, posts)
			}
			for _, state := range r.FollowupProposals {
				if state.Status != "sent" {
					t.Fatal(state)
				}
			}
		})
	}
}

func TestFollowupInterruptNormalizedPostingAndRetainedResume(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := proposalRecord()
			raw := encode(Review{Decision: "approve", FindingSpecs: []reviewFindingSpec{{Problem: "優先", Expected: fixtureEvidence + "期待", Test: fixtureVerification}, {Problem: "通常", Expected: fixtureEvidence + "期待", Test: fixtureVerification}, {Problem: "旧形式", Expected: fixtureEvidence + "期待", Test: fixtureVerification}}, Findings: []string{"優先"}, Kinds: []string{"other"}, Severities: []string{"minor"}, Improvements: []string{"通常", "旧形式"}, Interrupts: []reviewInterrupt{{Text: "優先", Reason: " 早く修正 "}, {Text: "通常 ", Reason: "不一致"}}})
			_, r.Review = normalizeReview(raw)
			var normalized Review
			if err := json.Unmarshal(r.Review, &normalized); err != nil {
				t.Fatal(err)
			}
			normalized.DeferredReviewItems = []followupProposalItem{reviewProposalItems(normalized)[2]}
			r.Review = encode(normalized)
			f.DropNoticeResponse, f.HideNotices = unknown, unknown
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			bodies := proposalBodies(t, f)
			got := map[string][]string{}
			for _, body := range bodies {
				var priority struct {
					Level  string `json:"level"`
					Reason string `json:"reason"`
				}
				if p, ok := body["priority"]; ok {
					if err := json.Unmarshal(p, &priority); err != nil || priority.Level != "high" {
						t.Fatal(string(p), err)
					}
				}
				var items []followupProposalItem
				if err := json.Unmarshal(body["items"], &items); err != nil {
					t.Fatal(err)
				}
				for _, item := range items {
					got[priority.Reason] = append(got[priority.Reason], item.Problem)
				}
			}
			if !reflect.DeepEqual(got["早く修正"], []string{"優先"}) || !reflect.DeepEqual(got[""], []string{"通常", "旧形式"}) {
				t.Fatal(got)
			}
			posts := f.NoticePosts
			resumed, err := f.Executor.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			rememberReview(resumed)
			resumed.Review = mergeRetainedReview(resumed.Review, resumed.PreviousReview)
			f.DropNoticeResponse, f.HideNotices = false, false
			if err := f.Executor.postFollowupProposals(context.Background(), resumed); err != nil || f.NoticePosts != posts {
				t.Fatal(err, f.NoticePosts, posts)
			}
			for _, state := range resumed.FollowupProposals {
				if state.Status != "sent" {
					t.Fatal(state)
				}
			}
		})
	}
}

func TestFollowupInterruptRetainedThroughReview(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.PreviousReview = encode(Review{Decision: "hold", DeferredReviewItems: []followupProposalItem{{Problem: "保持", Expected: fixtureEvidence + "期待", Test: fixtureVerification}}})
	_, r.Review, _ = decideReview(r, encode(Review{Decision: "approve", Improvements: []string{"保持"}, Interrupts: []reviewInterrupt{{Text: "保持", Reason: " 急ぐ "}}}))
	rememberReview(r)
	_, r.Review, _ = decideReview(r, encode(Review{Decision: "hold"}))
	rememberReview(r)
	_, r.Review, _ = decideReview(r, encode(Review{Decision: "approve"}))
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	bodies := proposalBodies(t, f)
	if len(bodies) != 1 {
		t.Fatal(bodies)
	}
	var priority struct {
		Level  string `json:"level"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(bodies[0]["priority"], &priority); err != nil || priority.Level != "high" || priority.Reason != "急ぐ" {
		t.Fatal(priority, err)
	}
}

func TestFollowupInterruptAbsentInOldReview(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.Review = encode(Review{Decision: "approve", Improvements: []string{"旧レビューの改善"}, ImprovementSpecs: []followupProposalItem{fixtureProposal("旧レビューの改善")}})
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	bodies := proposalBodies(t, f)
	if len(bodies) != 1 || bodies[0]["priority"] != nil {
		t.Fatal(bodies)
	}
	var items []followupProposalItem
	if err := json.Unmarshal(bodies[0]["items"], &items); err != nil || len(items) != 1 || items[0].Problem != "旧レビューの改善" {
		t.Fatal(items, err)
	}
}

func TestFollowupProposalEligibility(t *testing.T) {
	const problem = "handler.go:42 で空の入力が重複投稿される"
	const evidence = "確認済み根拠: 固定版 abc123 handler.go:42 で空入力も送信される。契約 api.md:10 は空入力を拒否する\n空入力の投稿を拒否する"
	const verification = "go test ./internal/handler\n再現入力: 空文字列\n期待結果: 投稿0件"
	no := false
	valid := reviewFindingSpec{Group: "handler", Problem: problem, Expected: evidence, Test: verification, ExternalCheckOnly: &no}
	for _, tc := range []struct {
		name       string
		spec       reviewFindingSpec
		recordOnly bool
		external   bool
		fallback   bool
		mixed      bool
		want       int
	}{
		{name: "eligible", spec: valid, want: 1},
		{name: "fullwidth colons and reversed labels", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n期待結果：投稿0件\n再現入力：空文字列"}, want: 1},
		{name: "command only", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler"}},
		{name: "missing input label", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n期待結果: 投稿0件"}},
		{name: "missing result label", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: 空文字列"}},
		{name: "missing method", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "再現入力: 空文字列\n期待結果: 投稿0件"}},
		{name: "punctuation method", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "。、；\n再現入力: 空文字列\n期待結果: 投稿0件"}},
		{name: "punctuation input", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: 。、；\n期待結果: 投稿0件"}},
		{name: "punctuation result", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: 空文字列\n期待結果: 。、；"}},
		{name: "repeated input label", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: 再現入力: \n期待結果: 投稿0件"}},
		{name: "missing evidence", spec: reviewFindingSpec{Problem: problem, Expected: "投稿を拒否", Test: verification}},
		{name: "empty evidence", spec: reviewFindingSpec{Problem: problem, Expected: "確認済み根拠: \n投稿を拒否", Test: verification}},
		{name: "empty input", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: \n期待結果: 投稿0件"}},
		{name: "empty result", spec: reviewFindingSpec{Problem: problem, Expected: evidence, Test: "go test ./internal/handler\n再現入力: 空文字列\n期待結果: "}},
		{name: "fallback", fallback: true},
		{name: "record only", spec: valid, recordOnly: true},
		{name: "external only", spec: valid, external: true},
		{name: "undecided", spec: reviewFindingSpec{Problem: problem}},
		{name: "mixed", spec: valid, mixed: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			base := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := base(ctx, req)
				if req.Review && err == nil {
					var raw map[string]any
					if err := json.Unmarshal(a.JSON, &raw); err != nil {
						t.Fatal(err)
					}
					findings := []string{problem}
					specs := []reviewFindingSpec{}
					if !tc.fallback {
						spec := tc.spec
						if tc.external {
							yes := true
							spec.ExternalCheckOnly = &yes
						}
						specs = append(specs, spec)
					}
					if tc.mixed {
						findings = []string{"別の不完全な指摘", problem}
					}
					kinds, severities := make([]string, len(findings)), make([]string, len(findings))
					for i := range findings {
						kinds[i], severities[i] = "other", "minor"
					}
					raw["findings"], raw["findingKinds"], raw["findingSeverities"], raw["findingSpecs"] = findings, kinds, severities, specs
					raw["improvements"], raw["resolvableUncertainties"] = []string{}, []string{}
					if tc.want == 1 {
						raw["followupSpecs"] = []map[string]string{{"group": valid.Group, "problem": valid.Problem, "expected": tc.spec.Expected, "test": tc.spec.Test}}
					}
					if tc.recordOnly {
						raw["recordOnly"] = []string{problem}
					}
					a.JSON = encode(raw)
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			bodies := proposalBodies(t, f)
			if len(bodies) != tc.want {
				t.Fatalf("proposals=%v", bodies)
			}
			if tc.want == 1 {
				var items []followupProposalItem
				if err := json.Unmarshal(bodies[0]["items"], &items); err != nil || len(items) != 1 || items[0].Problem != problem || items[0].Expected != evidence || items[0].Test != tc.spec.Test || string(bodies[0]["version"]) != "3" {
					t.Fatalf("proposal=%v err=%v", bodies[0], err)
				}
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil || !strings.Contains(string(r.Review), problem) {
				t.Fatalf("review=%v err=%v", r, err)
			}
			if tc.mixed && !strings.Contains(string(r.Review), "別の不完全な指摘") {
				t.Fatal("mixed finding lost")
			}
			found := false
			for _, note := range f.Notes {
				if data, ok := note["data"].(map[string]any); ok && data["text"] == problem {
					found = true
				}
			}
			if !found {
				t.Fatalf("finding missing from notes: %v", f.Notes)
			}
			if tc.mixed {
				found = false
				for _, note := range f.Notes {
					if data, ok := note["data"].(map[string]any); ok && data["text"] == "別の不完全な指摘" {
						found = true
					}
				}
				if !found {
					t.Fatal("mixed finding missing from notes")
				}
			}
		})
	}
}

func TestFollowupHistoricalMismatchedSpecRetainsIdentity(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	other := fixtureProposal("別の指摘")
	r.Review = encode(Review{Decision: "approve", Findings: []string{"保存された指摘"}, Kinds: []string{"other"}, Severities: []string{"minor"}, FindingSpecs: []reviewFindingSpec{{Problem: other.Problem, Expected: other.Expected, Test: other.Test}}})
	id := followupProposalID(r, 0)
	r.FollowupProposals = []journal.FollowupProposal{{ID: id, Generation: r.Generation, Head: r.Head, Status: "sent", DeliveryOutcome: "confirmed"}}
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var review Review
	if err := json.Unmarshal(saved.Review, &review); err != nil {
		t.Fatal(err)
	}
	original := followupProposalItem{Problem: "保存された指摘", Expected: other.Expected, Test: other.Test}
	if f.NoticePosts != 0 || len(review.DeferredReviewDeliveries) != 1 || review.DeferredReviewDeliveries[0].Proposal.ID != id || deliveryItemHash(review.DeferredReviewDeliveries[0]) != digestReview(string(encode(wireProposalItem(original)))) {
		t.Fatalf("historical delivery changed: posts=%d deliveries=%+v", f.NoticePosts, review.DeferredReviewDeliveries)
	}
}
