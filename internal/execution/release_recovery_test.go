package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func currentPolicy(f *pipelineFixture, auto bool) *Configuration {
	c := f.Executor.Config
	c.Repositories = append([]Repository{}, c.Repositories...)
	for i := range c.Repositories {
		c.Repositories[i].AutoMerge = auto
	}
	return &c
}
func prepareMergeRecovery(t *testing.T, f *pipelineFixture) {
	t.Helper()
	f.CurrentPolicy = currentPolicy(f, false)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("準備結果: %s %v", state.Status, err)
	}
	record, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	record.Stage = "merge"
	record.FinalState = nil
	record.FinalReported = false
	if err := f.Executor.Store.Save(record); err != nil {
		t.Fatal(err)
	}
}
func TestMergeRecoveryRechecksCurrentPolicy(t *testing.T) {
	for _, kind := range []string{"auto-disabled", "permission-added", "approval-stale"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			record, _ := f.Executor.Store.Load(1)
			f.CurrentPolicy = currentPolicy(f, kind != "auto-disabled")
			if kind != "auto-disabled" {
				f.CurrentPolicy.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{"feature.txt"}}
			}
			if kind == "approval-stale" {
				f.CurrentView = map[string]json.RawMessage{"permissionApproval": encode(map[string]any{"id": "a", "head": record.Head, "pr": record.PR, "files": []string{"other.txt"}, "resumePublication": true, "issuePosted": true, "prPosted": true, "approvalStatement": "許可", "approvedFrom": "screen"})}
			}
			before := len(f.Calls)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" || f.merged {
				t.Fatalf("現在の門を迂回: %s %v", state.Status, err)
			}
			if len(f.Calls) != before {
				t.Fatal("保留のために不要な実装を繰り返しました")
			}
		})
	}
}
func TestMergeRecoveryRetestsAndReviewsAdvancedMain(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	before, _ := f.Executor.Store.Load(1)
	f.CurrentPolicy = nil
	source := filepath.Join(f.root, "source")
	_ = os.WriteFile(filepath.Join(source, "README.md"), []byte("advanced main\n"), 0600)
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "advance main")
	f.git(source, "push", f.bare, "main")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("追随結果: %s %v", state.Status, err)
	}
	after, _ := f.Executor.Store.Load(1)
	if after.Base == before.Base || after.Head == before.Head || len(f.Reviews) != 2 {
		t.Fatalf("旧基点のまま公開: base=%s reviews=%d", after.Base, len(f.Reviews))
	}
}
func TestFinalMergeRejectsChangedPullIdentity(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.PullOverride = func(p *gitea.Pull) {
		if f.PullReads >= 2 {
			p.Base.Ref = "other"
			p.Base.Repo.FullName = "fork/project"
		}
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if f.merged || err == nil && state.Status == "merged" {
		t.Fatal("別baseのPRをマージしました")
	}
}
func TestUnknownPRCreationIsNotReposted(t *testing.T) {
	f := newPipeline(t, "approve")
	f.CreatePostDrop = true
	f.HideCreated = true
	patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
	base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	r := patchRequest("126-aabbccdd", base, patch)
	for i := 0; i < 2; i++ {
		if _, err := createRequestedPullForTest(context.Background(), f.Executor, r); err == nil {
			t.Fatal("投稿不明を成功にしました")
		}
	}
	if f.CreatePosts != 1 {
		t.Fatalf("未知の投稿を再送: %d", f.CreatePosts)
	}
	f.HideCreated = false
	result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil || result["status"] != "created" || f.CreatePosts != 1 {
		t.Fatalf("目印の回収: %v %v", result, err)
	}
}
func TestManualMergeRequiresFastForwardResultCommit(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/manual"
	f.git(source, "checkout", "-b", f.Branch)
	_ = os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600)
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "manual")
	f.git(source, "push", f.bare, f.Branch)
	f.head = f.git(source, "rev-parse", "HEAD")
	f.pull = true
	f.MergeCommit = f.git(source, "rev-parse", "HEAD^")
	p := &requestProgress{Request: monitorRequest{ID: "127-aabbccdd", Repo: "owner/project", Kind: "merge_manual", Payload: encode(map[string]any{"number": 1, "head": f.head})}, Stage: "claimed"}
	for i := 0; i < 2; i++ {
		result, err := f.Executor.mergeRequestedPull(context.Background(), p)
		if err != nil || result["status"] != "failed" {
			t.Fatalf("異なるmerge commitを成功扱い: %v %v", result, err)
		}
	}
	if f.MergePosts != 1 {
		t.Fatal("送信後を再マージしました")
	}
}

func patchRequest(id, base, patch string) monitorRequest {
	sum := sha256.Sum256([]byte(patch))
	return monitorRequest{ID: id, Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Title: "feature", Body: "本文", Patch: patch, Base: base, PatchHash: hex.EncodeToString(sum[:])})}
}

func TestDefinitePRCreateRefusalCanResume(t *testing.T) {
	f := newPipeline(t, "approve")
	f.CreateStatus = 403
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err == nil || f.CreatePosts != 1 {
		t.Fatal("POST拒否のfixtureが不正です")
	}
	record, _ := f.Executor.Store.Load(1)
	if record.PRCreationAttempted {
		t.Fatal("明確拒否を未知投稿に固定しました")
	}
	f.CreateStatus = 0
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || f.CreatePosts != 2 {
		t.Fatalf("修正後の再開: %s %v", state.Status, err)
	}
}
func TestAlreadyMergedRecoveryUsesDestinationAndMergeCommit(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		record, _ := f.Executor.Store.Load(1)
		f.merged = true
		f.git(f.root, "--git-dir", f.bare, "update-ref", "refs/heads/main", record.Head)
		if wrong {
			f.PullOverride = func(p *gitea.Pull) { p.Base.Repo.FullName = "other/project" }
		}
		before := f.MergePosts
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if wrong {
			if err == nil && state.Status == "merged" {
				t.Fatal("別宛先のmerged PRを成功回収しました")
			}
		} else if err != nil || state.Status != "merged" {
			t.Fatalf("既mergedをpolicyの保留で止めました: %s %v", state.Status, err)
		}
		if f.MergePosts != before {
			t.Fatal("既mergedを再マージしました")
		}
	}
}
func TestDirectPRConfirmedRefusalCanResume(t *testing.T) {
	f := newPipeline(t, "approve")
	patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
	base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	r := patchRequest("128-aabbccdd", base, patch)
	f.CreateStatus = 422
	if _, err := createRequestedPullForTest(context.Background(), f.Executor, r); err == nil {
		t.Fatal("明確POST拒否をfixtureで再現できません")
	}
	p, err := f.Executor.loadRequest(r)
	if err != nil || p.Stage != "claimed" {
		t.Fatal("確実に拒否された投稿を未知に固定しました")
	}
	f.CreateStatus = 0
	result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil || result["status"] != "created" || f.CreatePosts != 2 {
		t.Fatalf("拒否後の修正再開: %v %v", result, err)
	}
}

func TestMergeHandoffUsesIntakeReceipts(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	var planned Descriptor
	if err := json.Unmarshal(f.Job.Raw, &planned); err != nil {
		t.Fatal(err)
	}
	planned.Scope = []string{"feature.txt"}
	planned.Generation = 1
	planned.View["retryFromBase"] = encode(map[string]int{"generation": planned.Generation})
	planned.View["executionPlanSHA"] = encode(strings.Repeat("a", 64))
	planned.View["executionPlan"] = encode(executionPlan{Version: 1, Requirements: []executionRequirement{
		{ID: "one", Criterion: "機能を実装", Owner: "implementer", Phase: "verification", Method: "固定検証", Expected: "成功", EvidenceKind: "fixed_test"},
		{ID: "report", Criterion: "訂正を投稿", Owner: "reporter", Phase: "report", Method: "根拠を照合", Expected: "投稿済み", EvidenceKind: "report_comment"},
		{ID: "deploy", Criterion: "配備を確認", Owner: "controller", Phase: "deployment", Method: "配備先を確認", Expected: "配備済み", EvidenceKind: "external_check"},
	}})
	f.Job.Raw = encode(planned)
	f.MergeFailStatus, f.PullFailAfterPost, f.MergeFailApplied = 503, 503, true
	if state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil }); err == nil || state.Status == "merged" || f.MergePosts != 1 || !f.merged {
		var result, view map[string]json.RawMessage
		_ = json.Unmarshal(state.Result, &result)
		_ = json.Unmarshal(state.View, &view)
		continuation := field[struct{ Cause string }](view, "continuationHead")
		t.Fatalf("Worker A did not stop after merge POST: status=%q hold=%q summary=%q wait=%q continuation=%q err=%v posts=%d applied=%t",
			state.Status, state.HoldReason, field[string](result, "summary"), field[string](view, "executionWait"), continuation.Cause, err, f.MergePosts, f.merged)
	}
	f.closed, f.MergeFailStatus, f.PullFailAfterPost = true, 0, 0
	var checkpoint intake.State
	for _, received := range f.ReceivedStates {
		var v map[string]json.RawMessage
		_ = json.Unmarshal(received.View, &v)
		if field[struct{ Stage string }](v, "workerExecution").Stage == "merge" {
			checkpoint = received
		}
	}
	if checkpoint.PR == nil || len(checkpoint.ControllerTest) == 0 {
		t.Fatalf("missing Worker A merge checkpoint: status=%q pr=%v fixedTest=%t",
			checkpoint.Status, checkpoint.PR != nil, hasJSON(checkpoint.ControllerTest))
	}
	f.Comments = append(f.Comments, map[string]any{"body": "訂正報告済み\n<!-- issue-desk-execution-report:1:" + checkpoint.HeadSHA + ":" + field[string](planned.View, "executionPlanSHA") + " -->"})
	reportComments := func() int {
		count := 0
		for _, comment := range f.Comments {
			body, _ := comment["body"].(string)
			if strings.Contains(body, "<!-- issue-desk-execution-report:") {
				count++
			}
		}
		return count
	}
	workerA := f.Executor
	jobA := f.Job
	f.CurrentLeaseEpoch = jobA.LeaseEpoch + 1
	lost := errors.New("lease lost")
	wrong := strings.Repeat("d", 40)
	var sentTest struct {
		Test json.RawMessage `json:"test"`
	}
	if err := json.Unmarshal(checkpoint.ControllerTest, &sentTest); err != nil {
		t.Fatal(err)
	}
	editReceipt := func(index int, key string, value any) func(map[string]any, map[string]json.RawMessage) {
		return func(_ map[string]any, view map[string]json.RawMessage) {
			receipts := field[[]map[string]any](view, "executionReceipts")
			receipts[index][key] = value
			view["executionReceipts"] = encode(receipts)
		}
	}
	for _, tc := range []struct {
		name, status string
		edit         func(map[string]any, map[string]json.RawMessage)
		pull         func(*gitea.Pull)
		fenceErr     error
		wantErr      bool
		staleJournal bool
		staleTarget  bool
		noPull       bool
		pullStatus   int
		resume       bool
	}{
		{name: "confirmed", status: "merged"},
		{name: "confirmed with stale journal and historical receipt", status: "merged", staleJournal: true, edit: func(_ map[string]any, view map[string]json.RawMessage) {
			receipts := field[[]map[string]any](view, "executionReceipts")
			historical := maps.Clone(receipts[0])
			historical["generation"] = 0
			view["executionReceipts"] = encode(append([]map[string]any{historical}, receipts...))
		}},
		{name: "confirmed with same-generation stale PR", status: "merged", staleTarget: true},
		{name: "confirmed without a plan with same-generation stale PR", status: "merged", staleTarget: true, edit: func(_ map[string]any, view map[string]json.RawMessage) {
			delete(view, "executionPlan")
			delete(view, "executionPlanSHA")
		}},
		{name: "non-retry current branch with same-generation stale PR", status: "stopped", staleTarget: true, noPull: true, edit: func(job map[string]any, _ map[string]json.RawMessage) {
			job["branch"] = "codex/issue-7-job-1"
		}},
		{name: "unmerged current PR with same-generation stale PR", status: "stopped", staleTarget: true, pull: func(p *gitea.Pull) { p.Merged = false }},
		{name: "unavailable current PR with same-generation stale PR", wantErr: true, staleTarget: true, pullStatus: 404},
		{name: "ordinary closure", status: "stopped", pull: func(p *gitea.Pull) { p.Merged = false }},
		{name: "old branch", status: "stopped", edit: func(job map[string]any, _ map[string]json.RawMessage) {
			job["branch"] = "codex/issue-7-job-1"
		}, pull: func(p *gitea.Pull) { p.Head.Ref = "codex/issue-7-job-1" }},
		{name: "old head", wantErr: true, edit: func(job map[string]any, _ map[string]json.RawMessage) {
			job["headSha"] = wrong
		}},
		{name: "different generation", status: "stopped", edit: func(job map[string]any, _ map[string]json.RawMessage) { job["generation"] = 2 }},
		{name: "awaiting approval", status: "stopped", edit: func(_ map[string]any, view map[string]json.RawMessage) {
			view["workerExecution"] = json.RawMessage(`{"stage":"held","generation":1}`)
		}},
		{name: "missing fixed test record", status: "held", edit: func(_ map[string]any, view map[string]json.RawMessage) { delete(view, "executionTestRecord") }},
		{name: "missing report", status: "held", resume: true, edit: func(_ map[string]any, view map[string]json.RawMessage) {
			receipts := field[[]map[string]any](view, "executionReceipts")
			view["executionReceipts"] = encode(receipts[:1])
		}},
		{name: "old receipt generation with stale journal", status: "held", staleJournal: true, edit: editReceipt(0, "generation", 0)},
		{name: "old receipt head", status: "held", edit: editReceipt(1, "head", wrong)},
		{name: "old receipt plan", status: "held", edit: editReceipt(0, "planSHA", strings.Repeat("b", 64))},
		{name: "wrong requirement", status: "held", edit: editReceipt(1, "requirement", "different")},
		{name: "wrong owner", status: "held", edit: editReceipt(0, "source", "reporter")},
		{name: "failed receipt", status: "held", edit: editReceipt(0, "status", "failed")},
		{name: "empty evidence", status: "held", edit: editReceipt(1, "detail", " ")},
		{name: "ambiguous receipt", status: "held", edit: func(_ map[string]any, view map[string]json.RawMessage) {
			receipts := field[[]map[string]any](view, "executionReceipts")
			view["executionReceipts"] = encode(append(receipts, receipts[1]))
		}},
		{name: "PR number", wantErr: true, pull: func(p *gitea.Pull) { p.Number++ }},
		{name: "head", wantErr: true, pull: func(p *gitea.Pull) { p.Head.SHA = wrong }},
		{name: "head repo", wantErr: true, pull: func(p *gitea.Pull) { p.Head.Repo.FullName = "other/repo" }},
		{name: "base repo", wantErr: true, pull: func(p *gitea.Pull) { p.Base.Repo.FullName = "other/repo" }},
		{name: "head branch", wantErr: true, pull: func(p *gitea.Pull) { p.Head.Ref = "other-branch" }},
		{name: "base branch", wantErr: true, pull: func(p *gitea.Pull) { p.Base.Ref = "other-branch" }},
		{name: "merge commit", wantErr: true, pull: func(p *gitea.Pull) { p.MergeCommitSHA = wrong }},
		{name: "lease lost", wantErr: true, fenceErr: lost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			var original Descriptor
			_ = json.Unmarshal(jobA.Raw, &raw)
			_ = json.Unmarshal(jobA.Raw, &original)
			view := original.View
			var checkpointView map[string]json.RawMessage
			_ = json.Unmarshal(checkpoint.View, &checkpointView)
			maps.Copy(view, checkpointView)
			raw["branch"], raw["pr"], raw["headSha"], raw["view"] = checkpoint.Branch, checkpoint.PR, checkpoint.HeadSHA, view
			raw["leaseEpoch"] = f.CurrentLeaseEpoch
			view["executionTestRecord"] = encode(map[string]any{
				"host": workerA.Config.Host, "generation": planned.Generation, "epoch": jobA.LeaseEpoch,
				"at": time.Now().UTC(), "test": sentTest.Test, "reportedHead": checkpoint.HeadSHA,
			})
			receipts := []map[string]any{
				{"requirement": "one", "generation": planned.Generation, "recordID": "test-1", "planSHA": field[string](view, "executionPlanSHA"), "head": checkpoint.HeadSHA, "status": "passed", "source": "implementer", "detail": "固定検証が完了"},
				{"requirement": "report", "generation": planned.Generation, "recordID": "report-1", "planSHA": field[string](view, "executionPlanSHA"), "head": checkpoint.HeadSHA, "status": "passed", "source": "reporter", "detail": "対象headの訂正報告を投稿済み"},
			}
			view["executionReceipts"] = encode(receipts)
			if tc.edit != nil {
				tc.edit(raw, view)
			}
			job := intake.Job{ID: jobA.ID, Repo: jobA.Repo, LeaseEpoch: f.CurrentLeaseEpoch, Raw: encode(raw)}
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if tc.staleJournal || tc.staleTarget {
				record := &journal.Record{ID: job.ID, Repo: job.Repo, Issue: original.Issue, Generation: 0, Epoch: jobA.LeaseEpoch,
					Stage: "held", Head: checkpoint.HeadSHA, Branch: checkpoint.Branch, PR: *checkpoint.PR,
					ExecutionPlanSHA: field[string](planned.View, "executionPlanSHA"), ExecutionReportPending: true,
					Test: sentTest.Test, ExecutionReport: encode(map[string]any{"head": checkpoint.HeadSHA})}
				if tc.staleTarget {
					record.Generation = planned.Generation
					record.PR++
					retryGeneration := planned.Generation
					record.RetryBaseGeneration = &retryGeneration
				}
				if err := store.Save(record); err != nil {
					t.Fatal(err)
				}
			}
			cfg := workerA.Config
			cfg.Host = "replacement"
			cfg.Assignments.WorkerAccounts = map[string]string{"replacement": "replacement-worker"}
			cfg.RuntimeDir = t.TempDir()
			b := &Executor{Config: cfg, API: f.API, Gitea: workerA.Gitea, Store: store, ready: true}
			b.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				t.Error("Worker B invoked a provider")
				return providers.Answer{}, errors.New("unexpected provider call")
			}
			f.PullOverride, f.PullFailAfterPost = tc.pull, tc.pullStatus
			comments := reportComments()
			postedBefore := len(f.ReceivedStates)
			pullReads := f.PullReads
			var state intake.State
			if tc.status == "merged" {
				f.Executor, f.Job = b, job
				t.Cleanup(func() { f.Executor, f.Job = workerA, jobA })
				runErrorResume(t, f)
				if sent := len(f.ReceivedStates) - postedBefore; sent != 1 {
					t.Fatalf("Worker B posted %d states, want one final report", sent)
				}
				state = f.ReceivedStates[postedBefore]
			} else {
				state, err = b.Execute(t.Context(), job, func(context.Context) error { return tc.fenceErr })
			}
			if (err != nil) != tc.wantErr || state.Status != tc.status || f.MergePosts != 1 {
				var result map[string]json.RawMessage
				_ = json.Unmarshal(state.Result, &result)
				t.Fatalf("Worker B status=%q hold=%q summary=%q err=%v merge posts=%d",
					state.Status, state.HoldReason, field[string](result, "summary"), err, f.MergePosts)
			}
			if reportComments() != comments || hasJSON(state.ControllerTest) || hasJSON(state.ControllerReport) {
				t.Fatalf("handoff repeated publication or controller evidence: comments added=%d fixedTest=%t report=%t",
					reportComments()-comments, hasJSON(state.ControllerTest), hasJSON(state.ControllerReport))
			}
			if tc.noPull && (f.PullReads != pullReads || state.PR != nil) {
				t.Fatalf("stopped handoff read a PR or returned a PR: reads=%d pr=%v", f.PullReads-pullReads, state.PR)
			}
			saved, loadErr := store.Load(job.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			switch tc.status {
			case "merged":
				var result map[string]json.RawMessage
				_ = json.Unmarshal(state.Result, &result)
				if saved == nil || !saved.FinalReported || state.PR == nil || *state.PR != *checkpoint.PR ||
					state.HeadSHA != checkpoint.HeadSHA || field[string](result, "mergeCommit") != checkpoint.HeadSHA ||
					hasJSON(result["fixedTest"]) != hasJSON(view["executionPlan"]) || hasJSON(result["implementationResult"]) {
					t.Fatalf("unexpected final evidence: acknowledged=%t head=%q mergeCommit=%q fixedTest=%t implementationResult=%t",
						saved != nil && saved.FinalReported, state.HeadSHA, field[string](result, "mergeCommit"),
						hasJSON(result["fixedTest"]), hasJSON(result["implementationResult"]))
				}
			case "held":
				var view map[string]json.RawMessage
				_ = json.Unmarshal(state.View, &view)
				if state.HoldReason != "review_held" || !field[bool](view, "codeMerged") || hasJSON(state.ControllerReport) {
					t.Fatalf("missing evidence: hold=%q codeMerged=%t report=%t",
						state.HoldReason, field[bool](view, "codeMerged"), hasJSON(state.ControllerReport))
				}
				if tc.resume {
					if field[string](view, "executionWait") != "reporter" {
						t.Fatalf("missing report: wait=%q", field[string](view, "executionWait"))
					}
					var assignmentView map[string]json.RawMessage
					_ = json.Unmarshal(encode(raw["view"]), &assignmentView)
					maps.Copy(assignmentView, view)
					assignmentView["executionReceipts"] = encode(receipts)
					raw["view"] = assignmentView
					job.LeaseEpoch++
					raw["leaseEpoch"] = job.LeaseEpoch
					job.Raw = encode(raw)
					resumed, err := b.Execute(t.Context(), job, func(context.Context) error { return nil })
					if err != nil || resumed.Status != "merged" || reportComments() != comments || f.MergePosts != 1 ||
						hasJSON(resumed.ControllerTest) || hasJSON(resumed.ControllerReport) {
						t.Fatalf("resuming with the receipt: status=%q hold=%q err=%v merge posts=%d comments added=%d fixedTest=%t report=%t",
							resumed.Status, resumed.HoldReason, err, f.MergePosts, reportComments()-comments,
							hasJSON(resumed.ControllerTest), hasJSON(resumed.ControllerReport))
					}
				}
			default:
				if tc.staleTarget {
					if saved == nil || saved.PR != *checkpoint.PR+1 || saved.Stage != "held" {
						t.Fatal("unverified recovery changed the stale journal")
					}
				} else if saved != nil {
					t.Fatal("unverified recovery saved a journal")
				}
				if tc.fenceErr != nil && !errors.Is(err, tc.fenceErr) {
					t.Fatal(err)
				}
			}
		})
	}
}
