package execution

import (
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type safeTarget struct {
	parent *os.Root
	name   string
	info   os.FileInfo
}

func cleanControllerPath(name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.ContainsAny(name, "\x00\r\n\\") {
		return "", errors.New("controllerの対象が作業ツリーの通常パスではありません")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if strings.EqualFold(part, ".git") {
			return "", errors.New("Gitメタデータへは反映できません")
		}
	}
	if clean == ".claude/worktrees" || strings.HasPrefix(clean, ".claude/worktrees/") {
		return "", errors.New("Claudeのworktreesへは反映できません")
	}
	return clean, nil
}
func openSafeTarget(tree, name string, create bool) (*safeTarget, error) {
	clean, err := cleanControllerPath(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(tree)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, cause.New("作業ツリーが通常のディレクトリではありません", err)
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("作業ツリーの実体が変わりました")
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		before, err := root.Lstat(part)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = root.Mkdir(part, 0755); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
			before, err = root.Lstat(part)
		}
		if err != nil {
			root.Close()
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("対象の祖先にリンクまたは通常ディレクトリ以外があります")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			root.Close()
			return nil, errors.New("対象の祖先が検査中に変わりました")
		}
		root.Close()
		root = next
	}
	target := &safeTarget{parent: root, name: parts[len(parts)-1]}
	target.info, err = root.Lstat(target.name)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	if !target.info.Mode().IsRegular() {
		root.Close()
		return nil, errors.New("対象はリンクやディレクトリを含まない通常ファイルにしてください")
	}
	return target, nil
}
func (t *safeTarget) close() { _ = t.parent.Close() }
func (t *safeTarget) remove() error {
	if t.info == nil {
		return nil
	}
	current, err := t.parent.Lstat(t.name)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(t.info, current) {
		return errors.New("削除対象が検査後に変わりました")
	}
	return t.parent.Remove(t.name)
}
func readSafeFile(tree, name string, max int64) ([]byte, error) {
	target, err := openSafeTarget(tree, name, false)
	if err != nil {
		return nil, err
	}
	defer target.close()
	if target.info == nil {
		return nil, os.ErrNotExist
	}
	f, err := target.parent.OpenFile(target.name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(target.info, info) {
		return nil, cause.New("読取対象が検査後に変わりました", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("反映ファイルを読めません: %w", err)
	}
	if int64(len(data)) > max {
		return nil, errors.New("反映ファイルの大きさが上限を超えました")
	}
	return data, nil
}
func writeSafeFile(tree, name string, data []byte) error {
	target, err := openSafeTarget(tree, name, true)
	if err != nil {
		return err
	}
	defer target.close()
	mode := os.FileMode(0644)
	if target.info != nil {
		mode = target.info.Mode().Perm()
	}
	temp := ".worker-stage-" + providers.UUID()
	f, err := target.parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer target.parent.Remove(temp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return target.parent.Rename(temp, target.name)
}
