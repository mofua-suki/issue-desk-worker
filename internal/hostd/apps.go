package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/gitea"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var appJobIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var originRepoPattern = regexp.MustCompile(`^(?:https?://[^/]+/|ssh://[^/]+/|[^/@]+@[^:]+:)([^/]+/[^/]+?)(?:\.git)?$`)

type App struct {
	Repo         string          `json:"repo"`
	Path         string          `json:"path"`
	WorkDir      string          `json:"workDir"`
	ComposeFiles []string        `json:"composeFiles"`
	EnvFile      string          `json:"envFile"`
	Before       [][]string      `json:"before"`
	After        [][]string      `json:"after"`
	Health       json.RawMessage `json:"health"`
}
type appPaths struct {
	Root, CWD, Env string
	Files          []string
}

func argv(command []string) bool {
	if len(command) == 0 || command[0] == "" {
		return false
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return false
		}
	}
	return true
}
func inside(root, relative string, directory bool) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("アプリの相対パスが不正です")
	}
	for _, part := range strings.FieldsFunc(relative, func(c rune) bool { return c == '/' || c == '\\' }) {
		if part == ".." {
			return "", errors.New("アプリの相対パスが外側を指しています")
		}
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", cause.New("アプリのパスを確認できません", err)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", errors.New("アプリのパスが設定の外側です")
	}
	info, err := os.Stat(resolved)
	if err != nil || directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return "", cause.New("アプリのファイル種別が不正です", err)
	}
	return resolved, nil
}
func appConfig(a App, files bool) (appPaths, error) {
	var p appPaths
	if !gitea.ValidRepo(a.Repo) || !filepath.IsAbs(a.Path) || len(a.ComposeFiles) == 0 {
		return p, errors.New("アプリ配置の設定が不正です")
	}
	for _, list := range [][][]string{a.Before, a.After} {
		for _, command := range list {
			if !argv(command) {
				return p, errors.New("アプリ配置のコマンドが不正です")
			}
		}
	}
	var healthURL string
	var healthArgs []string
	if json.Unmarshal(a.Health, &healthURL) == nil {
		u, err := url.Parse(healthURL)
		if err != nil || u.User != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
			return p, cause.New("アプリ起動確認URLが不正です", err)
		}
	} else if json.Unmarshal(a.Health, &healthArgs) != nil || !argv(healthArgs) {
		return p, errors.New("アプリ起動確認の設定が不正です")
	}
	if !files {
		return p, nil
	}
	root, err := filepath.EvalSymlinks(a.Path)
	if err != nil {
		return p, cause.New("アプリ配置先を確認できません", err)
	}
	p.Root = root
	cwd := a.WorkDir
	if cwd == "" {
		cwd = "."
	}
	p.CWD, err = inside(root, cwd, true)
	if err != nil {
		return p, err
	}
	for _, file := range a.ComposeFiles {
		resolved, err := inside(root, file, false)
		if err != nil {
			return p, err
		}
		p.Files = append(p.Files, resolved)
	}
	if a.EnvFile != "" {
		p.Env, err = inside(root, a.EnvFile, false)
		if err != nil {
			return p, err
		}
	}
	return p, nil
}
func (r *Runtime) git(ctx context.Context, root string, args ...string) (string, error) {
	return r.command(ctx, root, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=never", "-C", root}, args...), "", 15*time.Minute)
}
func (r *Runtime) appUp(ctx context.Context, a App) error {
	p, err := appConfig(a, true)
	if err != nil {
		return err
	}
	for _, command := range a.Before {
		if _, err := r.command(ctx, p.CWD, command[0], command[1:], "", 15*time.Minute); err != nil {
			return err
		}
	}
	args := []string{"compose", "--project-directory", p.CWD}
	if p.Env != "" {
		args = append(args, "--env-file", p.Env)
	}
	for _, file := range p.Files {
		args = append(args, "-f", file)
	}
	args = append(args, "up", "-d", "--build")
	if _, err := r.command(ctx, p.CWD, "docker", args, "", 15*time.Minute); err != nil {
		return err
	}
	for _, command := range a.After {
		if _, err := r.command(ctx, p.CWD, command[0], command[1:], "", 15*time.Minute); err != nil {
			return err
		}
	}
	deadline := r.Now().Add(180 * time.Second)
	for r.Now().Before(deadline) {
		timeout := deadline.Sub(r.Now())
		if timeout > 5*time.Second {
			timeout = 5 * time.Second
		}
		var u string
		var command []string
		healthy := false
		if json.Unmarshal(a.Health, &u) == nil {
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			req, err := http.NewRequestWithContext(checkCtx, "GET", u, nil)
			if err == nil {
				res, err := r.HTTP.Do(req)
				if err == nil {
					healthy = res.StatusCode >= 200 && res.StatusCode < 300
					res.Body.Close()
				}
			}
			cancel()
		} else {
			_ = json.Unmarshal(a.Health, &command)
			_, err := r.command(ctx, p.CWD, command[0], command[1:], "", timeout)
			healthy = err == nil
		}
		if healthy && !r.Now().After(deadline) {
			return nil
		}
		remaining := deadline.Sub(r.Now())
		if remaining <= 0 {
			break
		}
		if remaining > 2*time.Second {
			remaining = 2 * time.Second
		}
		if err := r.Sleep(ctx, remaining); err != nil {
			return err
		}
	}
	return errors.New("アプリの起動を確認できません")
}

type appResult struct {
	ID       json.Number `json:"id"`
	Status   string      `json:"status"`
	Reason   string      `json:"reason"`
	Previous string      `json:"previous,omitempty"`
}
type appRecovery struct {
	Repo     string `json:"repo"`
	Previous string `json:"previous"`
}
type appState struct {
	Tried   []string   `json:"tried"`
	Pending *appResult `json:"pending"`
	// PendingError は Pending の配置が失敗した理由。受付へ送る結果は理由の分類だけなので、ここに残す。
	PendingError string       `json:"pendingError,omitempty"`
	Recovery     *appRecovery `json:"recovery"`
}

func (r *Runtime) runApps(ctx context.Context) error {
	state := &appState{Tried: []string{}}
	if err := r.load("apps-state.json", &state); err != nil {
		return err
	}
	if state == nil {
		state = &appState{Tried: []string{}}
	}
	apps, err := configuredApps(r.Config.Apps, state)
	if err != nil {
		return err
	}
	if state.Pending != nil || state.Recovery != nil {
		return r.finishApps(ctx, state, apps)
	}
	if len(apps) == 0 {
		return nil
	}
	var next struct {
		Job *appJob `json:"job"`
	}
	if err := r.api(ctx, "GET", "deploy-next", nil, &next); err != nil {
		return err
	}
	if next.Job == nil {
		return nil
	}
	job := next.Job
	if !appJobIDPattern.MatchString(string(job.ID)) || !gitea.ValidRepo(job.Repo) || !shaPattern.MatchString(job.Commit) {
		return errors.New("アプリ配置の依頼が不正です")
	}
	state.Pending = &appResult{ID: job.ID, Status: "failed", Reason: "preflight_failed"}
	state.PendingError = ""
	if err := r.save("apps-state.json", state); err != nil {
		return err
	}
	if err := r.deployApp(ctx, state, apps, job); err != nil {
		state.PendingError = err.Error()
		fmt.Fprintf(os.Stderr, "アプリ配置%sに失敗しました（%s）: %v\n", job.ID, state.Pending.Reason, err)
	}
	if err := r.save("apps-state.json", state); err != nil {
		return err
	}
	return r.finishApps(ctx, state, apps)
}

// appJob is one deployment the intake hands out.
type appJob struct {
	ID     json.Number `json:"id"`
	Repo   string      `json:"repo"`
	Commit string      `json:"commit"`
}

// configuredApps indexes the configured apps by repository and checks each.
// Paths are checked strictly only when no deployment is pending or recovering.
func configuredApps(configured []App, state *appState) (map[string]App, error) {
	apps := map[string]App{}
	for _, a := range configured {
		if _, exists := apps[a.Repo]; exists {
			return nil, errors.New("アプリの配置先が重複しています")
		}
		if _, err := appConfig(a, state.Pending == nil && state.Recovery == nil); err != nil {
			return nil, err
		}
		apps[a.Repo] = a
	}
	return apps, nil
}

// finishApps rolls back an interrupted deployment and reports the pending
// result, saving the state after each step.
func (r *Runtime) finishApps(ctx context.Context, state *appState, apps map[string]App) error {
	if state.Recovery != nil {
		recovery := state.Recovery
		if state.Pending == nil || !shaPattern.MatchString(recovery.Previous) {
			return errors.New("アプリの復旧状態が不正です")
		}
		state.Recovery = nil
		state.Pending.Status = "failed"
		state.Pending.Reason = "rollback_failed"
		if err := r.save("apps-state.json", state); err != nil {
			return err
		}
		a, ok := apps[recovery.Repo]
		if ok {
			if _, err := r.git(ctx, a.Path, "checkout", "--detach", "--no-overwrite-ignore", recovery.Previous); err == nil {
				if r.appUp(ctx, a) == nil {
					state.Pending.Status = "rolled_back"
					state.Pending.Reason = "rollback_healthy"
				}
			}
		}
		if err := r.save("apps-state.json", state); err != nil {
			return err
		}
	}
	if state.Pending != nil {
		if err := r.api(ctx, "POST", "deploy-result", state.Pending, nil); err != nil {
			return err
		}
		state.Pending, state.PendingError = nil, ""
		return r.save("apps-state.json", state)
	}
	return nil
}

// deployApp checks the local checkout of job's app and moves it to the job's
// commit. On failure the caller records the error. state.Recovery is saved
// just before the checkout, so finishApps can roll it back.
func (r *Runtime) deployApp(ctx context.Context, state *appState, apps map[string]App, job *appJob) error {
	key := job.Repo + ":" + job.Commit
	for _, tried := range state.Tried {
		if tried == key {
			return errors.New("同じ配置を実行済みです")
		}
	}
	state.Tried = append(state.Tried, key)
	if err := r.save("apps-state.json", state); err != nil {
		return err
	}
	a, allowed := apps[job.Repo]
	if !allowed {
		state.Pending.Reason = "not_allowed"
		return errors.New("配置先はローカルで許可されていません")
	}
	paths, err := appConfig(a, true)
	if err != nil {
		return err
	}
	root, err := r.git(ctx, paths.Root, "rev-parse", "--show-toplevel")
	if err != nil || root != paths.Root {
		return cause.New("配置先のGitが一致しません", err)
	}
	dirty, err := r.git(ctx, paths.Root, "status", "--porcelain")
	if err != nil || dirty != "" {
		return cause.New("配置先に変更があります", err)
	}
	origin, err := r.git(ctx, paths.Root, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	match := originRepoPattern.FindStringSubmatch(origin)
	if len(match) != 2 || match[1] != a.Repo {
		return errors.New("配置先のリポジトリが一致しません")
	}
	if _, err := r.git(ctx, paths.Root, "fetch", "origin"); err != nil {
		return err
	}
	ref, err := r.git(ctx, paths.Root, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil || !strings.HasPrefix(ref, "refs/remotes/origin/") {
		return cause.New("既定ブランチを確認できません", err)
	}
	if _, err := r.git(ctx, paths.Root, "merge-base", "--is-ancestor", job.Commit, ref); err != nil {
		return err
	}
	previous, err := r.git(ctx, paths.Root, "rev-parse", "HEAD")
	if err != nil || !shaPattern.MatchString(previous) {
		return cause.New("配置前のコミットを確認できません", err)
	}
	state.Pending.Previous = previous
	state.Recovery = &appRecovery{a.Repo, previous}
	if err := r.save("apps-state.json", state); err != nil {
		return err
	}
	if _, err := r.git(ctx, paths.Root, "checkout", "--detach", "--no-overwrite-ignore", job.Commit); err != nil {
		return err
	}
	if err := r.appUp(ctx, a); err != nil {
		return err
	}
	state.Recovery = nil
	state.Pending.Status = "deployed"
	state.Pending.Reason = "healthy"
	return nil
}
