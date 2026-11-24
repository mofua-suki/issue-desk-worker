package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func samePull(p gitea.Pull, repo, branch, base, head string) bool {
	return p.Number > 0 && strings.EqualFold(p.Head.Repo.FullName, repo) && strings.EqualFold(p.Base.Repo.FullName, repo) && p.Head.Ref == branch && p.Base.Ref == base && p.Head.SHA == head
}
func (e *Executor) findOpenPull(ctx context.Context, repo, branch, base, head string, includeMerged bool) (int64, error) {
	state := "open"
	if includeMerged {
		state = "all"
	}
	var found int64
	err := e.Gitea.EachPull(ctx, repo, state, 1000, func(p gitea.Pull) (bool, error) {
		if samePull(p, repo, branch, base, head) && (p.State == "open" && !p.Merged || includeMerged && p.Merged && p.MergeCommitSHA == head) {
			found = p.Number
			return true, nil
		}
		if strings.EqualFold(p.Head.Repo.FullName, repo) && strings.EqualFold(p.Base.Repo.FullName, repo) && p.Head.Ref == branch && p.Base.Ref == base && p.Head.SHA != head {
			return false, errors.New("同じ枝のPR先頭が公開したコミットと一致しません")
		}
		return false, nil
	})
	if errors.Is(err, gitea.ErrListIncomplete) {
		return 0, errors.New("開いたPRの一覧を最後まで照合できません")
	}
	return found, err
}

type releaseHold struct {
	reason, summary string
	view            map[string]any
}

func (h *releaseHold) Error() string { return h.summary }

// Re-read both policy and head-bound approvals immediately before a merge, including recovered stages.
func (e *Executor) releaseGate(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, s gitwork.Snapshot) error {
	posted, postErr := e.reviewNotice(ctx, r)
	if postErr != nil || !posted {
		return &releaseHold{"review_held", "独立レビュー投稿の結果を照合中です。自動マージしません", map[string]any{"reviewPostingPending": true, "releasePending": true}}
	}
	snapshot, err := e.API.Config(ctx)
	if err != nil {
		return err
	}
	current, err := DecodeConfig(snapshot, e.Config.Host)
	if err != nil {
		return err
	}
	var latest struct {
		Job struct {
			Descriptor
			Status     string `json:"status"`
			LeaseHost  string `json:"leaseHost"`
			LeaseEpoch int64  `json:"leaseEpoch"`
			HeadSHA    string `json:"headSha"`
		}
	}
	if err := e.API.Call(ctx, "GET", fmt.Sprintf("/api/v1/jobs/%d", d.ID), nil, &latest); err != nil {
		return err
	}
	job := latest.Job
	if d.finalApprovalFixedTest {
		current := job.Descriptor
		current.HeadSHA = job.HeadSHA
		if _, ok := selectFinalApprovalPublication(current); !ok || current.Branch != d.Branch {
			return finalPublicationHold("現在の最終承認に対応する公開続行情報を確認できません")
		}
	}
	if job.ID != d.ID || job.Repo != d.Repo || job.Issue != d.Issue || job.Generation != d.Generation || job.Status != "running" || job.LeaseHost != e.Config.Host || job.LeaseEpoch != r.Epoch || job.PR != r.PR || job.HeadSHA != r.Head {
		return errors.New("現在のジョブ・lease・PR・先頭を確認できないためマージしません")
	}
	checker := &Executor{Config: current}
	repo, err := current.Repository(d.Repo)
	if err != nil {
		return err
	}
	files, err := checker.permissionFiles(s, d.Repo)
	if err != nil {
		return &releaseHold{"review_held", err.Error(), nil}
	}
	if len(files) > 0 && !permissionApprovedContent(ctx, w, job.Descriptor, r.Head, r.PR, files) {
		return &releaseHold{"review_held", "権限の変更は利用者の承認を待っています", map[string]any{"permissionHold": map[string]any{"v": 1, "head": r.Head, "base": r.Base, "pr": r.PR, "files": files}, "releasePending": true}}
	}
	if sqlHold(s) && field[struct {
		Head string `json:"head"`
	}](job.View, "deletionApproved").Head != r.Head {
		return &releaseHold{"deletions", "データ削除SQLの承認を待っています", map[string]any{"deletionApproval": map[string]any{"head": r.Head, "items": s.Paths, "total": len(s.Paths)}, "releasePending": true}}
	}
	if !repo.AutoMerge {
		return &releaseHold{"review_held", "PRを公開しました。手動マージを待っています", map[string]any{"releasePending": true}}
	}
	return nil
}
