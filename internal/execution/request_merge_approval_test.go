package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/gitea"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestMergeBranchAllowed(t *testing.T) {
	cases := []struct {
		branch   string
		number   int64
		approval string
		want     bool
	}{
		{"fix/foo", 3, "", true},
		{"codex/issue-14-job-2724", 30, "", false},
		{"codex/issue-14-job-2724", 30, "30はマージして", true},
		{"codex/issue-14-job-2724", 30, "300 をマージして", false},
		{"codex/issue-14-job-2724", 30, "30", false},
		{"codex/issue-14-job-2724", 30, "31 はマージして", false},
		{"bad..name", 3, "3 をマージして", false},
	}
	for _, c := range cases {
		if got := mergeBranchAllowed(c.branch, c.number, c.approval); got != c.want {
			t.Errorf("%q #%d %q: got %v want %v", c.branch, c.number, c.approval, got, c.want)
		}
	}
}

func TestMergeApprovalRecord(t *testing.T) {
	got := mergeApprovalRecord(&Executor{}, mergeRequestPayload{Approval: "PR 30 をマージして<!-- x", Head: "abc"}, &mergeGate{protected: []string{"internal/execution/guards.go"}, sql: true})
	for _, want := range []string{"PR 30 をマージして&lt;!-- x", "対象コミット: abc", "データ削除SQLを含みます", "権限ファイル: internal/execution/guards.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q に %q がありません", got, want)
		}
	}
}

// createApprovedFixture creates the PR through the creation path and builds the merge request the intake
// hands over after checking the creation request: the approval copied with approvalRequest.
// The creation request is not kept on this PC, as when another PC created the PR.
func createApprovedFixture(t *testing.T) (*pipelineFixture, *requestProgress) {
	t.Helper()
	f := newPipeline(t, "approve")
	merge := directMergeRequest(t, f, "124-aabbccdd")
	f.pull = false
	f.Executor.Config.Fleet.ReleasePolicy.PermissionFiles = []string{"feature.txt"}
	create := &requestProgress{Request: monitorRequest{ID: "123-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: encode(prPayload{Branch: f.Branch, Title: "feature", Body: "feature"})}, Stage: "claimed"}
	result, err := f.Executor.createRequestedPullProgress(context.Background(), create)
	if err != nil || result["status"] != "created" {
		t.Fatalf("create: %v %v", result, err)
	}
	if err := os.Remove(f.Executor.requests().File(create.Request.ID)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	merge.Request.Payload = encode(map[string]any{"number": 1, "head": f.head, "approval": "直接PRをマージまで許可します", "approvalDelegated": true, "approvalRequest": create.Request.ID})
	f.PullOverride = func(p *gitea.Pull) { p.Body = f.Body }
	f.GiteaOverride = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/owner/project/issues/1/comments" {
			return false
		}
		var comment map[string]any
		if err := json.NewDecoder(r.Body).Decode(&comment); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return true
		}
		f.Comments = append(f.Comments, comment)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
		return true
	}
	return f, merge
}

func TestManualMergePreapprovedAtCreate(t *testing.T) {
	f, merge := createApprovedFixture(t)
	result, err := f.Executor.mergeRequestedPull(context.Background(), merge)
	commit, _ := result["mergeCommit"].(string)
	if err != nil || result["status"] != "merged" || commit == "" || f.MergePosts != 1 || len(f.Comments) != 1 {
		t.Fatalf("merge: %v %v posts=%d comments=%v", result, err, f.MergePosts, f.Comments)
	}
	body := f.Comments[0]["body"].(string)
	for _, want := range []string{"直接PRをマージまで許可します", "123-aabbccdd", "権限ファイル: feature.txt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("approval record lacks %q: %s", want, body)
		}
	}
}

func TestManualMergePreapprovalRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]any)
		body  func(string) string
	}{
		{"別の作成依頼", func(p map[string]any) { p["approvalRequest"] = "125-aabbccdd" }, nil},
		{"運用代行でない", func(p map[string]any) { delete(p, "approvalDelegated") }, nil},
		{"短い承認", func(p map[string]any) { p["approval"] = "マージして" }, nil},
		{"承認なし", func(p map[string]any) { delete(p, "approval") }, nil},
		{"印が2つ", nil, func(b string) string { return b + "\n<!-- issue-desk-pr-request:123-aabbccdd -->" }},
		{"印なし", nil, func(string) string { return "feature" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, merge := createApprovedFixture(t)
			var payload map[string]any
			if err := json.Unmarshal(merge.Request.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if tc.alter != nil {
				tc.alter(payload)
				merge.Request.Payload = encode(payload)
			}
			if tc.body != nil {
				f.PullOverride = func(p *gitea.Pull) { p.Body = tc.body(f.Body) }
			}
			result, err := f.Executor.mergeRequestedPull(context.Background(), merge)
			if err != nil || result["status"] != "refused" || f.MergePosts != 0 {
				t.Fatalf("merge: %v %v posts=%d", result, err, f.MergePosts)
			}
		})
	}
}
