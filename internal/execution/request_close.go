package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"regexp"
	"strconv"
	"strings"
)

var issueBranchPattern = regexp.MustCompile(`^codex/issue-(\d+)(?:-|$)`)

func (e *Executor) closeRequestedPull(ctx context.Context, r monitorRequest) (map[string]any, error) {
	var p struct {
		Number int64 `json:"number"`
		Issue  int64 `json:"issue"`
	}
	if json.Unmarshal(r.Payload, &p) != nil || p.Number < 1 || p.Issue < 1 {
		return refusal("PRクローズ依頼が不正です"), nil
	}
	worker := struct {
		ID int64 `json:"id"`
	}{}
	if err := e.Gitea.Request(ctx, "GET", "/user", nil, &worker); err != nil {
		return nil, err
	}
	if worker.ID < 1 {
		return nil, errors.New("監視用アカウントを確認できません")
	}
	marker := "<!-- issue-desk-pr-close:" + r.ID + ":"
	{
		var comments []struct {
			Body string `json:"body"`
			User struct {
				ID int64 `json:"id"`
			} `json:"user"`
		}
		if err := e.Gitea.Request(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d/comments", r.Repo, p.Issue), nil, &comments); err != nil {
			return nil, err
		}
		for _, c := range comments {
			if c.User.ID == worker.ID {
				for _, status := range []string{"closed", "skipped", "refused", "failed"} {
					if strings.Contains(c.Body, marker+status+" -->") {
						return map[string]any{"status": status, "number": p.Number}, nil
					}
				}
			}
		}
	}
	pull, err := e.Gitea.Pull(ctx, r.Repo, p.Number)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"status": "skipped", "number": p.Number}
	if !pull.Merged && pull.State != "closed" {
		issueBranch := issueBranchPattern.FindStringSubmatch(pull.Head.Ref)
		matches := len(issueBranch) > 1 && issueBranch[1] == strconv.FormatInt(p.Issue, 10)
		if pull.State != "open" || pull.Head.Repo.FullName != r.Repo || !matches {
			result = refusal("同じリポジトリの対象Issueの作業枝ではありません")
		} else {
			issue, err := e.Gitea.Issue(ctx, r.Repo, p.Issue)
			if err != nil {
				return nil, err
			}
			if issue.State != "closed" || hasJSON(issue.PullRequest) {
				result = refusal("対象Issueが閉じていません")
			} else {
				var edited gitea.Pull
				if err := e.Gitea.Request(ctx, "PATCH", fmt.Sprintf("/repos/%s/pulls/%d", r.Repo, p.Number), map[string]string{"state": "closed"}, &edited); err != nil {
					return nil, err
				}
				if edited.State != "closed" {
					return nil, errors.New("PRのクローズ結果が不正です")
				}
				result["status"] = "closed"
			}
		}
	}
	text := fmt.Sprintf("担当WorkerのPRクローズ結果: %s\n\n%s%s -->\n<!-- gcr:issue-desk-close -->", result["status"], marker, result["status"])
	if err := e.Gitea.Comment(ctx, r.Repo, p.Issue, text); err != nil {
		return nil, err
	}
	return result, nil
}
