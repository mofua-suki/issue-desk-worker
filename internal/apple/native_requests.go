package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/nativetest"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/redact"
	"issue-desk-worker/internal/requeststore"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

// appleRestart ends the runner: the configuration changed in a way that needs
// the startup check again, which launchd's restart provides.
type appleRestart struct{ error }

func (e *appleRestart) Unwrap() error { return e.error }

// runState is what one round needs to resume and report requests: the
// intake, the configuration of this round and the saved request progress.
type runState struct {
	API    *intake.Client
	Config fleetconfig.Configuration
	store  requeststore.Store
}

func (s *runState) saveRequest(p *requeststore.Progress) error { return s.store.Save(p) }

func (s *runState) releaseRequest(ctx context.Context, id string) error {
	return requeststore.Release(ctx, s.API, id)
}

func (s *runState) nativePolicy(ctx context.Context, repo, host string) error {
	return nativetest.Policy(ctx, s.API, s.Config, repo, host)
}

func (a *Runner) stateExecutor() *runState {
	return &runState{API: a.API, Config: a.Config, store: requeststore.Under(filepath.Join(a.Root, "control"))}
}

func (a *Runner) cycle(ctx context.Context) error {
	e := a.stateExecutor()
	// Finish a durable claimed/result request before making another claim.
	files, err := filepath.Glob(filepath.Join(e.store.Dir, "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var p requeststore.Progress
		if json.Unmarshal(data, &p) != nil || !gitwork.ValidRequestID(p.Request.ID) {
			// Set the unreadable state aside with its bytes so it does not
			// stop every later round; the intake redistributes the request.
			aside := file + ".broken"
			if _, err := os.Lstat(aside); err == nil {
				aside = fmt.Sprintf("%s.broken-%d", file, time.Now().UnixNano())
			}
			if err := os.Rename(file, aside); err != nil {
				return fmt.Errorf("保存されたApple依頼状態（%s）が不正で、退避もできません: %w", filepath.Base(file), err)
			}
			fmt.Fprintf(os.Stderr, "Apple依頼状態（%s）が不正なため%sへ退避しました\n", filepath.Base(file), filepath.Base(aside))
			continue
		}
		if p.Stage != "done" {
			return a.process(ctx, e, &p)
		}
	}
	snapshot, err := a.API.Config(ctx)
	if err != nil {
		return err
	}
	fresh, err := fleetconfig.DecodeApple(snapshot, a.Config.Host)
	if err != nil {
		return err
	}
	// Credential changes require a new startup check, never silently adopt them.
	if fresh.WorkerTokenFile != a.Config.WorkerTokenFile || fresh.GiteaURL != a.Config.GiteaURL {
		return &appleRestart{errors.New("資格情報設定が変更されました。再検査が必要です")}
	}
	changed := fresh.Version != a.Config.Version
	a.Config = fresh
	if changed {
		// The startup check copies the SDKs for every repository; keep the
		// heartbeat going meanwhile so the intake does not take this host as
		// gone. A heartbeat failure here does not interrupt the check.
		checkCtx, stop := a.keepAlive(ctx, false, nil)
		err = a.Check(checkCtx)
		stop()
		if err != nil {
			return &appleRestart{err}
		}
	} else {
		a.checksMu.Lock()
		a.checks.SettingsVersion = fresh.SettingsVersion
		a.checksMu.Unlock()
	}
	e = a.stateExecutor()
	repos, err := fresh.NativeRepositories()
	if err != nil {
		return err
	}
	var askFailures []error
	for _, repo := range repos {
		var out struct {
			Request *requeststore.Request `json:"request"`
		}
		if err = e.API.Call(ctx, "GET", "/api/v1/requests/next?kind=native_test&repo="+url.QueryEscape(repo.Name), nil, &out); err != nil {
			// A refusal answered for one repository does not keep the others
			// from being asked. A lost response may have claimed a request, so
			// it ends the round as before.
			var refusal *intake.APIError
			if ctx.Err() != nil || !errors.As(err, &refusal) {
				return errors.Join(append(askFailures, err)...)
			}
			askFailures = append(askFailures, fmt.Errorf("%sの依頼を取得できません: %w", repo.Name, err))
			continue
		}
		if out.Request == nil {
			continue
		}
		r := *out.Request
		if r.Kind != "native_test" || r.Repo != repo.Name || !gitwork.ValidRequestID(r.ID) {
			return errors.New("Apple依頼の対象が不正です")
		}
		// /next grants a new attempt. Durable results belong only to the
		// previous claim, even when the server redistributes the same ID.
		p := &requeststore.Progress{Request: r, Stage: "claimed", NativeVersion: a.Config.Version}
		if err = e.saveRequest(p); err != nil {
			return errors.Join(append(askFailures, err)...)
		}
		return errors.Join(append(askFailures, a.process(ctx, e, p))...)
	}
	return errors.Join(askFailures...)
}

func (a *Runner) current(ctx context.Context, e *runState, p *requeststore.Progress) error {
	var out struct {
		Request *nativetest.RequestState `json:"request"`
	}
	if err := e.API.Call(ctx, "GET", "/api/v1/requests/"+p.Request.ID, nil, &out); err != nil {
		var apiErr *intake.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 409 {
			return errNativeClaimLost
		}
		return err
	}
	r := out.Request
	if r == nil || r.ID != p.Request.ID || r.Kind != "native_test" || r.Repo != p.Request.Repo || !jsonEqual(r.Payload, p.Request.Payload) {
		return errors.New("claimed依頼が一致しません")
	}
	if len(p.Result) > 0 && r.Result != nil && jsonEqual(encode(r.Result), p.Result) {
		return errNativeResultAccepted
	}
	switch r.Status {
	case "claimed", "running":
		return nil
	default:
		return errNativeClaimLost
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func (a *Runner) process(ctx context.Context, e *runState, p *requeststore.Progress) error {
	if err := a.current(ctx, e, p); err != nil {
		if errors.Is(err, errNativeResultAccepted) || errors.Is(err, errNativeClaimLost) {
			p.Stage = "done"
			return e.saveRequest(p)
		}
		return err
	}
	if p.NativeVersion != a.Config.Version {
		return a.retire(ctx, e, p)
	}
	var target nativetest.Target
	if json.Unmarshal(p.Request.Payload, &target) != nil || target.Host != a.Config.Host || !gitwork.ValidSHA(target.Head) || !gitwork.ValidSHA(target.Base) || !gitwork.ValidSHA(target.Tree) {
		return errors.New("native_test payloadが不正です")
	}
	repos, err := a.Config.NativeRepositories()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(repos, func(r fleetconfig.Repository) bool { return r.Name == p.Request.Repo })
	if index < 0 {
		return errors.New("担当外のnative_testです")
	}
	if err = e.nativePolicy(ctx, p.Request.Repo, target.Host); err != nil {
		return a.policyFailure(ctx, e, p, err)
	}
	if len(p.Result) == 0 {
		if p.Stage == "running" {
			// A crash may have interrupted a test. Do not execute it a second time.
			p.Result = encode(nativetest.Result{Status: "interrupted", Head: target.Head, Base: target.Base, Tree: target.Tree, Message: "runnerが実行中に停止しました"})
		} else {
			p.Stage = "running"
			if err = e.saveRequest(p); err != nil {
				return err
			}
			// Stop the test when the heartbeat, the claim or the policy is lost.
			runCtx, stop := a.keepAlive(ctx, true, func(c context.Context) error {
				if err := a.current(c, e, p); err != nil {
					return err
				}
				return e.nativePolicy(c, p.Request.Repo, target.Host)
			})
			if err = a.heartbeat(ctx, true, true); err != nil {
				stop()
				return err
			}
			result := a.execute(runCtx, p.Request.Repo, target, repos[index])
			stop()
			p.Result = encode(result)
		}
		p.Stage = "result"
		if err = e.saveRequest(p); err != nil {
			return err
		}
	}
	// Never publish a successful result after claim/configuration loss.
	if err = a.current(ctx, e, p); err != nil {
		if errors.Is(err, errNativeClaimLost) || errors.Is(err, errNativeResultAccepted) {
			p.Stage = "done"
			return e.saveRequest(p)
		}
		return err
	}
	if err = e.nativePolicy(ctx, p.Request.Repo, target.Host); err != nil {
		return a.policyFailure(ctx, e, p, err)
	}
	if err = e.API.Call(ctx, "POST", "/api/v1/requests/"+p.Request.ID+"/result", map[string]any{"result": p.Result}, nil); err != nil {
		return a.policyFailure(ctx, e, p, err)
	}
	p.Stage = "done"
	return e.saveRequest(p)
}

func (a *Runner) execute(ctx context.Context, repo string, target nativetest.Target, policy fleetconfig.Repository) (result nativetest.Result) {
	defer func() {
		if ctx.Err() != nil {
			result.Status = "interrupted"
			result.Message = "ネイティブテストが中断されました"
		}
	}()
	result = nativetest.Result{Status: "unrunnable", Head: target.Head, Base: target.Base, Tree: target.Tree, Message: "Git準備またはtree検証に失敗しました"}
	dir, err := os.MkdirTemp(a.Root, checkoutPrefix)
	if err != nil {
		return result
	}
	defer removeTemp(dir)
	git := gitwork.Config{Git: a.Config.Git, Root: dir, BaseURL: a.Config.GiteaURL, TokenFile: a.Config.WorkerTokenFile, Executable: a.Executable, User: a.Config.GitUser}
	// Name the step that failed and keep git's reason, so a refused fetch
	// and a conflicting merge can be told apart in the result.
	prepare := func(step string, args ...string) bool {
		if _, err := git.Checked(ctx, dir, args...); err != nil {
			result.Message = "Git準備（" + step + "）に失敗しました: " + clip(redact.Known(err.Error()), 800)
			return false
		}
		return true
	}
	if !gitea.ValidRepo(repo) {
		result.Message = "Git準備に失敗しました: リポジトリ名が不正です"
		return result
	}
	if !prepare("init", "init") || !prepare("remote", "remote", "add", "origin", strings.TrimRight(a.Config.GiteaURL, "/")+"/"+repo+".git") || !prepare("fetch", "fetch", "--no-tags", "origin", target.Base, target.Head) || !prepare("checkout", "checkout", "--detach", target.Base) || !prepare("merge", "merge", "--no-commit", "--no-ff", target.Head) {
		return result
	}
	tree, err := git.Checked(ctx, dir, "write-tree")
	if err != nil {
		result.Message = "Git準備（write-tree）に失敗しました: " + clip(redact.Known(err.Error()), 800)
		return result
	}
	if tree != target.Tree {
		result.Message = "マージしたtreeが依頼の対象と一致しません"
		return result
	}
	timeout := policy.TestTimeoutSeconds
	if timeout <= 0 {
		timeout = a.Config.TestTimeoutSeconds
	}
	out, err := a.runSandbox(ctx, dir, policy.TestCommand, time.Duration(timeout)*time.Second, policy)
	result.Status = "failed"
	result.Message = "ネイティブテストが不合格です"
	if detail := a.diagnostic(out, err); detail != "" {
		result.Message += ": " + detail
	}
	if ctx.Err() != nil || out.Interrupted {
		result.Status = "interrupted"
		result.Message = "ネイティブテストが中断されました"
	} else if err != nil || !out.Started {
		result.Status = "unrunnable"
		result.Message = "ネイティブテストを実行できません: " + a.diagnostic(out, err)
	} else if out.Code == 0 {
		result.Status = "passed"
		result.Message = "ネイティブテストが合格しました"
	}
	return result
}

func (a *Runner) diagnostic(out process.Result, err error) string {
	message := out.Err
	if err != nil {
		message = err.Error() + ": " + message
	}
	for _, path := range []string{a.Bootstrap.ServerTokenFile, a.Config.WorkerTokenFile} {
		if path != "" {
			if token, e := os.ReadFile(path); e == nil && len(strings.TrimSpace(string(token))) > 0 {
				message = strings.ReplaceAll(message, strings.TrimSpace(string(token)), "[秘密]")
			}
		}
	}
	// Hide the known credential formats too, as the Linux worker does. Keep
	// the end, where tools put the reason for failing.
	message = redact.Known(message)
	if runes := []rune(message); len(runes) > 1000 {
		message = "…" + string(runes[len(runes)-999:])
	}
	return message
}

// The per-run directories under Root; the startup sweep removes leftovers.
const (
	probePrefix    = "probe-"
	checkoutPrefix = "checkout-"
	testHomePrefix = "test-home-"
)

func (a *Runner) retire(ctx context.Context, e *runState, p *requeststore.Progress) error {
	err := e.releaseRequest(ctx, p.Request.ID)
	var apiErr *intake.APIError
	if !errors.Is(err, requeststore.ErrReleased) && !(errors.As(err, &apiErr) && apiErr.Status == 409) {
		return err
	}
	p.Stage = "done"
	p.Result = nil
	return e.saveRequest(p)
}

func (a *Runner) policyFailure(ctx context.Context, e *runState, p *requeststore.Progress, err error) error {
	var apiErr *intake.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == 409 {
			return a.retire(ctx, e, p)
		}
		return err
	}
	// Only a confirmed fresh snapshot permits retirement; network failures retain progress.
	snapshot, fetchErr := a.API.Config(ctx)
	if fetchErr != nil {
		return fetchErr
	}
	if snapshot.ConfigurationVersion != p.NativeVersion {
		return a.retire(ctx, e, p)
	}
	return err
}
