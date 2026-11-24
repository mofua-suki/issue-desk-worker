package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

func outboxStages(t *testing.T, e *Executor) []string {
	t.Helper()
	dir := filepath.Join(e.Config.RuntimeDir, "control", "outbox")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stages := []string{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var n notification
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		stages = append(stages, n.Stage)
	}
	return stages
}

// 送り先が受け付けない通知（読めるリポジトリでの404・410・422）は、復旧を毎回止めず、破棄として残す。
func TestRecoveryDiscardsNoticeTheTargetRejects(t *testing.T) {
	for _, status := range []int{404, 410, 422} {
		f := newPipeline(t, "approve")
		r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
		f.NoticePostStatus = status
		if done, err := f.Executor.issueNotice(context.Background(), r, "gone", "通常通知"); err == nil || done {
			t.Fatalf("%d: 拒否が伝わりません", status)
		}
		if err := f.Executor.recoverNotifications(context.Background()); err != nil {
			t.Fatalf("%d: 送れない通知1件で復旧が止まりました: %v", status, err)
		}
		posts := f.NoticePosts
		if err := f.Executor.recoverNotifications(context.Background()); err != nil || f.NoticePosts != posts {
			t.Fatalf("%d: 破棄した通知を送り直しました: %d→%d %v", status, posts, f.NoticePosts, err)
		}
		if got := strings.Join(outboxStages(t, f.Executor), ","); got != "discarded" {
			t.Fatalf("%d: 破棄として残っていません: %s", status, got)
		}
	}
}

// 後で解ける可能性のある拒否（403）は、破棄せず再試行の対象に残す。
func TestRecoveryKeepsRetryableRefusal(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	f.NoticePostStatus = 403
	if _, err := f.Executor.issueNotice(context.Background(), r, "later", "通常通知"); err == nil {
		t.Fatal("拒否が伝わりません")
	}
	if err := f.Executor.recoverNotifications(context.Background()); err == nil {
		t.Fatal("再試行する失敗を隠しました")
	}
	if got := strings.Join(outboxStages(t, f.Executor), ","); got != "prepared" {
		t.Fatalf("再試行の対象に残っていません: %s", got)
	}
}

// 読めない記録は退避し、ほかの通知は復旧する。
func TestRecoveryContinuesPastOneBrokenNotice(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	f.NoticePostStatus = 403
	if _, err := f.Executor.issueNotice(context.Background(), r, "later", "通常通知"); err == nil {
		t.Fatal("拒否が伝わりません")
	}
	f.NoticePostStatus = 0
	dir := filepath.Join(f.Executor.Config.RuntimeDir, "control", "outbox")
	broken := filepath.Join(dir, "00.json")
	if err := os.WriteFile(broken, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	posts := f.NoticePosts
	if err := f.Executor.recoverNotifications(context.Background()); err != nil {
		t.Fatalf("壊れた記録1件で復旧が止まりました: %v", err)
	}
	if f.NoticePosts != posts+1 {
		t.Fatalf("ほかの通知を送っていません: %d→%d", posts, f.NoticePosts)
	}
	if raw, err := os.ReadFile(broken + ".broken"); err != nil || string(raw) != "{broken" {
		t.Fatalf("壊れた記録を残していません: %q %v", raw, err)
	}
}
