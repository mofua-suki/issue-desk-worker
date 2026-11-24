package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
)

// directMergeRequest は、マージ依頼できる直接PRのブランチと依頼を用意する。
func directMergeRequest(t *testing.T, f *pipelineFixture, id string) *requestProgress {
	t.Helper()
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
	return &requestProgress{Request: monitorRequest{ID: id, Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}, Stage: "claimed"}
}

func giteaRefusal(t *testing.T, message string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"message": message, "url": "http://gitea/api/swagger"})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestMergeRefusalKeepsGiteaReason(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "126-aabbccdd")
	f.MergeFailStatus = 405
	f.MergeFailBody = giteaRefusal(t, "The PR cannot be fast-forwarded")
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := result["message"].(string)
	if result["status"] != "failed" || f.merged {
		t.Fatalf("result %v", result)
	}
	for _, want := range []string{"マージ後のPRの結果を確認できません", "405", "The PR cannot be fast-forwarded"} {
		if !strings.Contains(message, want) {
			t.Fatalf("%q がありません: %s", want, message)
		}
	}
}

func TestMergeRefusalWithoutJSONKeepsBody(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "127-aabbccdd")
	f.MergeFailStatus = 502
	f.MergeFailBody = "bad gateway from proxy"
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if message, _ := result["message"].(string); result["status"] != "failed" || !strings.Contains(message, "bad gateway from proxy") {
		t.Fatalf("result %v", result)
	}
}

func TestMergeRefusalRedactsCredentialsAndLimitsLength(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "128-aabbccdd")
	f.Executor.secret = "fixture-gitea-credential"
	// 秘密の検査に掛からないよう、トークン様の値は実行時に組み立てる。
	token := "idk_" + strings.Repeat("a", 24)
	f.MergeFailStatus = 405
	f.MergeFailBody = giteaRefusal(t, "denied "+f.Executor.secret+" "+token+" "+strings.Repeat("x", 5000))
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := result["message"].(string)
	if result["status"] != "failed" || !strings.Contains(message, "denied [REDACTED] [REDACTED]") {
		t.Fatalf("result %v", result)
	}
	if strings.Contains(message, f.Executor.secret) || strings.Contains(message, token) {
		t.Fatalf("資格情報が残っています: %s", message)
	}
	// 理由は上限で切り、観測した状態(merged・open・mergeable)の文は別に添える。
	if limit := len([]rune("マージ後のPRの結果を確認できません(観測: merged=false, open=true, mergeable=true): ")) + mergeErrorLimit; len([]rune(message)) > limit {
		t.Fatalf("長すぎます: %d", len([]rune(message)))
	}
	saved, err := f.Executor.loadRequest(p.Request)
	if err != nil || strings.Contains(saved.MergeError, f.Executor.secret) || strings.Contains(saved.MergeError, token) {
		t.Fatalf("保存した理由が伏せられていません: %v %s", err, saved.MergeError)
	}
}

func TestMergeErrorWithConfirmedMergeIsMerged(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "129-aabbccdd")
	f.MergeFailStatus = 500
	f.MergeFailApplied = true
	f.MergeFailBody = giteaRefusal(t, "response lost")
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "merged" || result["mergeCommit"] != f.head || !f.merged {
		t.Fatalf("result %v", result)
	}
}

func TestMergeReasonSurvivesFailedRereadAndResume(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "130-aabbccdd")
	f.MergeFailStatus = 405
	f.MergeFailBody = giteaRefusal(t, "The PR cannot be fast-forwarded")
	f.PullFailAfterPost = 500
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err == nil || result != nil {
		t.Fatalf("読み直しの失敗を返していません: %v %v", result, err)
	}
	if !strings.Contains(err.Error(), "The PR cannot be fast-forwarded") {
		t.Fatalf("エラーに理由がありません: %v", err)
	}
	saved, err := f.Executor.loadRequest(p.Request)
	if err != nil || saved.Stage != "posted" {
		t.Fatalf("再開の印がありません: %v", err)
	}
	f.PullFailAfterPost = 0
	resumed, err := f.Executor.mergeRequestedPull(context.Background(), saved)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := resumed["message"].(string)
	if resumed["status"] != "failed" || !strings.Contains(message, "指定headのfast-forwardマージを確認できません") || !strings.Contains(message, "The PR cannot be fast-forwarded") {
		t.Fatalf("result %v", resumed)
	}
	if f.MergePosts != 1 {
		t.Fatalf("マージ要求を再送しました: %d", f.MergePosts)
	}
}

func TestResumeWithoutMergeErrorKeepsFixedMessage(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "131-aabbccdd")
	p.Stage = "posted"
	p.Branch = f.Branch
	p.Base = "main"
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "failed" || result["message"] != "指定headのfast-forwardマージを確認できません(観測: merged=false, open=true, mergeable=true)" || f.MergePosts != 0 {
		t.Fatalf("result %v", result)
	}
}

func TestResumeMergeVerifiesDescendantCommit(t *testing.T) {
	for _, tc := range []struct {
		name, status            string
		unrelated, deleteBranch bool
	}{
		{name: "descendant", status: "merged"},
		{name: "deleted branch", status: "merged", deleteBranch: true},
		{name: "unrelated", status: "failed", unrelated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			p := directMergeRequest(t, f, "132-aabbccdd")
			f.PullFailAfterPost = 500
			if result, err := f.Executor.mergeRequestedPull(context.Background(), p); err == nil || result != nil || f.MergePosts != 1 {
				t.Fatalf("first result=%v err=%v posts=%d", result, err, f.MergePosts)
			}
			saved, err := f.Executor.loadRequest(p.Request)
			if err != nil || saved.Stage != "posted" {
				t.Fatalf("saved=%v err=%v", saved, err)
			}
			source := filepath.Join(f.root, "source")
			if tc.unrelated {
				f.git(source, "checkout", "--detach", f.head+"^")
			}
			f.git(source, "commit", "--allow-empty", "-m", "observed merge commit")
			f.MergeCommit = f.git(source, "rev-parse", "HEAD")
			f.git(source, "push", "--force", f.bare, "HEAD:refs/heads/main")
			if tc.deleteBranch {
				f.git(f.root, "--git-dir", f.bare, "update-ref", "-d", "refs/heads/"+f.Branch)
				f.pull = false // The API retains the PR head after branch deletion.
			}
			f.PullFailAfterPost = 0
			result, err := f.Executor.mergeRequestedPull(context.Background(), saved)
			if err != nil || result["status"] != tc.status || f.MergePosts != 1 {
				t.Fatalf("result=%v err=%v posts=%d", result, err, f.MergePosts)
			}
			if tc.unrelated && !strings.Contains(result["message"].(string), "マージ結果未確認") {
				t.Fatalf("result=%v", result)
			}
		})
	}
}

func TestMergeAncestryFetchFailureKeepsPostedForResume(t *testing.T) {
	f := newPipeline(t, "approve")
	p := directMergeRequest(t, f, "133-aabbccdd")
	source := filepath.Join(f.root, "source")
	f.git(source, "commit", "--allow-empty", "-m", "observed merge commit")
	f.MergeCommit = f.git(source, "rev-parse", "HEAD")
	f.git(source, "push", f.bare, "HEAD:refs/heads/observed-merge")

	marker := filepath.Join(f.root, "fail-fetch")
	script := filepath.Join(f.root, "git")
	body := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = fetch ] && [ -f %q ]; then
		echo 'temporary fetch failure' >&2
		exit 128
	fi
done
exec %q "$@"
`, marker, f.Executor.Config.Git)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	f.MergeFailStatus = 500
	f.MergeFailApplied = true
	f.MergeFailBody = giteaRefusal(t, "response lost")
	f.PullOverride = func(pull *gitea.Pull) {
		if pull.Merged {
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				t.Error(err)
			}
		}
	}

	ctx := context.Background()
	result, err := f.Executor.mergeRequestedPull(ctx, p)
	if err == nil || result != nil || !strings.Contains(err.Error(), "temporary fetch failure") || !strings.Contains(err.Error(), "response lost") || f.MergePosts != 1 {
		t.Fatalf("first result=%v err=%v posts=%d", result, err, f.MergePosts)
	}
	saved, err := f.Executor.loadRequest(p.Request)
	if err != nil || saved.Stage != "posted" {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	f.PullOverride = nil
	result, err = f.Executor.mergeRequestedPull(ctx, saved)
	if err == nil || result != nil || !strings.Contains(err.Error(), "temporary fetch failure") || !strings.Contains(err.Error(), "response lost") || f.MergePosts != 1 {
		t.Fatalf("retry result=%v err=%v posts=%d", result, err, f.MergePosts)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	result, err = f.Executor.mergeRequestedPull(ctx, saved)
	if err != nil || result["status"] != "merged" || result["mergeCommit"] != f.MergeCommit || f.MergePosts != 1 {
		t.Fatalf("resumed result=%v err=%v posts=%d", result, err, f.MergePosts)
	}
}

func TestProtectedMergeNeedsDelegatedApproval(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"承認なし":    {},
		"印なし":     {"approval": "PR 1 の権限変更をマージして"},
		"印だけ":     {"approvalDelegated": true},
		"番号違いの発言": {"approval": "PR 99 の権限変更をマージして", "approvalDelegated": true},
	} {
		f := newPipeline(t, "approve")
		p := directMergeRequest(t, f, "130-aabbccdd")
		f.Executor.Config.Fleet.ReleasePolicy.PermissionFiles = []string{"feature.txt"}
		var payload map[string]any
		if err := json.Unmarshal(p.Request.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		for key, value := range extra {
			payload[key] = value
		}
		p.Request.Payload = encode(payload)
		result, err := f.Executor.mergeRequestedPull(context.Background(), p)
		if err != nil {
			t.Fatal(name, err)
		}
		if result["status"] != "refused" || f.merged {
			t.Fatalf("%s: %v", name, result)
		}
	}
}
