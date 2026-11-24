package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func createRequestedPullForTest(ctx context.Context, e *Executor, r monitorRequest) (map[string]any, error) {
	progress, err := e.loadRequest(r)
	if err != nil {
		return nil, err
	}
	return e.createRequestedPullProgress(ctx, progress)
}

func TestRequestedPatchPublishesExactContentAndResumesWithoutDuplicate(t *testing.T) {
	f := newPipeline(t, "approve")
	base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
	sum := sha256.Sum256([]byte(patch))
	r := monitorRequest{ID: "123-aabbccdd", Kind: "pr_create", Repo: "owner/project"}
	p := prPayload{Title: "feature", Body: "feature body", Patch: patch, Base: base, PatchHash: hex.EncodeToString(sum[:])}
	r.Payload = encode(p)
	first, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil {
		t.Fatal(err)
	}
	if first["status"] != "created" {
		t.Fatalf("result %v", first)
	}
	before := f.git(f.root, "--git-dir", f.bare, "rev-parse", "chat/"+r.ID)
	second, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil {
		t.Fatal(err)
	}
	if second["number"] != first["number"] {
		t.Fatal("duplicate pull")
	}
	if after := f.git(f.root, "--git-dir", f.bare, "rev-parse", "chat/"+r.ID); after != before {
		t.Fatal("published branch changed")
	}
	if main := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main"); main != base {
		t.Fatal("PR creation changed main")
	}
	content := f.git(f.root, "--git-dir", f.bare, "show", before+":feature.txt")
	if content != "accepted feature" {
		t.Fatal("patch content mismatch")
	}
}
func TestManualRequestTestsExactHeadAndStoresBeforeMerge(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/direct"
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "feature")
	f.git(source, "push", f.bare, f.Branch)
	f.head = f.git(source, "rev-parse", "HEAD")
	f.pull = true
	p := &requestProgress{Request: monitorRequest{ID: "124-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}, Stage: "claimed"}
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "merged" || !f.merged {
		t.Fatalf("result %v", result)
	}
	if p.Stage != "posted" {
		t.Fatal("merge not persisted before sending")
	}
	saved, err := f.Executor.loadRequest(p.Request)
	if err != nil || saved.Stage != "posted" {
		t.Fatal("missing durable merge marker", err)
	}
	resumed, err := f.Executor.mergeRequestedPull(context.Background(), saved)
	if err != nil || resumed["status"] != "merged" {
		t.Fatal("resume failed", err, resumed)
	}
}

func TestRowDeletionNeverReachesAutomaticMerge(t *testing.T) {
	f := newPipeline(t, "sql-delete")
	result, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "held" || result.HoldReason != "deletions" || f.merged {
		t.Fatalf("削除SQLが承認待ちを通過しました: %+v", result)
	}
}

func TestExistingJobWithoutProviderUsesConfiguredDefault(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Provider = ""
	f.Job.Raw = encode(d)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || !f.merged {
		t.Fatal("providerの無い旧ジョブが完了しません", err, state.Status)
	}
}

func TestPullCreateReturnsExistingOpenPullOfTheSameBranch(t *testing.T) {
	f := newPipeline(t, "approve")
	f.pull = true
	f.Branch = "fix/same-branch"
	f.Body = "別の依頼が作ったPR"
	f.head = "1111111111111111111111111111111111111111"
	r := monitorRequest{ID: "130-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Branch: "fix/same-branch", Title: "題", Body: "本文"})}
	result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil || result["status"] != "created" || result["number"] != int64(1) || f.CreatePosts != 0 {
		t.Fatalf("既存PRを返すはずです: %v %v 作成=%d", result, err, f.CreatePosts)
	}
}

// 同じブランチから別の枝へ向いた開いているPRは、既定ブランチへのPR作成の結果にしない。
func TestPullCreateIgnoresOpenPullToAnotherBase(t *testing.T) {
	f := newPipeline(t, "approve")
	f.pull = true
	f.PullBase = "release"
	f.Branch = "fix/same-branch"
	f.Body = "別の枝へのPR"
	f.head = "1111111111111111111111111111111111111111"
	f.git(f.root, "--git-dir", f.bare, "branch", "fix/same-branch", "main")
	r := monitorRequest{ID: "132-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Branch: "fix/same-branch", Title: "題", Body: "本文"})}
	result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err != nil || f.CreatePosts != 1 || result["status"] != "created" || result["base"] != "main" {
		t.Fatalf("別の枝へのPRを結果にしました: %v 作成=%d %v", result, f.CreatePosts, err)
	}
}

func TestPullCreateRejectionBecomesRefusedInsteadOfRetryingForever(t *testing.T) {
	f := newPipeline(t, "approve")
	f.CreateStatus = 409
	r := monitorRequest{ID: "131-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Branch: "fix/other-branch", Title: "題", Body: "本文"})}
	f.git(f.root, "--git-dir", f.bare, "branch", "fix/other-branch", "main")
	for i := 0; i < 2; i++ {
		result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
		if err != nil || result["status"] != "refused" {
			t.Fatalf("拒否は結果として終えるはずです: %v %v", result, err)
		}
	}
}

func TestBusyRequestsHandleBranchPRCreationAndReleasePatchOnes(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Host = "fixture"
	f.Executor.Config.Repositories = []Repository{{Name: "owner/project"}}
	f.Executor.Config.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
	f.git(f.root, "--git-dir", f.bare, "branch", "fix/busy", "main")
	f.NextRequest = &monitorRequest{ID: "140-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Branch: "fix/busy", Title: "題", Body: "本文"})}
	if err := f.Executor.ProcessBusyRequestsFor(context.Background(), []string{"owner/project"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.RequestKinds, []string{"merge_manual", "pr_create"}) || len(f.RequestPosts) != 1 || f.RequestPosts[0] != "/api/v1/requests/140-aabbccdd/result" || f.CreatePosts != 1 {
		t.Fatalf("ブランチ指定のPR作成を処理するはずです: kinds=%v posts=%v 作成=%d", f.RequestKinds, f.RequestPosts, f.CreatePosts)
	}
	f.Executor.busyRequestsNext = time.Time{}
	f.RequestPosts = nil
	f.NextRequest = &monitorRequest{ID: "141-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Title: "題", Body: "本文", Patch: "diff", Base: strings.Repeat("a", 40), PatchHash: "x"})}
	if err := f.Executor.ProcessBusyRequestsFor(context.Background(), []string{"owner/project"}); err != nil {
		t.Fatal(err)
	}
	if len(f.RequestPosts) != 1 || f.RequestPosts[0] != "/api/v1/requests/141-aabbccdd/release" || f.CreatePosts != 1 {
		t.Fatalf("パッチ指定は作業ツリーを使うので待ちへ戻すはずです: posts=%v 作成=%d", f.RequestPosts, f.CreatePosts)
	}
}

func TestBusyRequestsTakeManualMergeFirst(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Host = "fixture"
	f.Executor.Config.Repositories = []Repository{{Name: "owner/project"}}
	f.Executor.Config.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
	f.NextRequest = &monitorRequest{ID: "142-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: json.RawMessage(`{"number":0,"head":"x"}`)}
	if err := f.Executor.ProcessBusyRequestsFor(context.Background(), []string{"owner/project"}); err != nil {
		t.Fatal(err)
	}
	if len(f.RequestKinds) != 1 || f.RequestKinds[0] != "merge_manual" {
		t.Fatalf("手動マージを最初に引き取るはずです: %v", f.RequestKinds)
	}
	if len(f.RequestPosts) != 1 || f.RequestPosts[0] != "/api/v1/requests/142-aabbccdd/result" {
		t.Fatalf("ジョブ中でも待ちへ戻さず結果を返すはずです: %v", f.RequestPosts)
	}
}

// 公開済みの依頼の枝を後から誰かが進めていたら、手元のコミットが記録どおりでもPRを作らない。
func TestRequestedPatchRefusesPublishedBranchMovedByOthers(t *testing.T) {
	f := newPipeline(t, "approve")
	base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
	sum := sha256.Sum256([]byte(patch))
	r := monitorRequest{ID: "124-aabbccdd", Kind: "pr_create", Repo: "owner/project"}
	r.Payload = encode(prPayload{Title: "feature", Body: "feature body", Patch: patch, Base: base, PatchHash: hex.EncodeToString(sum[:])})
	f.CreateStatus = 422
	if _, _ = createRequestedPullForTest(context.Background(), f.Executor, r); f.CreatePosts != 1 {
		t.Fatalf("PRの作成まで進んでいません: %d", f.CreatePosts)
	}
	// 第三者が公開済みの枝へ別のコミットを積む。
	published := f.git(f.root, "--git-dir", f.bare, "rev-parse", "chat/"+r.ID)
	tree := f.git(f.root, "--git-dir", f.bare, "rev-parse", published+"^{tree}")
	other := f.git(f.root, "--git-dir", f.bare, "-c", "user.name=x", "-c", "user.email=x@invalid", "commit-tree", "-p", published, "-m", "other", tree)
	f.git(f.root, "--git-dir", f.bare, "update-ref", "refs/heads/chat/"+r.ID, other)
	f.CreateStatus = 0
	posts := f.CreatePosts
	result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
	if err == nil || !strings.Contains(err.Error(), "公開済みの依頼ブランチと記録が一致しません") || f.CreatePosts != posts {
		t.Fatalf("第三者が進めた枝でPRを作りました: %v %v", result, err)
	}
}

// パッチの依頼は、SHA256の不一致と、既定ブランチの祖先でないbaseを断る。
func TestRequestedPatchRefusesBadHashAndBase(t *testing.T) {
	f := newPipeline(t, "approve")
	main := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
	patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
	sum := sha256.Sum256([]byte(patch))
	tree := f.git(f.root, "--git-dir", f.bare, "rev-parse", main+"^{tree}")
	stray := f.git(f.root, "--git-dir", f.bare, "-c", "user.name=x", "-c", "user.email=x@invalid", "commit-tree", "-m", "stray", tree)
	f.git(f.root, "--git-dir", f.bare, "update-ref", "refs/heads/stray", stray)
	for _, tc := range []struct {
		id, want string
		p        prPayload
	}{
		{"125-aabbccdd", "パッチの内容・base・SHA256が不正です", prPayload{Title: "t", Body: "b", Patch: patch, Base: main, PatchHash: strings.Repeat("0", 64)}},
		{"126-aabbccdd", "baseが既定ブランチの祖先ではありません", prPayload{Title: "t", Body: "b", Patch: patch, Base: stray, PatchHash: hex.EncodeToString(sum[:])}},
	} {
		r := monitorRequest{ID: tc.id, Kind: "pr_create", Repo: "owner/project", Payload: encode(tc.p)}
		result, err := createRequestedPullForTest(context.Background(), f.Executor, r)
		if !strings.Contains(fmt.Sprint(err, result["message"]), tc.want) || f.CreatePosts != 0 {
			t.Fatalf("%s: 断っていません: %v %v", tc.want, result, err)
		}
	}
}
