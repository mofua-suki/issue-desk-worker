package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestReviewBaselinePipeline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, decision, damage, kind string
		change, upstream, noFindings bool
	}{
		{name: "first", kind: "full"},
		{name: "base merge", decision: "approve", upstream: true, kind: "skipped"},
		{name: "changed", decision: "approve", change: true, upstream: true, kind: "delta"},
		{name: "held", decision: "hold", change: true, kind: "delta"},
		{name: "held unchanged", decision: "hold", kind: "held"},
		{name: "held without findings", decision: "hold", kind: "held", noFindings: true},
		{name: "held corrupt", decision: "hold", damage: "corrupt", kind: "full"},
		{name: "held missing", decision: "hold", damage: "missing", kind: "full"},
		{name: "held mismatch", decision: "hold", damage: "hash", kind: "full"},
		{name: "held generation", decision: "hold", damage: "generation", kind: "full"},
		{name: "held request", decision: "hold", damage: "request", kind: "full"},
		{name: "corrupt", decision: "approve", damage: "corrupt", kind: "full"},
		{name: "missing", decision: "approve", damage: "missing", kind: "full"},
		{name: "record mismatch", decision: "approve", damage: "hash", kind: "full"},
		{name: "generation changed", decision: "approve", damage: "generation", kind: "full"},
		{name: "request changed", decision: "approve", damage: "request", kind: "full"},
		{name: "execution plan changed", decision: "approve", damage: "plan", kind: "full"},
		{name: "instruction boundary changed", decision: "approve", damage: "instruction boundary", kind: "full"},
		{name: "instruction recorded with old hash", decision: "approve", damage: "old instruction hash", kind: "full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "feature.txt")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
			before, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "test", Branch: f.Branch, Provider: "codex", Head: before.Head, Base: before.Base, Result: implementationFixture()}
			var baselineDescriptor Descriptor
			if tc.damage == "instruction boundary" || tc.damage == "old instruction hash" {
				var current Descriptor
				if err := json.Unmarshal(f.Job.Raw, &current); err != nil {
					t.Fatal(err)
				}
				current.View["instructions"] = encode([]instructionContent{{Text: "a", Context: "bc"}})
				f.Job.Raw = encode(current)
				baselineDescriptor.View = map[string]json.RawMessage{
					"instructions": encode([]instructionContent{{Text: "ab", Context: "c"}}),
				}
			}
			if tc.damage == "plan" {
				if err := json.Unmarshal(f.Job.Raw, &baselineDescriptor); err != nil {
					t.Fatal(err)
				}
				plan := executionPlan{Version: 1, Requirements: []executionRequirement{{
					ID: "code", Criterion: "機能を実装", Owner: "implementer", Phase: "verification",
					Method: "固定検証", Expected: "成功", EvidenceKind: "fixed_test",
				}}}
				baselineDescriptor.View["executionPlanSHA"] = encode(strings.Repeat("a", 64))
				baselineDescriptor.View["executionPlan"] = encode(plan)
				f.Job.Raw = encode(baselineDescriptor)
				plan.Requirements[0].Expected = "変更前の期待結果"
				baselineDescriptor.View["executionPlan"] = encode(plan)
			}
			if tc.decision != "" {
				raw := encode(map[string]any{"decision": tc.decision, "findings": []string{"前回の認証回帰"}, "findingKinds": []string{"acceptance"}, "findingSeverities": []string{"must"}, "summary": "元の保留理由", "deferredReviewItems": []followupProposalItem{{Problem: "保持項目", Expected: "期待", Test: "検証"}}, "deferredReviewDeliveries": []deferredReviewDelivery{{Source: "source", Proposal: journal.FollowupProposal{ID: "saved-delivery", Status: "sent"}}}})
				if tc.noFindings {
					raw = encode(map[string]any{"decision": "hold", "summary": "元の保留理由", "userJudgments": []string{"利用者の判断が必要"}, "deferredReviewItems": []followupProposalItem{{Problem: "保持項目"}}, "deferredReviewDeliveries": []deferredReviewDelivery{{Source: "source", Proposal: journal.FollowupProposal{ID: "saved-delivery", Status: "sent"}}}})
				}
				if tc.decision == "approve" {
					item := fixtureProposal("後続案")
					raw = encode(map[string]any{"decision": "approve", "improvements": []string{item.Problem}, "findingSpecs": []reviewFindingSpec{{Problem: item.Problem, Expected: item.Expected, Test: item.Test}}})
				}
				review, normalized := normalizeReview(raw)
				if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := f.Executor.saveReviewBaseline(r, before, review, normalized, "full", reviewRequestHash(baselineDescriptor, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
					t.Fatal(err)
				}
				r.Review = normalized
				switch tc.damage {
				case "corrupt":
					if err := os.WriteFile(reviewBaselinePath(c.Root, 1), []byte("broken"), 0600); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(reviewBaselinePath(c.Root, 1)); err != nil {
						t.Fatal(err)
					}
				case "hash":
					r.ReviewBaseline.Hash = "broken"
				case "generation":
					r.ReviewBaseline.Generation++
				case "old instruction hash":
					r.ReviewBaseline.RequestHash = digestReview(string(encode([]string{"Implement feature", "受入条件: feature.txtを生成する", "abc"})))
				case "request":
					r.ReviewBaseline.RequestHash = reviewRequestHash(Descriptor{}, "Implement feature", "changed body")
				}
			}
			if tc.change {
				if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\nadditional change\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(w.Tree, "add", "feature.txt")
				f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "change")
			}
			if tc.upstream {
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream only\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "upstream.txt")
				f.git(source, "commit", "-m", "upstream")
				f.git(source, "push", f.bare, "main")
			}
			marker := filepath.Join(f.root, "fixed-test-ran")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "touch '"+marker+"'")
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			original := f.Executor.Call
			calledDelta := false
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.Review && (tc.damage == "instruction boundary" || tc.damage == "old instruction hash") {
					_, data, ok := strings.Cut(req.Prompt, "\n根拠(データ): ")
					var input struct {
						Instructions []instructionContent
					}
					if !ok || json.NewDecoder(strings.NewReader(data)).Decode(&input) != nil ||
						len(input.Instructions) != 1 || input.Instructions[0] != (instructionContent{Text: "a", Context: "bc"}) {
						t.Fatal("フルレビューへの再移行で現在の追加指示が失われました")
					}
				}
				if req.Review && tc.kind == "delta" {
					if !strings.Contains(req.Prompt, "新たな差分") || !strings.Contains(req.Prompt, "前回の指摘") || len(req.Policy.ReadFiles) != 2 {
						t.Errorf("delta prompt/files: %s %v", req.Prompt, req.Policy.ReadFiles)
					} else {
						data, err := os.ReadFile(req.Policy.ReadFiles[1])
						if err != nil {
							t.Error(err)
						}
						if strings.Contains(string(data), "upstream only") {
							t.Error("base change in review delta")
						}
						if tc.change && !strings.Contains(string(data), "additional change") {
							t.Error("changed content missing from delta")
						}
						full, err := os.ReadFile(req.Policy.ReadFiles[0])
						if err != nil || !strings.Contains(string(full), "accepted feature") {
							t.Errorf("full patch unavailable: %v", err)
						}
						if tc.decision == "hold" {
							parts := strings.Split(req.Prompt, "根拠(データ): ")
							var evidence map[string]json.RawMessage
							if len(parts) != 2 || json.NewDecoder(strings.NewReader(parts[1])).Decode(&evidence) != nil {
								t.Fatal("review evidence unavailable")
							}
							var prior Review
							if json.Unmarshal(evidence["previousReview"], &prior) != nil || len(prior.Findings) != 1 || prior.Findings[0] != "前回の認証回帰" || strings.Count(req.Prompt, "前回の認証回帰") != 1 {
								t.Error("prior review not embedded exactly once")
							}
						}
						calledDelta = true
					}
				}
				return original(ctx, req)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			wantStatus := "merged"
			if tc.kind == "held" {
				wantStatus = "held"
			}
			if err != nil || state.Status != wantStatus {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("fixed test was not run", err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kind == "held" {
				var review Review
				if json.Unmarshal(saved.Review, &review) != nil || (!tc.noFindings && len(review.Findings) != 1) || (tc.noFindings && len(review.Findings) != 0) || len(review.DeferredReviewItems) != 1 || len(review.DeferredReviewDeliveries) != 1 || len(saved.FollowupProposals) != 1 || saved.FollowupProposals[0].Status != "sent" {
					t.Fatalf("retention lost: %+v proposals=%+v", review, saved.FollowupProposals)
				}
				if len(f.Calls) != 0 || saved.ReviewSkipped || saved.Runs != r.Runs || saved.ReviewReworkRuns != r.ReviewReworkRuns || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "差分が変わっていません") || (!tc.noFindings && !strings.Contains(string(state.Result), "前回の認証回帰")) || (tc.noFindings && !strings.Contains(string(state.Result), "利用者の判断が必要")) || len(proposalBodies(t, f)) != 0 || saved.PR != 0 {
					t.Fatalf("held calls=%d skipped=%v runs=%d/%d reworks=%d/%d reason=%s summary=%s posts=%d pr=%d", len(f.Calls), saved.ReviewSkipped, saved.Runs, r.Runs, saved.ReviewReworkRuns, r.ReviewReworkRuns, state.HoldReason, state.Result, f.NoticePosts, saved.PR)
				}
			} else if got := saved.ReviewRounds[len(saved.ReviewRounds)-1].Kind; got != tc.kind {
				t.Fatalf("kind=%s want %s", got, tc.kind)
			}
			if tc.kind == "skipped" {
				if len(f.Calls) != 0 || !saved.ReviewSkipped {
					t.Fatalf("calls=%v skipped=%v", f.Calls, saved.ReviewSkipped)
				}
				if len(saved.FollowupProposals) != 1 || saved.FollowupProposals[0].ID != followupProposalID(saved, 0) {
					t.Fatal("final head proposal missing", saved.FollowupProposals)
				}
			} else if tc.kind != "held" && len(f.Calls) != 1 {
				t.Fatalf("calls=%v", f.Calls)
			}
			if tc.kind == "delta" && !calledDelta {
				t.Fatal("delta was not reviewed")
			}
			// Reloaded records remain tied to their patch after execution.
			if _, ok := loadReviewBaseline(c.Root, saved); !ok {
				t.Fatal("baseline did not survive reload")
			}
		})
	}
}

func TestReviewDiffHashIncludesContextAndPositions(t *testing.T) {
	a := "diff --git a/a b/a\nindex 123..456 100644\n--- a/a\n+++ b/a\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"
	b := "diff --git a/a b/a\nindex abc..def 100644\n--- a/a\n+++ b/a\n@@ -9,2 +9,2 @@\n changed context\n-old\n+new\n"
	ah, ok := reviewDiffHash(a)
	if !ok {
		t.Fatal("bad patch")
	}
	bh, ok := reviewDiffHash(b)
	if !ok || ah == bh {
		t.Fatal("context and position must change identity")
	}
	ch, _ := reviewDiffHash(strings.Replace(b, "+new\n", "+new \n", 1))
	if ch == ah {
		t.Fatal("changed whitespace ignored")
	}
	if _, ok := reviewDiffHash("diff --git a/a b/a\nBinary files a/a and b/a differ\n"); ok {
		t.Fatal("binary comparison must fall back")
	}
}

func TestReviewDiffHashIgnoresPositionWithIdenticalContext(t *testing.T) {
	a := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1,7 +1,7 @@\n one\n two\n three\n-old\n+new\n four\n five\n six\n"
	b := strings.Replace(a, "@@ -1,7 +1,7 @@", "@@ -40,7 +50,7 @@", 1)
	ah, aok := reviewDiffHash(a)
	bh, bok := reviewDiffHash(b)
	if !aok || !bok || ah != bh {
		t.Fatalf("identical context at different positions: %q %q", ah, bh)
	}
}

func TestSkippedReviewResumeRequiresIntactBaseline(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "feature.txt")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	s, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := encode(map[string]any{"decision": "approve"})
	review, _ := normalizeReview(raw)
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "publish", Branch: f.Branch, Provider: "codex", Base: s.Base, Head: s.Head, Result: implementationFixture(), Review: raw, Test: encode(map[string]any{"passed": true})}
	fixtureRecordEvidence(t, f, r, w.Tree)
	if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.saveReviewBaseline(r, s, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
		t.Fatal(err)
	}
	r.ReviewSkipped = true
	r.AddReviewRound(journal.ReviewRound{Kind: "skipped", Base: s.Base, Head: s.Head})
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reviewBaselinePath(c.Root, 1)); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || saved.ReviewSkipped || saved.ReviewRounds[len(saved.ReviewRounds)-1].Kind != "full" {
		t.Fatalf("calls=%v rounds=%+v skipped=%v", f.Calls, saved.ReviewRounds, saved.ReviewSkipped)
	}
}

func TestSkippedReviewPostsForNewHeadOnlyOnce(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 {
		t.Fatalf("initial post: %v %d", err, f.NoticePosts)
	}
	r.Head = strings.Repeat("b", 40)
	r.ReviewSkipped = true
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), saved); err != nil || f.NoticePosts != 2 || len(saved.FollowupProposals) != 2 {
		t.Fatalf("new head: %v posts=%d records=%v", err, f.NoticePosts, saved.FollowupProposals)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), saved); err != nil || f.NoticePosts != 2 {
		t.Fatalf("same head repost: %v posts=%d", err, f.NoticePosts)
	}
}

func TestRecordedReviewPostsFinalHeadFollowupsOnce(t *testing.T) {
	f := newPipeline(t, "approve")
	useMinorReview(f)
	f.Executor.Config.Repositories[0].AutoMerge = false
	marker := filepath.Join(f.root, "fixed-test-runs")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "echo run >> '"+marker+"'")
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	state, err := f.Executor.Execute(ctx, f.Job, fence)
	if err != nil || state.Status != "held" {
		t.Fatalf("first: %+v %v", state, err)
	}
	if len(proposalBodies(t, f)) != 2 {
		t.Fatal("initial proposals missing")
	}
	before, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if before.ReviewBaseline == nil || before.ReviewRounds[len(before.ReviewRounds)-1].Kind != "full" {
		t.Fatal("real review not recorded")
	}
	before.FinalReported = true
	if err := f.Executor.Store.Save(before); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("base only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "upstream.txt")
	f.git(source, "commit", "-m", "upstream")
	f.git(source, "push", f.bare, "main")
	calls := len(f.Calls)
	state, err = f.Executor.Execute(ctx, f.Job, fence)
	if err != nil || state.Status != "held" {
		t.Fatalf("resume: %+v %v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(runs), "run") != 2 {
		t.Fatalf("fixed tests: %q %v", runs, err)
	}
	if len(f.Calls) != calls || len(proposalBodies(t, f)) != 4 || !saved.ReviewSkipped || saved.ReviewRounds[len(saved.ReviewRounds)-1].Kind != "skipped" {
		t.Fatalf("calls=%v proposals=%d rounds=%v", f.Calls, len(proposalBodies(t, f)), saved.ReviewRounds)
	}
	if before.Head == saved.Head {
		t.Fatal("base merge did not change head")
	}
	counts := map[string]int{}
	for _, body := range proposalBodies(t, f) {
		var id string
		if err := json.Unmarshal(body["id"], &id); err != nil {
			t.Fatal(err)
		}
		counts[id]++
	}
	for _, record := range []*journal.Record{before, saved} {
		for batch := 0; batch < 2; batch++ {
			id := followupProposalID(record, batch)
			if counts[id] != 1 {
				t.Fatalf("proposal %s posted %d times", id, counts[id])
			}
		}
	}
	saved.FinalReported = true
	if err := f.Executor.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(ctx, f.Job, fence)
	if err != nil || state.Status != "held" || len(proposalBodies(t, f)) != 4 {
		t.Fatalf("same head: state=%+v err=%v proposals=%d", state, err, len(proposalBodies(t, f)))
	}
}

func TestReviewIdentityWithRealLineNumberShift(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	source := filepath.Join(f.root, "source")
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "prepare")
	f.git(source, "push", f.bare, "main")
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	lines[20] = "feature changed line"
	if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "README.md")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	before, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := os.ReadFile(filepath.Join(source, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), append([]byte("base prefix\n"), upstream...), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "base prefix")
	f.git(source, "push", f.bare, "main")
	changed, err := w.Follow(ctx)
	if err != nil || !changed {
		t.Fatalf("follow: %v %v", changed, err)
	}
	after, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Diff == after.Diff || strings.Contains(after.Diff, "+base prefix") {
		t.Fatal("unexpected PR patch", before.Diff, after.Diff)
	}
	a, okA := reviewDiffHash(before.Diff)
	b, okB := reviewDiffHash(after.Diff)
	if !okA || !okB || a != b {
		t.Fatalf("shift changed identity: %s %s", before.Diff, after.Diff)
	}
}

// carriedHeldPC delivers PC-A's held state through the intake client (state
// save, then a lease on another PC) and runs it on a fresh journal and runtime.
type carriedHeldPC struct {
	exec    *Executor
	store   *journal.Store
	job     intake.Job
	desc    Descriptor
	ai      []bool // req.Review for each AI call on PC-B
	started int    // PC-A's runs as delivered
	usage   float64
}

func deliverCarriedHeld(t *testing.T, f *pipelineFixture, alter func(desc *Descriptor, worker, result map[string]any), call func(context.Context, providers.Request) (providers.Answer, error)) (*carriedHeldPC, intake.State, error) {
	t.Helper()
	ctx := context.Background()
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" {
		t.Fatalf("PC-A: %s %s %v", state.Status, state.HoldReason, err)
	}
	a, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var saved intake.State
	var job map[string]any
	_ = json.Unmarshal(f.Job.Raw, &job)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/jobs/1/state":
			var body struct {
				Epoch int64 `json:"epoch"`
				intake.State
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			saved = body.State
			saved.View, saved.Result = storedJSON(t, saved.View), storedJSON(t, saved.Result)
			io.WriteString(w, `{"job":{"id":1}}`)
		case "/api/v1/jobs/next":
			var view, result map[string]any
			_ = json.Unmarshal(saved.View, &view)
			_ = json.Unmarshal(saved.Result, &result)
			var desc Descriptor
			_ = json.Unmarshal(f.Job.Raw, &desc)
			if alter != nil {
				worker, _ := view["workerExecution"].(map[string]any)
				alter(&desc, worker, result)
			}
			for key, value := range desc.View {
				view[key] = value
			}
			job["generation"] = desc.Generation
			job["view"], job["result"], job["branch"], job["headSha"], job["pr"], job["leaseEpoch"] = view, result, saved.Branch, saved.HeadSHA, saved.PR, 4
			_ = json.NewEncoder(w).Encode(map[string]any{"job": job, "lease": map[string]any{"epoch": 4, "expiresAt": time.Now().Add(time.Hour)}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.State(ctx, 1, 3, state); err != nil {
		t.Fatal(err)
	}
	assignment, err := client.NextFor(ctx, 1, nil)
	if err != nil || assignment.Job == nil {
		t.Fatalf("next: %v", err)
	}
	pc := &carriedHeldPC{job: *assignment.Job, started: a.Runs, usage: a.WeightedUsage}
	if err := json.Unmarshal(pc.job.Raw, &pc.desc); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if pc.store, err = journal.Open(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.store.Close() })
	cfg := f.Executor.Config
	cfg.RuntimeDir = root
	cfg.Repositories = append([]Repository(nil), cfg.Repositories...)
	pc.exec = &Executor{Config: cfg, API: f.API, Gitea: f.Executor.Gitea, Store: pc.store, Executable: f.Executor.Executable, ready: true}
	pc.exec.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		pc.ai = append(pc.ai, req.Review)
		if call == nil {
			return providers.Answer{}, errors.New("unexpected AI call on PC-B")
		}
		return call(ctx, req)
	}
	f.CurrentLeaseEpoch, f.CurrentJobHead = 4, saved.HeadSHA
	f.CurrentView = pc.desc.View
	return pc, saved, nil
}

func (pc *carriedHeldPC) run(t *testing.T) intake.State {
	t.Helper()
	out, err := pc.exec.Execute(context.Background(), pc.job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("PC-B: %v", err)
	}
	return out
}

const carriedHoldSummary = "前回の差し戻しから差分が変わっていません"

func TestCarriedHeldReviewExecution(t *testing.T) {
	t.Parallel()
	for _, findings := range []bool{true, false} {
		t.Run(fmt.Sprint("findings=", findings), func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "review-hold")
			f.AllowUsage = true
			finding := "検査を迂回できる経路が残っています"
			reviewer := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				answer, err := reviewer(ctx, req)
				if req.Review && err == nil && findings {
					answer.JSON = changeSecondReview(secondReviewResponse("security", finding), "breaking", true)
				}
				return answer, err
			}
			pc, _, _ := deliverCarriedHeld(t, f, nil, nil)
			out := pc.run(t)
			if out.Status != "held" || out.HoldReason != "review_held" || len(pc.ai) != 0 {
				t.Fatalf("PC-B: %s %s ai=%v %s", out.Status, out.HoldReason, pc.ai, out.Result)
			}
			if !strings.Contains(string(out.Result), carriedHoldSummary) || findings && !strings.Contains(string(out.Result), finding) {
				t.Fatalf("hold reason lost: %s", out.Result)
			}
			var view struct {
				Worker struct {
					Held *journal.CarriedHeldReview `json:"carriedHeldReview"`
				} `json:"workerExecution"`
			}
			if json.Unmarshal(out.View, &view) != nil || view.Worker.Held == nil || view.Worker.Held.Decision != "held" {
				t.Fatalf("held state not delivered onward: %s", out.View)
			}
			var result struct {
				Test TestResult `json:"fixedTest"`
			}
			if json.Unmarshal(out.Result, &result) != nil || !result.Test.Passed {
				t.Fatalf("PC-B did not run the fixed test: %s", out.Result)
			}
		})
	}
}

func TestCarriedHeldReviewFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		alter func(desc *Descriptor, worker, result map[string]any)
	}{
		{"diff", func(_ *Descriptor, w, _ map[string]any) {
			w["carriedHeldReview"].(map[string]any)["diffHash"] = strings.Repeat("0", 64)
		}},
		{"generation", func(d *Descriptor, _, _ map[string]any) { d.Generation++ }},
		{"request", func(_ *Descriptor, w, _ map[string]any) {
			w["carriedHeldReview"].(map[string]any)["requestHash"] = strings.Repeat("1", 64)
		}},
		{"instruction", func(d *Descriptor, _, _ map[string]any) {
			d.View["instructions"] = encode([]map[string]string{{"id": "new", "text": "変更を加える"}})
		}},
		{"missing", func(_ *Descriptor, w, _ map[string]any) { delete(w, "carriedHeldReview") }},
		{"broken", func(_ *Descriptor, w, _ map[string]any) { w["carriedHeldReview"] = "broken" }},
		{"format", func(_ *Descriptor, w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["version"] = 2 }},
		{"review mismatch", func(_ *Descriptor, _, result map[string]any) {
			result["review"].(map[string]any)["summary"] = "別のレビュー"
		}},
		{"unknown order", func(_ *Descriptor, w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["epoch"] = 4 }},
		{"approved without patch", func(_ *Descriptor, w, result map[string]any) {
			result["review"].(map[string]any)["decision"] = "approve"
			w["carriedHeldReview"].(map[string]any)["decision"] = "approved"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "review-hold")
			f.AllowUsage = true
			pc, _, _ := deliverCarriedHeld(t, f, tc.alter, f.Executor.Call)
			out, _ := pc.exec.Execute(context.Background(), pc.job, func(context.Context) error { return nil })
			if strings.Contains(string(out.Result), carriedHoldSummary) {
				t.Fatalf("carried hold applied: %s", out.Result)
			}
			if !slices.Contains(pc.ai, true) {
				t.Fatalf("independent review did not run: ai=%v status=%s", pc.ai, out.Status)
			}
			if record, err := pc.store.Load(1); err != nil || record.ReviewSkipped {
				t.Fatalf("review skipped without a patch: %+v %v", record, err)
			}
		})
	}
}

func TestCarriedHeldReviewValidationAndCounts(t *testing.T) {
	t.Parallel()
	t.Run("counts", func(t *testing.T) {
		t.Parallel()
		f := newPipeline(t, "review-hold")
		f.AllowUsage = true
		pc, _, _ := deliverCarriedHeld(t, f, nil, nil)
		if out := pc.run(t); out.Status != "held" || len(pc.ai) != 0 {
			t.Fatalf("PC-B: %s ai=%v", out.Status, pc.ai)
		}
		record, err := pc.store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		if record.Runs != pc.started || record.ReviewReworkRuns != 0 || record.InterruptedRuns != 0 || record.UnansweredRuns != 0 || record.WeightedUsage != pc.usage || record.OpenRun != 0 {
			t.Fatalf("counts changed without AI: runs=%d/%d rework=%d interrupted=%d unanswered=%d usage=%v/%v open=%d", record.Runs, pc.started, record.ReviewReworkRuns, record.InterruptedRuns, record.UnansweredRuns, record.WeightedUsage, pc.usage, record.OpenRun)
		}
		for _, round := range record.ReviewRounds {
			if round.Kind == "full" || round.Kind == "delta" {
				t.Fatalf("a review round was recorded without a review: %+v", record.ReviewRounds)
			}
		}
	})
	t.Run("environment and provider", func(t *testing.T) {
		t.Parallel()
		f := newPipeline(t, "review-hold")
		f.AllowUsage = true
		pc, _, _ := deliverCarriedHeld(t, f, nil, nil)
		// The fixed test passes only after the setup ran on PC-B.
		marker := filepath.Join(f.root, "pc-b-setup")
		pc.exec.Config.Repositories[0].SetupCommand = fixtureShellCommand(f, "touch '"+marker+"'")
		pc.exec.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "test -f '"+marker+"'")
		if out := pc.run(t); out.Status != "held" || out.HoldReason != "review_held" || len(pc.ai) != 0 {
			t.Fatalf("PC-B: %s %s ai=%v %s", out.Status, out.HoldReason, pc.ai, out.Result)
		}
		record, err := pc.store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		// A later fallback to the normal review needs the implementation provider.
		if record.Provider == "" {
			t.Fatal("provider was not decided before the carried hold")
		}
	})
	t.Run("failing fixed test", func(t *testing.T) {
		t.Parallel()
		f := newPipeline(t, "review-hold")
		f.AllowUsage = true
		pc, _, _ := deliverCarriedHeld(t, f, nil, func(context.Context, providers.Request) (providers.Answer, error) {
			return providers.Answer{}, errors.New("fixture stop at repair")
		})
		pc.exec.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "false")
		out, _ := pc.exec.Execute(context.Background(), pc.job, func(context.Context) error { return nil })
		if strings.Contains(string(out.Result), carriedHoldSummary) || out.Status == "held" && out.HoldReason == "review_held" {
			t.Fatalf("failing test bypassed by the carried hold: %s %s %s", out.Status, out.HoldReason, out.Result)
		}
		var result struct {
			Test TestResult `json:"fixedTest"`
		}
		if json.Unmarshal(out.Result, &result) != nil || result.Test.Passed {
			t.Fatalf("fixed test result missing: %s", out.Result)
		}
	})
	t.Run("edit requests", func(t *testing.T) {
		t.Parallel()
		f := newPipeline(t, "review-hold")
		f.AllowUsage = true
		pc, _, _ := deliverCarriedHeld(t, f, func(_ *Descriptor, _, result map[string]any) {
			for _, key := range []string{"implementationResult", "previousImplementationResult"} {
				if report, ok := result[key].(map[string]any); ok {
					report["deleteFiles"] = []string{"README.md"}
				}
			}
		}, func(context.Context, providers.Request) (providers.Answer, error) {
			return providers.Answer{}, errors.New("fixture stop at implementation")
		})
		out, _ := pc.exec.Execute(context.Background(), pc.job, func(context.Context) error { return nil })
		if strings.Contains(string(out.Result), carriedHoldSummary) || len(pc.ai) == 0 || pc.ai[0] {
			t.Fatalf("replayed edit requests were treated as the held tree: ai=%v %s", pc.ai, out.Result)
		}
	})
}
