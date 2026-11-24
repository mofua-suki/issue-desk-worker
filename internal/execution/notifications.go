package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/gitea"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

type notification struct {
	ProposalID    string `json:"proposalId,omitempty"`
	CommentID     int64  `json:"commentId,omitempty"`
	JobID         int64  `json:"jobId,omitempty"`
	Epoch         int64  `json:"epoch,omitempty"`
	Host          string `json:"host,omitempty"`
	Status        string `json:"status,omitempty"`
	Author        string `json:"author,omitempty"`
	Repo          string `json:"repo"`
	Target        int64  `json:"target"`
	Kind          string `json:"kind"`
	Marker        string `json:"marker"`
	Head          string `json:"head,omitempty"`
	Body          string `json:"body"`
	Stage         string `json:"stage"`
	RefusalReason string `json:"refusalReason,omitempty"`
}

const executionReportMarkerPrefix = "<!-- issue-desk-execution-report:"

func (n notification) isExecutionReport() bool {
	return strings.HasPrefix(n.Marker, executionReportMarkerPrefix)
}

func (e *Executor) notificationFile(n notification) string {
	sum := sha256.Sum256([]byte(n.Repo + "\n" + n.Marker))
	return filepath.Join(e.Config.RuntimeDir, "control", "outbox", hex.EncodeToString(sum[:])+".json")
}
func (e *Executor) saveNotification(n notification) error {
	file := e.notificationFile(n)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	return atomicfile.Write(file, ".notice-", 0600, false, encode(n), nil)
}
func (e *Executor) noticeRoute(n notification) string {
	if n.Kind == "review" {
		return fmt.Sprintf("/repos/%s/pulls/%d/reviews", n.Repo, n.Target)
	}
	return fmt.Sprintf("/repos/%s/issues/%d/comments", n.Repo, n.Target)
}
func (e *Executor) noticeFound(ctx context.Context, n notification) (bool, error) {
	routes := []string{e.noticeRoute(n)}
	if n.Kind == "review" && n.Stage == "attempted" {
		routes = append(routes, fmt.Sprintf("/repos/%s/issues/%d/comments", n.Repo, n.Target))
	}
	type noticeRow struct {
		Body   string `json:"body"`
		Commit string `json:"commit_id"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	check := func(row noticeRow) (bool, error) {
		if !strings.Contains(row.Body, n.Marker) {
			return false, nil
		}
		trusted := row.User.Login != "" && (row.User.Login == n.Author || n.Author == "" && row.User.Login == e.Config.Assignments.WorkerAccounts[e.Config.Host] || row.User.Login == e.Config.GitUser)
		if n.Kind == "repost" {
			trusted = n.Author != "" && row.User.Login == n.Author
		}
		if !trusted {
			if n.Kind == "repost" {
				return false, errRepostMarkerConflict
			}
			return false, errors.New("通知の目印を確認しましたが投稿者が監視アカウントと一致しません")
		}
		if n.Kind == "review" && row.Commit != n.Head && !strings.Contains(row.Body, "対象: "+n.Head) {
			return false, errors.New("レビューの対象コミットが一致しません")
		}
		return true, nil
	}
	for _, route := range routes {
		found := false
		visit := func(row noticeRow) (bool, error) {
			var err error
			found, err = check(row)
			return found, err
		}
		if strings.HasSuffix(route, "/comments") {
			// Issue comments are read in one response, without paging.
			var rows []noticeRow
			if err := e.Gitea.Request(ctx, "GET", route, nil, &rows); err != nil {
				return false, err
			}
			for _, row := range rows {
				if done, err := visit(row); err != nil || done {
					return found, err
				}
			}
			continue
		}
		err := gitea.Each(ctx, e.Gitea, route, 1000, visit)
		if errors.Is(err, gitea.ErrListIncomplete) {
			return false, errors.New("通知履歴を最後まで照合できません")
		}
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func (e *Executor) syncNotification(ctx context.Context, n notification, beforePost ...func() error) (bool, error) {
	return e.syncNotificationWithReportFence(ctx, n, nil, beforePost...)
}

// Persist the attempt before POST. An unknown response is reconciled by marker, never reposted.
func (e *Executor) syncNotificationWithReportFence(ctx context.Context, n notification, reportFence func(context.Context) error, beforePost ...func() error) (bool, error) {
	e.notificationsMu.Lock()
	defer e.notificationsMu.Unlock()
	file := e.notificationFile(n)
	raw, err := os.ReadFile(file)
	if err == nil {
		var saved notification
		if len(raw) > 512<<10 || json.Unmarshal(raw, &saved) != nil || saved.Repo != n.Repo || saved.Target != n.Target || saved.Marker != n.Marker || saved.Head != n.Head || saved.Body != n.Body {
			if n.Kind == "repost" && saved.Kind == "repost" && saved.Repo == n.Repo && saved.Marker == n.Marker {
				return false, e.stopRepostNotice(saved, "saved_repost_mismatch")
			}
			return false, errors.New("保存済み通知と要求が一致しません")
		}
		if (saved.Stage == "prepared" || saved.Stage == "discarded" && !saved.isExecutionReport()) && n.Epoch > saved.Epoch {
			n.Stage = "prepared"
			if err := e.saveNotification(n); err != nil {
				return false, err
			}
		} else {
			n = saved
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	} else {
		n.Stage = "prepared"
		if err := e.saveNotification(n); err != nil {
			return false, err
		}
	}
	report := n.isExecutionReport()
	if n.Stage == "discarded" && !report {
		return false, nil
	}
	if n.Stage == "refused" {
		return false, errRepostRefused
	}
	if n.Stage == "sent" {
		return true, nil
	}
	// The marker also identifies reports already in the outbox. Recovery
	// can reconcile attempts, but posting requires the acquired job's fence.
	reconcileOnly := n.Stage == "attempted" || n.Stage == "discarded"
	if report && reportFence == nil && !reconcileOnly {
		return false, nil
	}
	if n.Kind == "repost" {
		if err := e.validateRepostNotice(n); err != nil {
			return false, e.stopRepostNotice(n, err.Error())
		}
	}
	found, err := e.noticeFound(ctx, n)
	if err != nil {
		if errors.Is(err, errRepostMarkerConflict) {
			return false, e.stopRepostNotice(n, "marker_author")
		}
		return false, err
	}
	if found {
		n.Stage = "sent"
		return true, e.saveNotification(n)
	}
	if reconcileOnly {
		return false, nil
	}
	if n.JobID > 0 && n.Epoch > 0 {
		var current struct {
			Job struct {
				ID         int64  `json:"id"`
				LeaseEpoch int64  `json:"leaseEpoch"`
				LeaseHost  string `json:"leaseHost"`
				Status     string `json:"status"`
			}
		}
		if err := e.API.Call(ctx, "GET", fmt.Sprintf("/api/v1/jobs/%d", n.JobID), nil, &current); err != nil {
			return false, err
		}
		owned := current.Job.ID == n.JobID && current.Job.LeaseEpoch == n.Epoch && (current.Job.Status == "running" && current.Job.LeaseHost == n.Host || n.Status != "" && current.Job.Status == n.Status)
		if !owned {
			if report {
				return false, nil
			}
			n.Stage = "discarded"
			return false, e.saveNotification(n)
		}
	}
	if reportFence != nil {
		if err := reportFence(ctx); err != nil {
			return false, err
		}
	}
	n.Stage = "attempted"
	if err := e.saveNotification(n); err != nil {
		return false, err
	}
	for _, save := range beforePost {
		if err := save(); err != nil {
			return false, err
		}
	}
	body := map[string]any{"body": n.Body}
	if n.Kind == "review" {
		body["event"] = "COMMENT"
		body["commit_id"] = n.Head
	}
	var posted struct {
		ID int64 `json:"id"`
	}
	postErr := e.Gitea.Request(ctx, "POST", e.noticeRoute(n), body, &posted)
	if rejectedPost(postErr) {
		n.Stage = "prepared"
		if n.Kind == "repost" {
			return false, e.stopRepostNotice(n, "post_4xx")
		}
		if err := e.saveNotification(n); err != nil {
			return false, err
		}
		return false, postErr
	}
	if postErr == nil && posted.ID > 0 {
		n.Stage = "sent"
		return true, e.saveNotification(n)
	}
	found, err = e.noticeFound(ctx, n)
	if err != nil {
		if errors.Is(err, errRepostMarkerConflict) {
			return false, e.stopRepostNotice(n, "marker_author")
		}
		return false, err
	}
	if found {
		n.Stage = "sent"
		return true, e.saveNotification(n)
	}
	return false, nil
}
func (e *Executor) issueNotice(ctx context.Context, r *journal.Record, event, body string) (bool, error) {
	marker := fmt.Sprintf("<!-- issue-desk-notice:%d:%d:%d:%s:%s -->", r.ID, r.Generation, r.Epoch, e.Config.Host, event)
	status := ""
	if strings.HasPrefix(event, "result-") {
		status = strings.SplitN(strings.TrimPrefix(event, "result-"), "-", 2)[0]
	}
	return e.syncNotification(ctx, notification{Status: status, JobID: r.ID, Epoch: r.Epoch, Host: e.Config.Host, Author: e.Config.Assignments.WorkerAccounts[e.Config.Host], Repo: r.Repo, Target: r.Issue, Kind: "comment", Marker: marker, Body: clip(e.redact(body), 55000) + "\n\n" + marker})
}
func (e *Executor) reviewNotice(ctx context.Context, r *journal.Record) (bool, error) {
	if r.PR < 1 || !hasJSON(r.Review) {
		return true, nil
	}
	var review Review
	if json.Unmarshal(r.Review, &review) != nil {
		return false, errors.New("独立レビューを読めません")
	}
	sum := sha256.Sum256(r.Review)
	marker := fmt.Sprintf("<!-- issue-desk-review:%s:%s:%s -->", r.Head, r.Base, hex.EncodeToString(sum[:8]))
	decision := "保留"
	if review.Approved() {
		decision = "承認"
	}
	body := fmt.Sprintf("自動レビュー: %s\n\n%s\n\n破壊的変更: %t\n指摘:\n%s\n利用者の判断:\n%s\nAIや追加確認で解消可能:\n%s", decision, review.Summary, review.Breaking, strings.Join(review.Findings, "\n"), strings.Join(append(review.UserJudgments, review.Uncertainties...), "\n"), strings.Join(review.ResolvableUncertainties, "\n"))
	suffix := fmt.Sprintf("\n\n対象: %s\n基点: %s\n%s", r.Head, r.Base, marker)
	if r.ReviewSkipped && r.ReviewBaseline != nil {
		suffix = fmt.Sprintf("\n\n同じ差分の承認を引き継ぎました（元の対象: %s）", r.ReviewBaseline.Head) + suffix
	}
	body = clip(e.redact(body), 55000) + suffix
	return e.syncNotification(ctx, notification{JobID: r.ID, Epoch: r.Epoch, Host: e.Config.Host, Author: e.Config.Assignments.WorkerAccounts[e.Config.Host], Repo: r.Repo, Target: r.PR, Kind: "review", Marker: marker, Head: r.Head, Body: body})
}
func (e *Executor) recoverNotifications(ctx context.Context) error {
	dir := filepath.Join(e.Config.RuntimeDir, "control", "outbox")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// One notice that cannot be sent must not keep the others unsent; each
	// failure is reported and the rest of the outbox is still recovered.
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			failures = append(failures, fmt.Errorf("通知outbox（%s）を読めません: %w", entry.Name(), err))
			continue
		}
		var n notification
		if len(raw) > 512<<10 || json.Unmarshal(raw, &n) != nil || e.notificationFile(n) != filepath.Join(dir, entry.Name()) {
			// Set an unreadable record aside, keeping its bytes, so it does not
			// fail every later recovery.
			path := filepath.Join(dir, entry.Name())
			if err := os.Rename(path, path+".broken"); err != nil {
				failures = append(failures, fmt.Errorf("通知outboxの不正な記録（%s）を退避できません: %w", entry.Name(), err))
				continue
			}
			fmt.Fprintf(os.Stderr, "通知outboxの記録が不正なため%s.brokenへ退避しました\n", entry.Name())
			continue
		}
		if n.Stage == "sent" || n.Stage == "refused" || n.Stage == "discarded" && !n.isExecutionReport() {
			continue
		}
		if _, err := e.syncNotification(ctx, n); err != nil && n.Kind != "repost" && n.Kind != "followup" {
			if !e.targetGone(ctx, n, err) {
				failures = append(failures, fmt.Errorf("通知（%s %s#%d）を送れません: %w", n.Kind, n.Repo, n.Target, err))
				continue
			}
			// A permanent GET error cannot establish whether a report POST succeeded.
			if n.isExecutionReport() {
				continue
			}
			// The target no longer accepts this notice, so retrying cannot
			// succeed. Keep the record as discarded instead of blocking startup
			// and request processing on every cycle.
			if err := e.discardNotification(n); err != nil {
				failures = append(failures, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "通知（%s %s#%d）は送り先が受け付けないため破棄しました: %v\n", n.Kind, n.Repo, n.Target, err)
		}
	}
	e.commands.pollMu.Lock()
	defer e.commands.pollMu.Unlock()
	if err := e.recoverRepostResults(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// targetGone is a Gitea refusal that does not change on retry: the target is
// gone (410), rejects this notice (422), or is missing (404) from a repository
// this account can still read. Gitea also answers 404 for a repository the
// account lost access to, so that case, 401, 403 and 429 stay retryable.
func (e *Executor) targetGone(ctx context.Context, n notification, err error) bool {
	var refusal *gitea.Error
	if !errors.As(err, &refusal) {
		return false
	}
	switch refusal.Status {
	case 410, 422:
		return true
	case 404:
		var repo map[string]any
		return e.Gitea.Request(ctx, "GET", "/repos/"+n.Repo, nil, &repo) == nil
	}
	return false
}

func (e *Executor) discardNotification(n notification) error {
	e.notificationsMu.Lock()
	defer e.notificationsMu.Unlock()
	file := e.notificationFile(n)
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("通知outbox（%s）を読み直せません: %w", filepath.Base(file), err)
	}
	var saved notification
	if json.Unmarshal(raw, &saved) != nil {
		return fmt.Errorf("通知outboxの記録が不正です（%s）", filepath.Base(file))
	}
	// Only a notice still waiting to be sent is discarded; a final stage
	// reached meanwhile is kept.
	if saved.Stage != "prepared" && saved.Stage != "attempted" {
		return nil
	}
	saved.Stage = "discarded"
	return e.saveNotification(saved)
}
func (e *Executor) resultNotice(ctx context.Context, r *journal.Record, epoch int64, state intake.State) (intake.State, error) {
	var result map[string]json.RawMessage
	if json.Unmarshal(state.Result, &result) != nil || result == nil {
		return state, nil
	}
	var summary string
	_ = json.Unmarshal(result["summary"], &summary)
	sum := sha256.Sum256([]byte(summary))
	event := fmt.Sprintf("result-%s-%s-%x", state.Status, state.HoldReason, sum[:8])
	complete, notifyErr := e.issueNotice(ctx, r, event, "作業結果: "+state.Status+"\n\n"+summary)
	pending := !complete || notifyErr != nil
	_, hadPending := result["notificationPending"]
	if pending {
		result["notificationPending"] = json.RawMessage("true")
	} else if hadPending {
		delete(result, "notificationPending")
	}
	if pending || hadPending {
		state.Result = encode(result)
	}
	// A reused merge report follows the new lease. The notice keeps the
	// original epoch in its marker across retries.
	carriedMerge := state.Status == "merged" && epoch != r.Epoch
	if pending || hadPending || carriedMerge {
		if err := boundFinalResult(epoch, &state); err != nil {
			return intake.State{}, err
		}
		r.FinalState = encode(state)
		if carriedMerge {
			r.FinalReportEpoch = epoch
			r.FinalReported = false
		}
		if err := e.Store.Save(r); err != nil {
			return state, err
		}
	}
	return state, nil
}
