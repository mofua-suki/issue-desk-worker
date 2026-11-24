package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/process"
)

func TestPushSuccessWhenCanceledAfterRun(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{Config: Config{Git: script}, Tree: t.TempDir(), Branch: "codex/test"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := w.push(ctx, func(ctx context.Context, dir string, args ...string) (process.Result, error) {
		result, err := w.Config.Run(ctx, dir, args...)
		cancel()
		return result, err
	})
	if ctx.Err() != context.Canceled || err != nil {
		t.Fatalf("push error=%v ctx=%v", err, ctx.Err())
	}
}

func TestPushReasonPriority(t *testing.T) {
	tests := []struct{ name, output, want string }{
		{"branch", "non-fast-forward", PushBranchReason},
		{"branch before hook", "fetch first; pre-receive hook declined", PushBranchReason},
		{"gitea hook before auth", "remote: Gitea: User permission denied for writing.\n! [remote rejected] HEAD -> codex/x (pre-receive hook declined)", PushHookReason},
		{"remote rejected hook", "remote rejected: custom hook; access denied", PushHookReason},
		{"auth before connection", "authentication failed; connection refused", PushAuthReason},
		{"connection", "Could not resolve host", PushConnectionReason},
		{"other", "denied", PushOtherReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pushReason(tc.output); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
