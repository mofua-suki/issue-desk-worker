package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/journal"
	"os"
	"path/filepath"
	"strings"
)

func (e *Executor) applyRequests(ctx context.Context, r *journal.Record) error {
	if r.ReportCompletion != nil && r.ReportCompletion.Attempted && !r.ReportCompletion.Validated {
		return errors.New("報告補完の検証前に編集要求を反映できません")
	}
	if r.ReportCompletion != nil && r.ReportCompletion.Validated {
		return e.applyCompletedReportRequests(ctx, r)
	}
	if err := e.applyImplementationRequests(ctx, r); err != nil {
		return err
	}
	return e.noteAppliedRequests(r)
}

func (e *Executor) applyImplementationRequests(ctx context.Context, r *journal.Record) error {
	var extensions struct {
		DeleteFiles  []string `json:"deleteFiles"`
		Dependencies []struct {
			Name, Version string
			Dev           bool
		} `json:"addDependencies"`
	}
	if err := json.Unmarshal(r.Result, &extensions); err != nil {
		return err
	}
	if len(extensions.DeleteFiles) > 0 {
		var result Implementation
		_ = json.Unmarshal(r.Result, &result)
		if result.Status != "completed" {
			return errors.New("completed以外の削除要求は反映しません")
		}
		if err := e.applyDeleteRequests(ctx, r, extensions.DeleteFiles); err != nil {
			return err
		}
	}

	if r.Provider == "claude" {
		if err := e.applyClaudeStaged(ctx, r); err != nil {
			return err
		}
	}

	if len(extensions.Dependencies) > 0 {
		if e.Config.PNPM == "" {
			return errors.New("依存追加用pnpmが設定されていません")
		}
		if err := dependencyPolicy(r.Tree); err != nil {
			return err
		}
		packageName := npmPackagePattern
		version := npmVersionPattern
		for _, request := range extensions.Dependencies {
			if len(request.Name) > 214 || !packageName.MatchString(request.Name) || len(request.Version) > 100 || !version.MatchString(request.Version) || strings.HasPrefix(request.Version, "-") {
				return errors.New("依存追加の名前または版が不正です")
			}
			command := []string{e.Config.PNPM, "add"}
			if request.Dev {
				command = append(command, "--save-dev")
			}
			if _, err := os.Stat(filepath.Join(r.Tree, "pnpm-workspace.yaml")); err == nil {
				command = append(command, "--workspace-root")
			}
			command = append(command, request.Name+"@"+request.Version, "--ignore-scripts", "--ignore-pnpmfile", "--config.minimum-release-age=20160")
			out, err := e.sandbox(ctx, r, command, e.Config.TestTimeoutSeconds)
			if err != nil {
				return err
			}
			if out.Code != 0 {
				return errors.New(e.commandFailureDetail("依存"+request.Name+"の追加が拒否または失敗しました", out))
			}
		}
	}
	return nil
}
