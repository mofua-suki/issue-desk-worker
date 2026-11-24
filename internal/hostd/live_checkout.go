package hostd

import (
	"context"
	"errors"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/process"
	"os"
	"strings"
	"time"
)

// checkoutLive uses the configured origin's current default branch (HEAD).
func (r *Runtime) checkoutLive(ctx context.Context, path, commit string) error {
	dirty, err := r.liveGit(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil || dirty != "" {
		return cause.New("worktree not clean", err)
	}
	if _, err = r.liveGit(ctx, path, "fetch", "--no-tags", "origin", "HEAD"); err != nil {
		return err
	}
	if _, err = r.liveGit(ctx, path, "merge-base", "--is-ancestor", commit, "FETCH_HEAD"); err != nil {
		return err
	}
	// Fetch may take time; recheck before changing the worktree.
	dirty, err = r.liveGit(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil || dirty != "" {
		return cause.New("worktree not clean", err)
	}
	_, err = r.liveGit(ctx, path, "checkout", "--detach", "--no-overwrite-ignore", commit)
	return err
}

func (r *Runtime) liveGit(ctx context.Context, path string, args ...string) (string, error) {
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	if socket := os.Getenv("SSH_AUTH_SOCK"); socket != "" {
		env["SSH_AUTH_SOCK"] = socket
	}
	command := args
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=never", "-C", path}, args...)
	out, err := r.Run(ctx, "git", args, process.Options{Dir: path, Timeout: 15 * time.Minute, MaxOutputBytes: 16384, Env: env})
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		// Keep the subcommand, exit code and stderr tail (credentials hidden)
		// for the log; the report itself carries only the fixed reason.
		return "", errors.New(commandFailureLine("git", command, "", out, nil))
	}
	return strings.TrimSpace(out.Out), nil
}
