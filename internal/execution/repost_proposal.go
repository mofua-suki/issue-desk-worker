package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Sources are the requester-supplied command contract (issue #78) and the
// existing stop/instruction implementation. Unknown kinds intentionally have no ack.
var commandKinds = map[string]struct {
	Repost bool
	Source string
}{
	"stop":            {Source: "commands.go: existing lease-scoped stop contract"},
	"instruction":     {Source: "commands.go/instructions.go: existing durable instruction contract"},
	"repost_proposal": {Repost: true, Source: "issue #78: requester-supplied commands/ack contract"},
}
var errRepostRefused = errors.New("repost_post_refused")

func positiveCommandNumber(raw json.RawMessage) (int64, bool) {
	return proposalInteger(raw, 1, 9223372036854775807)
}
func (e *Executor) repostAssignment(repo, host, author string) string {
	if host == "" || host != e.Config.Host {
		return "target_host"
	}
	if !gitea.ValidRepo(repo) || !slices.Contains(e.requestRepos(), repo) {
		return "repository_not_assigned"
	}
	if author == "" || strings.TrimSpace(author) == "" {
		return "worker_account_missing"
	}
	if e.Config.GitUser == "" || strings.EqualFold(author, e.Config.GitUser) {
		return "worker_account_is_git_user"
	}
	if author != e.Config.Assignments.WorkerAccounts[e.Config.Host] {
		return "worker_account_changed"
	}
	return ""
}
func (e *Executor) repostProposal(ctx context.Context, cmd command) error {
	saved, err := e.loadRepostResult(cmd)
	if errors.Is(err, errRepostResultMismatch) {
		return e.refuseMismatchedRepost(ctx, cmd, saved)
	}
	if err != nil {
		return err
	}
	if saved != nil {
		return e.finishRepostResult(ctx, saved)
	}
	receipt := &repostResult{Command: cmd}
	refuse := func(reason string) error {
		receipt.Status = "failed"
		receipt.Result = repostOutcome{Result: "refused", Reason: reason}
		if err := e.saveRepostResult(receipt); err != nil {
			return err
		}
		return e.finishRepostResult(ctx, receipt)
	}
	author := e.Config.Assignments.WorkerAccounts[e.Config.Host]
	if reason := e.repostAssignment(cmd.Repo, cmd.TargetHost, author); reason != "" {
		return refuse(reason)
	}
	// The proposal source grammar limits issue numbers to the JSON safe range;
	// comment IDs are independent Gitea int64 identifiers.
	issue, ok := proposalInteger(cmd.Issue, 1, 9007199254740991)
	if !ok {
		return refuse("issue_number")
	}
	if _, valid := proposalInteger(cmd.Payload.V, 1, 1); !valid {
		return refuse("payload_version")
	}
	commentID, ok := positiveCommandNumber(cmd.Payload.CommentID)
	if !ok {
		return refuse("comment_id")
	}
	id, ok := proposalString(cmd.Payload.ProposalID, 128)
	if !ok || id == "" {
		return refuse("proposal_id")
	}
	marker := fmt.Sprintf("<!-- issue-desk-repost:%d -->", commentID)
	n := notification{Host: cmd.TargetHost, Author: author, Repo: cmd.Repo, Target: issue, Kind: "repost", Marker: marker, ProposalID: id, CommentID: commentID}
	// A different command ID for the same original comment reuses its durable body
	// and send state, including an ambiguous POST or a permanent refusal.
	raw, readErr := os.ReadFile(e.notificationFile(n))
	if readErr == nil {
		var prior notification
		if json.Unmarshal(raw, &prior) != nil || prior.Kind != n.Kind || prior.Repo != n.Repo || prior.Target != n.Target || prior.Marker != n.Marker || prior.ProposalID != id || prior.CommentID != commentID || prior.Author != author || prior.Host != cmd.TargetHost {
			return refuse("saved_repost_mismatch")
		}
		n = prior
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else {
		var comment struct {
			ID             int64  `json:"id"`
			Body           string `json:"body"`
			IssueURL       string `json:"issue_url"`
			PullRequestURL string `json:"pull_request_url"`
			User           struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := e.Gitea.Request(ctx, "GET", fmt.Sprintf("/repos/%s/issues/comments/%d", cmd.Repo, commentID), nil, &comment); err != nil {
			var refusal *gitea.Error
			if errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
				return refuse("comment_read_4xx")
			}
			return err // Transport errors and 5xx leave the command unacked.
		}
		commentURL := comment.IssueURL
		if commentURL == "" {
			commentURL = comment.PullRequestURL
		}
		if comment.ID != commentID || !repostCommentIssue(commentURL, cmd.Repo, issue) {
			return refuse("comment_issue")
		}
		if !strings.EqualFold(comment.User.Login, e.Config.GitUser) {
			return refuse("comment_author")
		}
		proposal, err := parseProposalComment(comment.Body)
		if err != nil {
			return refuse(err.Error())
		}
		if proposal.ID != id {
			return refuse("proposal_id_mismatch")
		}
		if proposal.Source != fmt.Sprintf("%s#%d", cmd.Repo, issue) {
			return refuse("proposal_source_mismatch")
		}
		n.Body = repostBody(comment.Body, commentID)
		if e.containsCredential(n.Body) {
			return refuse("body_redacted")
		}
		if utf16Length(n.Body) > 55000 {
			return refuse("body_too_long")
		}
		if _, err := parseProposalComment(n.Body); err != nil {
			return refuse("final_" + err.Error())
		}
		n.Stage = "prepared"
		if err := e.saveNotification(n); err != nil {
			return err
		}
	}
	receipt.Notice = &n
	receipt.Status = "done"
	receipt.Result = repostOutcome{Result: "queued", ProposalID: id, CommentID: commentID}
	if err := e.saveRepostResult(receipt); err != nil {
		return err
	}
	return e.finishRepostResult(ctx, receipt)
}

// The startup check binds the Gitea client to WorkerAccounts[Host]. Recheck the
// assignment before each prepared send, including recovery, without reading tokens.
func (e *Executor) validateRepostNotice(n notification) error {
	if reason := e.repostAssignment(n.Repo, n.Host, n.Author); reason != "" {
		return errors.New(reason)
	}
	if e.containsCredential(n.Body) || utf16Length(n.Body) > 55000 {
		return errors.New("saved_repost_body_unsafe")
	}
	proposal, err := parseProposalComment(n.Body)
	if err != nil {
		return err
	}
	if proposal.ID != n.ProposalID || proposal.Source != fmt.Sprintf("%s#%d", n.Repo, n.Target) || n.Marker != fmt.Sprintf("<!-- issue-desk-repost:%d -->", n.CommentID) {
		return errors.New("saved_repost_identity")
	}
	return nil
}

func (e *Executor) ackRepostResult(ctx context.Context, r *repostResult) error {
	if r.AckRejected != 0 {
		return nil // Persisted permanent refusal, not a successful delivery.
	}
	response, err := e.API.Explicit(ctx, "POST", "/api/v1/commands/"+url.PathEscape(r.Command.ID)+"/ack", encode(map[string]any{"status": r.Status, "result": r.Result}))
	if err != nil {
		return err
	}
	// Authentication changes, timeouts and rate limits can recover. Keep the
	// saved result pending so polling and startup recovery can retry its ack.
	switch response.Status {
	case 401, 403, 408, 429:
		return fmt.Errorf("再投稿指示のackが拒否されました: HTTP %d", response.Status)
	}
	// A finished or permanently rejected command must not block startup or
	// cause an endless ack loop. Keep rejection distinct from acknowledgement.
	if response.Status >= 400 && response.Status < 500 && response.Status != 409 {
		r.AckRejected = response.Status
		return e.saveRepostResult(r)
	}
	if response.Status != 409 && (response.Status < 200 || response.Status >= 300) {
		return fmt.Errorf("再投稿指示のackが拒否されました: HTTP %d", response.Status)
	}
	r.AckStatus = r.Status
	return e.saveRepostResult(r)
}

// Gitea generates issue_url or pull_request_url using ROOT_URL, which can
// differ from the Worker's API address and include a subpath. Match the tail
// of either the HTML or API path, without relying on its prefix or host.
func repostCommentIssue(raw, repo string, issue int64) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 5 {
		return false
	}
	tail := parts[len(parts)-4:]
	n, err := strconv.ParseInt(tail[3], 10, 64)
	return err == nil && n == issue && (tail[2] == "issues" || tail[2] == "pulls") && strings.EqualFold(tail[0]+"/"+tail[1], repo)
}
