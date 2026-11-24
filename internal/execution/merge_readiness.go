package execution

import (
	"context"
	"fmt"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

// Bound both elapsed waiting and read attempts while holding a merge turn.
// false is an unconfirmed mergeability result, not evidence of a conflict.
const mergeReadInterval = time.Second
const mergeReadLimit = 10 * time.Second
const mergeReadRetries = 10

type pullCheck struct {
	Field string `json:"field"`
	Kind  string `json:"kind"`
}

func mergePullChecks(p gitea.Pull, d Descriptor, r *journal.Record, base string) []pullCheck {
	checks := []pullCheck{}
	for _, c := range []struct {
		field string
		ok    bool
	}{
		{"number", p.Number > 0 && p.Number == r.PR},
		{"state", p.State == "open"},
		{"draft", !p.Draft},
		{"merged", !p.Merged},
		{"head.ref", p.Head.Ref == r.Branch},
		{"base.ref", p.Base.Ref == base},
		{"head.sha", p.Head.SHA == r.Head},
		{"head.repo.full_name", strings.EqualFold(p.Head.Repo.FullName, d.Repo)},
		{"base.repo.full_name", strings.EqualFold(p.Base.Repo.FullName, d.Repo)},
	} {
		if !c.ok {
			checks = append(checks, pullCheck{c.field, "mismatch"})
		}
	}
	if !p.Mergeable {
		checks = append(checks, pullCheck{"mergeable", "unconfirmed"})
	}
	return checks
}

func pullCheckHold(checks []pullCheck, phase string, waiting bool) error {
	fields := make([]string, len(checks))
	for i, c := range checks {
		fields[i] = c.Field
	}
	summary := "マージ前のPR照合を保留しました: " + strings.Join(fields, ", ")
	if waiting {
		summary = "mergeableの確認待ちです。待機上限に達したため自動マージを保留しました"
	}
	return &releaseHold{"review_held", summary, map[string]any{
		"releasePending": true,
		"mergePullCheck": map[string]any{"phase": phase, "checks": checks, "waiting": waiting},
	}}
}

// mergeReadiness is the state of one prepareMerge call. The budget bounds
// waits and retry decisions, not individual operations. In particular,
// expiring it must not kill a fetch or merge mid-write, and operation failures
// must retain their original error classification.
type mergeReadiness struct {
	e        *Executor
	ctx      context.Context
	d        Descriptor
	r        *journal.Record
	w        *gitwork.Workspace
	fence    func(context.Context) error
	now      func() time.Time
	snapshot gitwork.Snapshot
	// deadline starts at the first unconfirmed mergeability read.
	deadline  time.Time
	retries   int
	waitPhase string
}

// Every retry goes through the existing ownership, turn, policy, review and
// workspace gates. In particular, a final-read false cannot bypass these gates.
func (e *Executor) prepareMerge(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, fence, renew func(context.Context) error, snapshot gitwork.Snapshot) (bool, bool, error) {
	m := &mergeReadiness{e: e, ctx: ctx, d: d, r: r, w: w, fence: fence, now: e.mergeReadNow, waitPhase: "initial", snapshot: snapshot}
	if m.now == nil {
		m.now = time.Now
	}
	for {
		if err := m.active(); err != nil {
			return false, false, err
		}
		if m.retries > 0 {
			if !m.now().Before(m.deadline) {
				return false, false, pullCheckHold([]pullCheck{{"mergeable", "unconfirmed"}}, m.waitPhase, true)
			}
			if err := renew(ctx); err != nil {
				return false, false, err
			}
		}
		if err := m.fetchTarget(); err != nil {
			return false, false, err
		}
		base, err := w.Base(ctx)
		if err != nil {
			return false, false, err
		}
		if base != r.Base {
			if d.finalApprovalFixedTest {
				return false, false, finalPublicationHold("マージ準備中に既定ブランチが更新されました")
			}
			_, err := w.Follow(ctx)
			return false, err == nil, err
		}
		if changed, err := m.targetChanged(); err != nil || changed {
			return false, changed, err
		}
		pull, err := m.pull("マージ対象のPRを取得できません")
		if err != nil {
			return false, false, err
		}
		if pull.Number == r.PR && pull.Merged && samePull(pull, d.Repo, r.Branch, w.Default, r.Head) && pull.MergeCommitSHA == r.Head {
			return true, false, nil
		}
		if retry, err := m.check(pull, "initial"); err != nil {
			return false, false, err
		} else if retry {
			continue
		}
		if err := e.checkMergeReviews(ctx, d, r); err != nil {
			return false, false, err
		}
		if err := m.treeClean(); err != nil {
			return false, false, err
		}
		r.Stage = "merge"
		if err := e.saveStage(r); err != nil {
			return false, false, err
		}
		pull, err = m.pull("マージ直前のPRを取得できません")
		if err != nil {
			return false, false, err
		}
		if retry, err := m.check(pull, "final"); err != nil {
			return false, false, err
		} else if retry {
			continue
		}
		if err := m.fetchTarget(); err != nil {
			return false, false, err
		}
		if changed, err := m.targetChanged(); err != nil || changed {
			return false, changed, err
		}
		if err := e.checkMergeReviews(ctx, d, r); err != nil {
			return false, false, err
		}
		if err := m.active(); err != nil {
			return false, false, err
		}
		if err := renew(ctx); err != nil {
			return false, false, err
		}
		if err := m.treeClean(); err != nil {
			return false, false, err
		}
		return false, false, nil
	}
}

// active checks the commands, the context and the job's ownership.
func (m *mergeReadiness) active() error {
	if err := m.e.pollCommandsFor(m.ctx, m.d.ID); err != nil {
		return err
	}
	if err := m.ctx.Err(); err != nil {
		return err
	}
	return m.fence(m.ctx)
}

// check reports whether the pull is ready to merge. It holds a mismatch, and
// waits and asks for a retry while only mergeability is unconfirmed, until
// the budget runs out.
func (m *mergeReadiness) check(p gitea.Pull, phase string) (bool, error) {
	if err := m.active(); err != nil {
		return false, err
	}
	checks := mergePullChecks(p, m.d, m.r, m.w.Default)
	if len(checks) == 0 {
		return false, nil
	}
	if len(checks) != 1 || checks[0].Field != "mergeable" {
		return false, pullCheckHold(checks, phase, false)
	}
	m.waitPhase = phase
	if m.deadline.IsZero() {
		m.deadline = m.now().Add(mergeReadLimit)
	}
	remaining := m.deadline.Sub(m.now())
	if remaining <= 0 || m.retries >= mergeReadRetries {
		return false, pullCheckHold(checks, phase, true)
	}
	m.retries++
	wait := m.e.mergeReadWait
	if wait == nil {
		wait = waitTransient
	}
	if err := wait(m.ctx, min(mergeReadInterval, remaining)); err != nil {
		return false, err
	}
	return true, m.ctx.Err()
}

// fetchTarget fetches the branch and, for a final publication, checks the
// published artifacts and tree.
func (m *mergeReadiness) fetchTarget() error {
	ctx, d, w := m.ctx, m.d, m.w
	if err := w.Fetch(ctx); err != nil {
		if d.finalApprovalFixedTest && ctx.Err() == nil {
			return finalPublicationHold("マージ対象の枝を取得できません")
		}
		return err
	}
	if d.finalApprovalFixedTest {
		p, ok := selectFinalApprovalPublication(d)
		if !ok {
			return finalPublicationHold("公開続行情報を確認できません")
		}
		if cause := m.e.checkFinalApprovalPublication(ctx, d, p, w); cause != "" {
			return finalPublicationHold("公開済み成果物を照合できません: " + cause)
		}
		if reason := finalPublicationTreeCause(ctx, w, p.HeadSHA); reason != "" {
			return finalPublicationHold(reason)
		}
	}
	return nil
}

// targetChanged reports whether the head or base moved from the record, which
// sends the tree back to the test stage, and checks the release gate otherwise.
func (m *mergeReadiness) targetChanged() (bool, error) {
	previous := m.snapshot
	if m.d.finalApprovalFixedTest {
		previous = gitwork.Snapshot{}
	}
	snapshot, err := m.w.SnapshotReusing(m.ctx, previous)
	if err != nil {
		return false, err
	}
	if snapshot.Head != m.r.Head || snapshot.Base != m.r.Base {
		if m.d.finalApprovalFixedTest {
			return false, finalPublicationHold("マージ対象または基点が変更されました")
		}
		return true, nil
	}
	m.snapshot = snapshot
	return false, m.e.releaseGate(m.ctx, m.d, m.r, m.w, snapshot)
}

// pull reads the PR. A final publication holds a failed read with hold.
func (m *mergeReadiness) pull(hold string) (gitea.Pull, error) {
	pull, err := m.e.Gitea.Pull(m.ctx, m.d.Repo, m.r.PR)
	if err != nil {
		if m.d.finalApprovalFixedTest && m.ctx.Err() == nil {
			return pull, finalPublicationHold(hold)
		}
		return pull, err
	}
	return pull, nil
}

// treeClean holds a merge whose tree changed during the checks.
func (m *mergeReadiness) treeClean() error {
	if !m.w.Clean(m.ctx) {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		return &releaseHold{"review_held", errReviewTreeChanged.Error(), map[string]any{"releasePending": true}}
	}
	return nil
}

type mergeReview struct {
	State     string `json:"state"`
	Dismissed bool   `json:"dismissed"`
}

func (e *Executor) hasChangesRequested(ctx context.Context, repo string, pull int64) (bool, error) {
	found := false
	err := gitea.EachRequired(ctx, e.Gitea, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pull), 100, func(review mergeReview) (bool, error) {
		if review.State == "REQUEST_CHANGES" && !review.Dismissed {
			found = true
			return true, nil
		}
		return false, nil
	})
	return found, err
}

func (e *Executor) checkMergeReviews(ctx context.Context, d Descriptor, r *journal.Record) error {
	found, err := e.hasChangesRequested(ctx, d.Repo, r.PR)
	if err != nil {
		return err
	}
	if found {
		return &releaseHold{"review_held", errChangesRequested.Error(), map[string]any{"releasePending": true}}
	}
	return nil
}
