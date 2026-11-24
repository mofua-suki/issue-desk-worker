package execution

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"os"
	"regexp"
	"sort"
	"strings"

	"issue-desk-worker/internal/gitea"
)

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type StartupError struct {
	Name string
	Err  error
}

func (e *StartupError) Error() string { return e.Err.Error() }
func (e *StartupError) Unwrap() error { return e.Err }
func startupError(name string, err error) error {
	if err == nil {
		return nil
	}
	return &StartupError{Name: name, Err: err}
}

// Scope probes address nonexistent number zero only: a successful write is rejected.
func verifyWorkerScopes(ctx context.Context, c Configuration, g *gitea.Client) error {
	var repos []string
	for repo, hosts := range c.Assignments.Implementers {
		for _, host := range hosts {
			if host == c.Host {
				repos = append(repos, repo)
				break
			}
		}
	}
	sort.Strings(repos)
	if len(repos) == 0 {
		return errors.New("このPCが実装を担当するリポジトリがありません")
	}
	// Check the scopes on the assigned repositories in order and pass on the
	// first one that confirms them: one removed or renamed repository must not
	// keep the worker from starting. Report every failure if none does.
	var failures []error
	for _, repo := range repos {
		err := verifyRepositoryScopes(ctx, g, repo)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// verifyRepositoryScopes reads repo and checks, without changing anything,
// that the token may write its Issues and pull requests.
func verifyRepositoryScopes(ctx context.Context, g *gitea.Client, repo string) error {
	parts := strings.Split(repo, "/")
	if !repoNamePattern.MatchString(repo) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return errors.New("担当リポジトリ名が不正です")
	}
	if err := g.Request(ctx, "GET", "/repos/"+repo, nil, nil); err != nil {
		return fmt.Errorf("担当リポジトリ%sを読めません: %w", repo, err)
	}
	for _, kind := range []string{"issues", "pulls"} {
		err := g.Request(ctx, "PATCH", "/repos/"+repo+"/"+kind+"/0", map[string]any{}, nil)
		var refusal *gitea.Error
		if !errors.As(err, &refusal) || refusal.Status != 404 && refusal.Status != 422 {
			if err == nil {
				return fmt.Errorf("対象Giteaの%s書込権限を非破壊検査で確認できません（%sの番号0の更新が成功しました）", kind, repo)
			}
			return fmt.Errorf("対象Giteaの%s書込権限を非破壊検査で確認できません（%s）: %w", kind, repo, err)
		}
	}
	return nil
}

func verifyRuntimeVolume(dir string) error {
	if os.Getuid() != 1000 {
		return errors.New("このWorkerはuid 1000で実行してください")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return cause.New("実行状態フォルダーを作れません", err)
	}
	f, err := os.CreateTemp(dir, ".host-check-*")
	if err != nil {
		return cause.New("実行状態フォルダーに書けません", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString("ok"); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return cause.New("実行状態フォルダーへ保存できません", err)
	}
	return nil
}
