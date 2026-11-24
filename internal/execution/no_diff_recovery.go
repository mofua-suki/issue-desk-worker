package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

type closeTimelineEvent struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (e *Executor) closeTimeline(ctx context.Context, d Descriptor) ([]closeTimelineEvent, error) {
	var events []closeTimelineEvent
	seen := map[int64]bool{}
	err := gitea.EachRequired(ctx, e.Gitea, fmt.Sprintf("/repos/%s/issues/%d/timeline", d.Repo, d.Issue), 100, func(event closeTimelineEvent) (bool, error) {
		if event.ID <= 0 || seen[event.ID] || event.Type == "" || ((event.Type == "close" || event.Type == "reopen") && event.User.Login == "") {
			return false, errors.New("不正なtimeline")
		}
		seen[event.ID] = true
		events = append(events, event)
		return false, nil
	})
	return events, err
}

func (e *Executor) validNoDiffEvidence(r *journal.Record, d Descriptor, requestHash string) bool {
	if r == nil || r.ID != d.ID || r.Repo != d.Repo || r.Issue != d.Issue || r.Generation != d.Generation || r.PR != 0 || !gitwork.ValidSHA(r.Head) || !gitwork.ValidSHA(r.Base) {
		return false
	}
	if !reviewBaselineMatches(r, d, requestHash) || r.ReviewBaseline.Head != r.Head || r.ReviewBaseline.Decision != "approved" {
		return false
	}
	data, ok := loadReviewBaseline(e.Config.RuntimeDir, r)
	if !ok || len(data) != 0 {
		return false
	}
	var test TestResult
	return json.Unmarshal(r.Test, &test) == nil && test.Passed && testEvidenceReason(test, r.Head, r.Base) == ""
}

func (x *executionRun) prepareNoDiffClose() error {
	e, r := x.e, x.r
	r.NoDiffClose = nil
	if err := x.fence(x.ctx); err != nil {
		return err
	}
	account := e.Config.Assignments.WorkerAccounts[e.Config.Host]
	if account != "" && e.validNoDiffEvidence(r, x.d, x.requestHash) {
		events, err := e.closeTimeline(x.ctx, x.d)
		if err == nil {
			var max int64
			for _, event := range events {
				if event.ID > max {
					max = event.ID
				}
			}
			r.NoDiffClose = &journal.NoDiffClose{Account: account, TimelineMax: max, Head: r.Head, Base: r.Base, RequestHash: x.requestHash, TargetBase: r.TargetBase}
		}
	}
	if err := x.fence(x.ctx); err != nil {
		return err
	}
	if err := e.Store.Save(r); err != nil {
		return err
	}
	return x.fence(x.ctx)
}

func (e *Executor) resumeNoDiffClose(ctx context.Context, job intake.Job, d Descriptor, r *journal.Record, info gitea.Repository, issue gitea.Issue, fence func(context.Context) error) (intake.State, bool, error) {
	if r == nil || r.NoDiffClose == nil || d.Repo != job.Repo || d.PR != 0 || (d.HeadSHA != "" && d.HeadSHA != r.Head) || job.LeaseEpoch < 1 || r.Epoch < 1 || r.Epoch > job.LeaseEpoch || !info.Permissions.Push || info.Empty || retryBaseNeeded(d, r) {
		return intake.State{}, false, nil
	}
	c := r.NoDiffClose
	account := e.Config.Assignments.WorkerAccounts[e.Config.Host]
	if account == "" || account != c.Account || c.TargetBase != info.DefaultBranch || c.Head != r.Head || c.Base != r.Base || c.RequestHash != reviewRequestHash(d, issue.Title, issue.Body) || !e.validNoDiffEvidence(r, d, c.RequestHash) {
		return intake.State{}, false, nil
	}
	if err := fence(ctx); err != nil {
		return intake.State{}, true, err
	}
	events, err := e.closeTimeline(ctx, d)
	if err != nil {
		return intake.State{}, false, nil
	}
	var closeID int64
	maxPresent := c.TimelineMax == 0
	for _, event := range events {
		if event.ID == c.TimelineMax {
			maxPresent = true
		}
		if event.ID <= c.TimelineMax || event.Type != "close" {
			continue
		}
		if closeID != 0 || event.User.Login != account {
			return intake.State{}, false, nil
		}
		closeID = event.ID
	}
	if closeID == 0 || !maxPresent {
		return intake.State{}, false, nil
	}
	for _, event := range events {
		if event.ID > closeID && event.Type == "reopen" {
			return intake.State{}, false, nil
		}
	}
	if err := fence(ctx); err != nil {
		return intake.State{}, true, err
	}
	r.Epoch = job.LeaseEpoch
	saveExecutionAdmission(r, d)
	trackFailureRecord(ctx, r)
	out, err := e.finishMerged(ctx, r, fence, "独立レビューで既定ブランチが依頼を満たすことを確認しました", nil)
	return out, true, err
}
