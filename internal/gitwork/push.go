package gitwork

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/process"
)

// PushError contains only a locally authored fixed reason, never command output.
type PushError struct{ reason string }

func (e *PushError) Error() string { return e.reason }

const (
	PushBranchReason     = "pushが拒否されました。リモートの枝が先行または分岐している可能性があります"
	PushAuthReason       = "pushの認証または認可が拒否されました"
	PushHookReason       = "pushがリモートのフックに拒否されました"
	PushConnectionReason = "pushの接続に失敗しました"
	PushOtherReason      = "pushに失敗しました"
)

func pushReason(output string) string {
	s := strings.ToLower(output)
	contains := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(s, part) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("non-fast-forward", "fetch first"):
		return PushBranchReason
	case contains("pre-receive hook declined", "update hook declined", "hook declined", "hook rejected") || (contains("remote rejected") && contains("hook")):
		return PushHookReason
	case contains("authentication failed", "authorization failed", "permission denied", "access denied", "http 401", "http 403", "error: 401", "error: 403", "returned error: 401", "returned error: 403", "could not read username"):
		return PushAuthReason
	case contains("could not resolve host", "could not resolve hostname", "failed to connect", "connection refused", "connection timed out", "connection reset", "network is unreachable", "unable to access", "remote end hung up"):
		return PushConnectionReason
	default:
		return PushOtherReason
	}
}

func (w *Workspace) Push(ctx context.Context) error {
	return w.push(ctx, w.Config.Run)
}

func (w *Workspace) push(ctx context.Context, run func(context.Context, string, ...string) (process.Result, error)) error {
	out, err := run(ctx, w.Tree, "push", "origin", "HEAD:refs/heads/"+w.Branch)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil {
		return err
	}
	// A successful Run is authoritative even if ctx is canceled afterwards.
	if out.Code != 0 {
		return &PushError{reason: pushReason(out.Out + "\n" + out.Err)}
	}
	return nil
}

var deletePushHTTPStatus = regexp.MustCompile(`(?m)^(?:fatal: unable to access .*: The requested URL returned error: |error: RPC failed; HTTP )([0-9]{3})(?:[ \t]|$)`)

// DeleteRemoteBranchAt deletes one remote branch only at the confirmed SHA.
func DeleteRemoteBranchAt(ctx context.Context, c Config, repo, branch, expected string) error {
	if !gitea.ValidRepo(repo) || !ValidBranchShape(branch) || !ValidSHA(expected) {
		return errors.New("削除するリポジトリ・枝・SHAの指定が不正です")
	}
	mirror := filepath.Join(c.Root, "control", "repos", strings.ReplaceAll(repo, "/", "__")+".git")
	remote := strings.TrimRight(c.BaseURL, "/") + "/" + repo + ".git"
	runFrom := []string{"--git-dir", mirror}
	origin, err := c.checked(ctx, c.Root, append(runFrom, "remote", "get-url", "--push", "--all", "origin")...)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil || origin != remote {
		return errors.New("Git管理領域の接続先を確認できません")
	}
	// A literal URL avoids updating the mirror's remote-tracking refs. Reject
	// rewrites and URL-named remotes that could reinterpret the push target.
	rewrites, err := c.Run(ctx, c.Root, append(runFrom, "config", "--get-regexp", `^(url\..*\.(insteadof|pushinsteadof)$|remote\..*://.*\.)`)...)
	if err != nil {
		return err
	}
	if rewrites.Code != 1 {
		return errors.New("Gitの削除先を固定できません")
	}

	ref := "refs/heads/" + branch
	out, err := c.Run(ctx, c.Root, append(runFrom, "push", "--no-mirror", "--no-follow-tags", "--recurse-submodules=no", "--force-with-lease="+ref+":"+expected, "--", remote, ":"+ref)...)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		reason := pushReason(out.Out + "\n" + out.Err)
		if reason == PushConnectionReason {
			if match := deletePushHTTPStatus.FindStringSubmatch(out.Err); match != nil {
				status, _ := strconv.Atoi(match[1])
				if status >= 400 && status < 500 && status != 408 && status != 429 {
					reason = PushOtherReason
				}
			}
		}
		return &PushError{reason: reason}
	}
	return nil
}
