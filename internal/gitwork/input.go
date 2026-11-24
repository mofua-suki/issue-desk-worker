package gitwork

import (
	"context"
	"fmt"
	"issue-desk-worker/internal/process"
	"strings"
	"time"
)

func (c Config) Input(ctx context.Context, dir, input string, args ...string) (process.Result, error) {
	return process.Run(ctx, c.Git, c.args(args...), process.Options{Dir: dir, Input: input, Timeout: 5 * time.Minute, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}})
}

// InputChecked is Input that turns a non-zero exit into CommandError.
func (c Config) InputChecked(ctx context.Context, dir, input string, args ...string) error {
	out, err := c.Input(ctx, dir, input, args...)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return CommandError(args, out)
	}
	return nil
}

// MergeTree returns the tree of merging second onto first without touching
// the work tree. A conflict is ErrConflict; other failures name the cause.
func (c Config) MergeTree(ctx context.Context, dir, first, second string) (string, error) {
	args := []string{"merge-tree", "--write-tree", "--no-messages", first, second}
	out, err := c.Run(ctx, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git merge-tree を実行できません: %w", err)
	}
	switch {
	case out.Code == 0:
		return strings.TrimSpace(out.Out), nil
	case out.Code == 1:
		return "", ErrConflict
	case out.Code < 0:
		return "", fmt.Errorf("git merge-tree が外から停止されました: %w", CommandError(args, out))
	}
	return "", fmt.Errorf("git merge-tree が失敗しました（Git 2.38以上が必要）: %w", CommandError(args, out))
}
