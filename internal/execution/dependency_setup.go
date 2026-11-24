package execution

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/journal"
	"os"
	"path/filepath"
	"strings"
)

func (e *Executor) setup(ctx context.Context, repo Repository, r *journal.Record) (result error) {
	steps, err := e.beginTimingSteps(r, []string{"setupCommand", "pnpmInstall"})
	if err != nil {
		return err
	}
	if err := dependencyPolicy(r.Tree); err != nil {
		return err
	}
	_, packageErr := os.Stat(filepath.Join(r.Tree, "package.json"))
	if len(repo.SetupCommand) == 0 && (e.Config.PNPM == "" || packageErr != nil) {
		return nil
	}
	finishArtifacts, err := e.gitConfig().SetupArtifacts(ctx, r.Tree)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, finishArtifacts()) }()
	if len(repo.SetupCommand) > 0 {
		out, err := e.timedSandbox(ctx, r, steps, 0, repo.SetupCommand, e.Config.TestTimeoutSeconds)
		if err != nil {
			return err
		}
		if out.Code != 0 {
			return errors.New(e.commandFailureDetail("設定済みのsetupCommandが失敗しました", out))
		}
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "package.json")); err == nil && e.Config.PNPM != "" {
		if len(repo.SetupCommand) > 0 {
			if err := dependencyPolicy(r.Tree); err != nil {
				return err
			}
		}
		args := []string{e.Config.PNPM, "install", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile", "--config.minimum-release-age=20160"}
		out, err := e.timedSandbox(ctx, r, steps, 1, args, e.Config.TestTimeoutSeconds)
		if err != nil {
			return err
		}
		if out.Code != 0 {
			return errors.New(e.commandFailureDetail("固定された依存の導入が失敗しました", out))
		}
	}
	return nil
}

// relativeName is file's path inside tree, for messages.
func relativeName(tree, file string) string {
	if rel, err := filepath.Rel(tree, file); err == nil {
		return rel
	}
	return filepath.Base(file)
}

// Inspect nested manifests before any repository command. Symlinks cannot read outside the tree.
func dependencyPolicy(tree string) error {
	return filepath.WalkDir(tree, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return cause.New("依存設定を探せません", walkErr)
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == ".pnpm" {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if name != "package.json" && name != "pnpm-workspace.yaml" && name != ".npmrc" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return cause.New("依存設定"+relativeName(tree, file)+"は作業ツリーの通常ファイルにしてください", err)
		}
		if info.Size() > 2<<20 {
			return fmt.Errorf("依存設定%sの大きさが上限を超えました", relativeName(tree, file))
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return cause.New("依存設定を読めません", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			folded := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(line))
			if strings.Contains(folded, "minimumreleaseageexclude") || strings.Contains(folded, "configdependencies") {
				return fmt.Errorf("依存設定%sに、依存パッケージの公開後14日制限を回避できる設定があります", relativeName(tree, file))
			}
		}
		return nil
	})
}
