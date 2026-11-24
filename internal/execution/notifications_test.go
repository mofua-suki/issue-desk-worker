package execution

import (
	"context"
	"issue-desk-worker/internal/journal"
	"strings"
	"testing"
)

func TestUnknownIssueNoticeIsReconciledWithoutReposting(t *testing.T) {
	f := newPipeline(t, "approve")
	f.DropNoticeResponse = true
	f.HideNotices = true
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	for i := 0; i < 2; i++ {
		done, err := f.Executor.issueNotice(context.Background(), r, "test", "確認事項")
		if err != nil || done {
			t.Fatalf("未知の応答: %t %v", done, err)
		}
	}
	if f.NoticePosts != 1 {
		t.Fatalf("再投稿しました: %d", f.NoticePosts)
	}
	f.HideNotices = false
	if err := f.Executor.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, err := f.Executor.issueNotice(context.Background(), r, "test", "確認事項")
	if err != nil || !done || f.NoticePosts != 1 {
		t.Fatalf("目印の回収: %t %v", done, err)
	}
}
func TestNoticeMarkerRequiresTrustedAuthor(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Comments = []map[string]any{{"body": "<!-- issue-desk-notice:1:0:0:fixture:test -->", "user": map[string]any{"login": "other"}}}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	if _, err := f.Executor.issueNotice(context.Background(), r, "test", "確認事項"); err == nil || f.NoticePosts != 0 {
		t.Fatal("別投稿者の目印を信頼しました")
	}
}
func TestPipelinePostsStartResultAndCommentReview(t *testing.T) {
	f := newPipeline(t, "approve")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("実行: %s %v", state.Status, err)
	}
	if len(f.Comments) != 2 || len(f.Reviews) != 1 {
		t.Fatalf("通知/レビューが不足: comments=%d reviews=%d", len(f.Comments), len(f.Reviews))
	}
	review := f.Reviews[0]
	if review["event"] != "COMMENT" || review["commit_id"] != state.HeadSHA {
		t.Fatal("独立レビューの対象・COMMENT方式が違います")
	}
	before := f.NoticePosts
	_, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || f.NoticePosts != before {
		t.Fatalf("再起動で通知を再投稿しました: %v", err)
	}
}
func TestCompleteUnpagedCommentHistoryDoesNotLoop(t *testing.T) {
	f := newPipeline(t, "approve")
	for i := 0; i < 125; i++ {
		f.Comments = append(f.Comments, map[string]any{"body": "過去の通知", "user": map[string]any{"login": "worker"}})
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	done, err := f.Executor.issueNotice(context.Background(), r, "new", "新しい通知")
	if err != nil || !done || f.NoticePosts != 1 {
		t.Fatalf("全件commentsが止まりました: %t %v", done, err)
	}
}
func TestLongReviewRetainsMarkerAfterUnknownPost(t *testing.T) {
	f := newPipeline(t, "approve")
	f.DropNoticeResponse = true
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Review: encode(Review{Decision: "approve", Summary: strings.Repeat("長", 61000)})}
	done, err := f.Executor.reviewNotice(context.Background(), r)
	if err != nil || !done || f.NoticePosts != 1 {
		t.Fatalf("長文markerを回収できません: %t %v", done, err)
	}
	body, _ := f.Reviews[0]["body"].(string)
	if !strings.HasSuffix(body, " -->") || !strings.Contains(body, "対象: "+r.Head) {
		t.Fatal("末尾の対象/markerが欠けました")
	}
	if done, err := f.Executor.reviewNotice(context.Background(), r); err != nil || !done || f.NoticePosts != 1 {
		t.Fatal("同reviewを再POSTしました")
	}
}
func TestDefiniteNoticePostRefusalCanBeRetriedButUnknownCannot(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	f.NoticePostStatus = 403
	if done, err := f.Executor.issueNotice(context.Background(), r, "retry", "通知"); err == nil || done {
		t.Fatal("POST拒否を成功扱いしました")
	}
	f.NoticePostStatus = 0
	if done, err := f.Executor.issueNotice(context.Background(), r, "retry", "通知"); err != nil || !done || f.NoticePosts != 2 {
		t.Fatal("明確拒否後に条件修正しても投稿できません")
	}
	f.DropNoticeResponse = true
	f.HideNotices = true
	_, _ = f.Executor.issueNotice(context.Background(), r, "unknown", "通知")
	posts := f.NoticePosts
	f.NoticeReadStatus = 403
	_, _ = f.Executor.issueNotice(context.Background(), r, "unknown", "通知")
	f.NoticeReadStatus = 0
	_, _ = f.Executor.issueNotice(context.Background(), r, "unknown", "通知")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = f.Executor.issueNotice(ctx, r, "unknown", "通知")
	if f.NoticePosts != posts {
		t.Fatal("GET失敗/打切りで未知POSTを解除しました")
	}
}

func TestLostLeaseDropsPreparedNoticeBeforePosting(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Epoch: 3}
	f.NoticeReadStatus = 403
	_, _ = f.Executor.issueNotice(context.Background(), r, "lease", "開始")
	f.NoticeReadStatus = 0
	f.CurrentLeaseEpoch = 4
	if done, err := f.Executor.issueNotice(context.Background(), r, "lease", "開始"); err != nil || done || f.NoticePosts != 0 {
		t.Fatal("lease喪失後の未送信通知を投稿しました")
	}
	if err := f.Executor.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.NoticePosts != 0 {
		t.Fatal("再起動後に旧leaseの未送信通知を復活しました")
	}
}

func TestReviewNoticeIncludesBothUncertaintyClasses(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Review: encode(Review{Decision: "hold", UserJudgments: []string{"判断事項"}, ResolvableUncertainties: []string{"追加確認"}, Uncertainties: []string{"旧事項"}})}
	if done, err := f.Executor.reviewNotice(context.Background(), r); err != nil || !done {
		t.Fatal(done, err)
	}
	body, _ := f.Reviews[0]["body"].(string)
	for _, s := range []string{"利用者の判断:", "判断事項", "旧事項", "AIや追加確認で解消可能:", "追加確認"} {
		if !strings.Contains(body, s) {
			t.Fatal(body)
		}
	}
}

func TestOrdinaryNotice4xxCanBeRetriedByRecovery(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	f.NoticePostStatus = 403
	if done, err := f.Executor.issueNotice(context.Background(), r, "recover-4xx", "通常通知"); err == nil || done {
		t.Fatal("refusal was not reported")
	}
	f.NoticePostStatus = 0
	if err := f.Executor.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.NoticePosts != 2 {
		t.Fatalf("ordinary notification did not retry: %d", f.NoticePosts)
	}
	if done, err := f.Executor.issueNotice(context.Background(), r, "recover-4xx", "通常通知"); err != nil || !done || f.NoticePosts != 2 {
		t.Fatal("recovered ordinary notice was reposted", err)
	}
}
