package execution

import (
	"context"
	"errors"
	"issue-desk-worker/internal/gitea"
	"testing"
	"time"

	"issue-desk-worker/internal/journal"
)

// 送れない通知が残っていても、起動の復旧と依頼の処理は止めない。通知の失敗は依頼の周期の結果として示す。
func TestUnsentNoticeDoesNotBlockStartOrRequests(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	f.NoticePostStatus = 403
	if _, err := f.Executor.issueNotice(context.Background(), r, "later", "通常通知"); err == nil {
		t.Fatal("拒否が伝わりません")
	}
	f.Executor.Config.Assignments.Implementers = map[string][]string{"owner/project": {f.Executor.Config.Host}}
	if err := f.Executor.Recover(context.Background()); err != nil {
		t.Fatalf("送れない通知1件で起動を止めました: %v", err)
	}
	err := f.Executor.ProcessRequestsFor(context.Background(), []string{"owner/project"})
	var refusal *gitea.Error
	if !errors.As(err, &refusal) || refusal.Status != 403 {
		t.Fatalf("通知の失敗が分かりません: %v", err)
	}
	if len(f.RequestKinds) == 0 {
		t.Fatal("送れない通知1件で依頼を受け取りませんでした")
	}
	// 拒否が解ければ、次の周期で送り、失敗は消える。
	f.NoticePostStatus = 0
	posts := f.NoticePosts
	f.Executor.requestsNext = time.Time{}
	if err := f.Executor.ProcessRequestsFor(context.Background(), []string{"owner/project"}); err != nil || f.NoticePosts != posts+1 {
		t.Fatalf("次の周期で送っていません: %d→%d %v", posts, f.NoticePosts, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Executor.ProcessRequestsFor(cancelled, []string{"owner/project"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取り消しで戻りません: %v", err)
	}
}
