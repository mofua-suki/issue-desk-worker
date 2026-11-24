package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/journal"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const stagedGuidance = "Claudeで実装する場合、.claude/はCLIの保護対象なので、変更する完成ファイルを同じ相対パスで.claude-staged/へ書いてください。削除は.claude-staged/.deleteへ.claude/で始まるパスを1行1件で記してください。completedの各ターン後にcontrollerが全件検証して反映します。.claude/worktrees、.git、範囲外、リンク、ディレクトリ上書き、作成と削除の重複は拒否します。question/failed/停止中は反映せず、.claude-stagedはコミットしません。"

type stagedItem struct {
	name   string
	data   []byte
	remove bool
}

func stagedTargetName(raw string) (string, error) {
	name, err := cleanControllerPath(raw)
	if err != nil {
		return "", err
	}
	if name != raw || !strings.HasPrefix(name, ".claude/") {
		return "", errors.New("stageの反映先は.claude/配下の正規相対パスだけです")
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 || strings.EqualFold(parts[1], "worktrees") {
		return "", errors.New("Claudeのworktreesへ反映できません")
	}
	return name, nil
}
func checkStagedTarget(r *journal.Record, name string) error {
	if r.ControllerAllowed != nil && !slices.Contains(r.ControllerAllowed, name) {
		return errors.New("競合解消の許容ファイル一覧の外です")
	}
	target, err := openSafeTarget(r.Tree, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	target.close()
	return nil
}
func (e *Executor) applyClaudeStaged(ctx context.Context, r *journal.Record) error {
	info, err := os.Lstat(filepath.Join(r.Tree, ".claude-staged"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return cause.New(".claude-stagedが通常のディレクトリではありません", err)
	}
	var result Implementation
	if json.Unmarshal(r.Result, &result) != nil || result.Status != "completed" {
		return errors.New("completed以外のstageは反映しません")
	}
	root, err := os.OpenRoot(r.Tree)
	if err != nil {
		return err
	}
	defer root.Close()
	stage, err := root.OpenRoot(".claude-staged")
	if err != nil {
		return err
	}
	defer stage.Close()
	actual, err := stage.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		return cause.New("stageの実体が検査中に変わりました", err)
	}
	plan := []stagedItem{}
	deletes := []string{}
	seen := map[string]bool{}
	total := 0
	err = fs.WalkDir(stage.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if strings.Contains(name, "\\") || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("stageにリンクまたは不正な名前があります")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("stageは通常ファイルだけです")
		}
		data, err := readSafeFile(r.Tree, ".claude-staged/"+name, 2<<20)
		if err != nil {
			return err
		}
		total += len(data)
		if total > 16<<20 || len(plan) > 200 {
			return errors.New("stageの件数・大きさが上限を超えました")
		}
		if name == ".delete" {
			for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					deletes = append(deletes, line)
				}
			}
			return nil
		}
		target, err := stagedTargetName(".claude/" + name)
		if err != nil {
			return err
		}
		if err := checkStagedTarget(r, target); err != nil {
			return err
		}
		seen[target] = true
		plan = append(plan, stagedItem{name: target, data: data})
		return nil
	})
	if err != nil {
		return err
	}
	deleted := map[string]bool{}
	for _, raw := range deletes {
		name, err := stagedTargetName(raw)
		if err != nil {
			return err
		}
		if seen[name] {
			return errors.New("同じstage対象の書込と削除が重複しています")
		}
		if deleted[name] {
			continue
		}
		deleted[name] = true
		if err := checkStagedTarget(r, name); err != nil {
			return err
		}
		plan = append(plan, stagedItem{name: name, remove: true})
	}
	if len(plan) > 200 {
		return errors.New("stageの件数が上限を超えました")
	}
	// Only apply after validation of every write and removal; each path is re-anchored before use.
	for _, item := range plan {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		action := "staged-write"
		if item.remove {
			action = "staged-delete"
		}
		if err := e.recordFileAction(r, item.name, action, "attempted"); err != nil {
			return err
		}
		if item.remove {
			target, err := openSafeTarget(r.Tree, item.name, false)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			} else if err == nil {
				err = target.remove()
				target.close()
			}
			if err != nil {
				return err
			}
		} else if err := writeSafeFile(r.Tree, item.name, item.data); err != nil {
			return err
		}
		if err := e.recordFileAction(r, item.name, action, "applied"); err != nil {
			return err
		}
	}
	return root.RemoveAll(".claude-staged")
}
