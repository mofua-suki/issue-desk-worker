package execution

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/providers"
)

func (e *Executor) VerifyIsolation(ctx context.Context) error {
	parent := filepath.Join(e.Config.RuntimeDir, "workspaces")
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	tree, err := os.MkdirTemp(parent, "_isolation_check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tree)
	if err := os.MkdirAll(filepath.Join(e.Config.RuntimeDir, "control"), 0700); err != nil {
		return err
	}
	metadata := filepath.Join(tree, ".git")
	if info, err := os.Lstat(metadata); err == nil && info.IsDir() {
		if err := os.Remove(metadata); err != nil {
			return err
		}
	}
	if err := os.WriteFile(metadata, []byte("isolation-metadata"), 0600); err != nil {
		return err
	}
	sentinel := filepath.Join(e.Config.RuntimeDir, "control", "isolation-sentinel-"+providers.UUID())
	if err := os.WriteFile(sentinel, []byte("isolation"), 0600); err != nil {
		return err
	}
	defer os.Remove(sentinel)
	policy := providers.Policy{Root: e.Config.RuntimeDir, Tree: tree, TokenFile: e.Config.WorkerTokenFile, Tools: []string{e.Config.Codex, e.Executable}, Temp: tree}
	args := []string{"sandbox", "-P", "gitea-worker"}
	args = append(args, policy.Args()...)
	args = append(args, "-C", tree, "--", e.Executable, "isolation-probe", "--token-file", e.Config.WorkerTokenFile, "--sentinel", sentinel, "--workdir", tree)
	result, err := process.Run(ctx, e.Config.Codex, args, process.Options{Dir: tree, Timeout: 30 * time.Second})
	if err != nil || result.Code != 0 || !strings.Contains(result.Out, "ISOLATION_OK") {
		return fmt.Errorf("資格情報・状態の隔離検査に失敗しました（終了コード%d）。仕事は取得しません: %s", result.Code, clip(e.redact(result.Err), 1500))
	}
	if e.Store != nil {
		return e.clearEnvironmentHold()
	}
	return nil
}
