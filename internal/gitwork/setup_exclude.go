package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const setupExcludeName = "issue-desk-setup-exclude"

// gitDir resolves the per-worktree Git directory, which differs from the
// shared common directory for linked worktrees.
func gitDir(tree string) (string, error) {
	p := filepath.Join(tree, ".git")
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Abs(p)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("invalid worktree Git directory")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	line := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", errors.New("invalid worktree Git file")
	}
	p = strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(p) {
		p = filepath.Join(tree, p)
	}
	return filepath.Abs(p)
}

func localExcludeArgs(tree string, args ...string) []string {
	dir, err := gitDir(tree)
	if err != nil {
		return args
	}
	file := filepath.Join(dir, setupExcludeName)
	if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() {
		return append([]string{"-c", "core.excludesFile=" + file}, args...)
	}
	return args
}

// SetupArtifacts records an exclusion for setup-created untracked artifacts in
// this worktree's Git directory. Tracked files remain visible to Git.
func (c Config) SetupArtifacts(ctx context.Context, tree string) (func() error, error) {
	dir, err := gitDir(tree)
	if errors.Is(err, os.ErrNotExist) {
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, setupExcludeName)
	previous, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock := filepath.Join(tree, "pnpm-lock.yaml")
	_, err = os.Lstat(lock)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	tracked, err := c.Local(ctx, tree, 10*time.Second, "ls-files", "--error-unmatch", "--", "pnpm-lock.yaml")
	if err != nil {
		return nil, err
	}
	if tracked.Code != 0 && tracked.Code != 1 {
		return nil, fmt.Errorf("lockfile tracking check: %w", CommandError([]string{"ls-files"}, tracked))
	}
	write := func(lockExcluded bool) error {
		content := "node_modules/\n"
		if lockExcluded {
			content += "/pnpm-lock.yaml\n"
		}
		tmp, err := os.CreateTemp(dir, ".issue-desk-exclude-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, writeErr := tmp.WriteString(content)
		if err := errors.Join(writeErr, tmp.Close()); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), file)
	}
	wasExcluded := strings.Contains(string(previous), "/pnpm-lock.yaml\n")
	if err := write(wasExcluded); err != nil {
		return nil, err
	}
	return func() error {
		if existed || tracked.Code == 0 || wasExcluded {
			return nil
		}
		_, err := os.Lstat(lock)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return write(true)
	}, nil
}
