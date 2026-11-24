package hostd

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Reader struct {
	Repo         string   `json:"repo"`
	Path         string   `json:"path"`
	CheckCommand []string `json:"checkCommand"`
}
type readerResult struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Previous string `json:"previous,omitempty"`
}

func (r *Runtime) runReaders(ctx context.Context) error {
	if err := r.resendReaderResult(ctx); err != nil {
		return err
	}
	if len(r.Config.Readers) == 0 {
		return nil
	}
	var next struct {
		Jobs []readerJob `json:"jobs"`
	}
	if err := r.api(ctx, "GET", "reader-next", nil, &next); err != nil {
		return err
	}
	// Each job is decided and reported on its own; one bad job or setting
	// does not keep the others from being synced.
	var skipped error
	for _, job := range next.Jobs {
		if !shaPattern.MatchString(job.Commit) {
			// Not saved or reported: a result for an invalid commit could
			// never be resent and would block every later cycle.
			skipped = errors.Join(skipped, fmt.Errorf("読む側%sのコミットが不正です", job.Repo))
			continue
		}
		result := r.readerJobResult(ctx, job)
		if err := r.save("reader-state.json", result); err != nil {
			return err
		}
		if err := r.api(ctx, "POST", "reader-result", result, nil); err != nil {
			return err
		}
		if err := r.save("reader-state.json", nil); err != nil {
			return err
		}
	}
	return skipped
}

type readerJob struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// resendReaderResult sends the result a previous cycle saved but could not
// report.
func (r *Runtime) resendReaderResult(ctx context.Context) error {
	var pending *readerResult
	if err := r.load("reader-state.json", &pending); err != nil {
		return err
	}
	if pending == nil {
		return nil
	}
	if !shaPattern.MatchString(pending.Commit) {
		return errors.New("読む側の保存結果が不正です")
	}
	if err := r.api(ctx, "POST", "reader-result", pending, nil); err != nil {
		return err
	}
	return r.save("reader-state.json", nil)
}

// readerJobResult decides one job: which local reader it is for, whether
// that setting is usable, and the outcome of syncing it.
func (r *Runtime) readerJobResult(ctx context.Context, job readerJob) *readerResult {
	result := &readerResult{Repo: job.Repo, Commit: job.Commit, Status: "skipped", Reason: "not_allowed"}
	local, err := r.readerFor(job.Repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		result.Status, result.Reason = "failed", "preflight_failed"
		return result
	}
	if local == nil {
		return result
	}
	if !filepath.IsAbs(local.Path) || len(local.CheckCommand) > 0 && !argv(local.CheckCommand) {
		// Refuse a bad setting before touching the checkout, so it is never
		// left moved without its check.
		fmt.Fprintf(os.Stderr, "読む側%sの設定が不正です（pathは絶対パス、checkCommandは空でない引数の列にしてください）\n", local.Repo)
		result.Status, result.Reason = "failed", "preflight_failed"
		return result
	}
	if err := r.syncReader(ctx, local, job, result); err != nil {
		fmt.Fprintf(os.Stderr, "読む側%sを同期できません: %v\n", job.Repo, err)
		if result.Reason != "interrupted" {
			// Before the interrupted state is saved the checkout has not moved.
			// After it, keep that state and Previous so the move can be undone.
			result.Status, result.Reason, result.Previous = "failed", "preflight_failed", ""
		}
	}
	return result
}

// readerFor is the local reader configured for repo, or nil.
func (r *Runtime) readerFor(repo string) (*Reader, error) {
	var local *Reader
	for i := range r.Config.Readers {
		if r.Config.Readers[i].Repo == repo {
			if local != nil {
				return nil, fmt.Errorf("読む側%sの設定が重複しています", repo)
			}
			local = &r.Config.Readers[i]
		}
	}
	return local, nil
}

// syncReader fast-forwards the reader to job's commit, checks it and rolls
// it back when the check fails. It sets result's status and reason; an error
// is a failure before the checkout moved.
func (r *Runtime) syncReader(ctx context.Context, local *Reader, job readerJob, result *readerResult) error {
	root, err := filepath.EvalSymlinks(local.Path)
	if err != nil {
		return err
	}
	actual, err := r.git(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil || actual != root {
		return cause.New("読む側のGitを確認できません", err)
	}
	dirty, err := r.git(ctx, root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" {
		result.Reason = "dirty"
		return nil
	}
	branch, err := r.git(ctx, root, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		result.Reason = "detached"
		return nil
	}
	defaultRef, err := r.git(ctx, root, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return err
	}
	if branch != "refs/heads/"+strings.TrimPrefix(defaultRef, "refs/remotes/origin/") {
		result.Reason = "not_on_default"
		return nil
	}
	// 配備先のリポジトリ名を確認してから取得する。ジョブにURLは含めない。
	origin, err := r.git(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSuffix(origin, ".git"), "/"+job.Repo) && !strings.HasSuffix(strings.TrimSuffix(origin, ".git"), ":"+job.Repo) {
		return errors.New("読む側のリポジトリが一致しません")
	}
	if _, err := r.git(ctx, root, "fetch", "origin"); err != nil {
		return err
	}
	previous, err := r.git(ctx, root, "rev-parse", "HEAD")
	if err != nil || !shaPattern.MatchString(previous) {
		return cause.New("読む側の現在のコミットが不正です", err)
	}
	// A commit that is not there after the fetch is a failure, not a
	// divergence; merge-base answers both with a non-zero exit.
	if _, err := r.git(ctx, root, "cat-file", "-e", job.Commit+"^{commit}"); err != nil {
		return cause.New("読む側に取り込むコミットがありません", err)
	}
	if _, err := r.git(ctx, root, "merge-base", "--is-ancestor", job.Commit, defaultRef); err != nil {
		result.Reason = "diverged"
		return nil
	}
	if _, err := r.git(ctx, root, "merge-base", "--is-ancestor", previous, job.Commit); err != nil {
		if _, err := r.git(ctx, root, "merge-base", "--is-ancestor", job.Commit, previous); err == nil {
			result.Reason = "ahead"
		} else {
			result.Reason = "diverged"
		}
		return nil
	}
	result.Previous = previous
	result.Status = "failed"
	result.Reason = "interrupted"
	if err := r.save("reader-state.json", result); err != nil {
		result.Reason = "preflight_failed"
		return err
	}
	if _, err := r.git(ctx, root, "merge", "--ff-only", job.Commit); err != nil {
		// ff-only normally leaves the checkout as it was; only a checkout that
		// did move keeps the interrupted state so the move can be undone.
		head, headErr := r.git(ctx, root, "rev-parse", "HEAD")
		dirty, dirtyErr := r.git(ctx, root, "status", "--porcelain")
		if headErr == nil && head == previous && dirtyErr == nil && dirty == "" {
			result.Reason = "preflight_failed"
		}
		return err
	}
	if len(local.CheckCommand) > 0 {
		if _, err := r.command(ctx, root, local.CheckCommand[0], local.CheckCommand[1:], "", 3*time.Minute); err != nil {
			result.Status = "failed"
			result.Reason = "rollback_failed"
			dirty, readErr := r.git(ctx, root, "status", "--porcelain")
			if readErr == nil && dirty == "" {
				if _, resetErr := r.git(ctx, root, "reset", "--hard", previous); resetErr == nil {
					result.Status = "rolled_back"
					result.Reason = "rollback_healthy"
				}
			}
			return nil
		}
	}
	result.Status = "synced"
	result.Reason = "healthy"
	return nil
}
