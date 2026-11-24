// Package apple runs native (Apple) tests on a Mac for the intake. It never
// enters the Linux implementation scheduler or the provider sandbox.
package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/redact"
	"issue-desk-worker/internal/tempdir"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/failurelog"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/readiness"
)

// Runner never enters the Linux implementation scheduler or provider sandbox.
// OS is injectable so its admission checks can be exercised on Linux.
type Runner struct {
	OS         string
	Bootstrap  config.Bootstrap
	Config     fleetconfig.Configuration
	API        *intake.Client
	Executable string
	Root       string
	// checks is the last startup check; heartbeats read it while a check
	// after a configuration change runs, so checksMu guards it.
	checksMu sync.Mutex
	checks   readiness.Report
	sandbox  string
}

func New(b config.Bootstrap, c fleetconfig.Configuration, api *intake.Client, executable string) (*Runner, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return &Runner{OS: runtime.GOOS, Bootstrap: b, Config: c, API: api, Executable: executable, Root: filepath.Join(cache, "issue-desk", "apple", b.WorkerID), sandbox: "/usr/bin/sandbox-exec"}, nil
}
func (a *Runner) Check(ctx context.Context) (err error) {
	defer func() {
		checks := readiness.Report{At: time.Now(), SettingsVersion: a.Config.SettingsVersion}
		checks.Record("runtime", err == nil)
		a.checksMu.Lock()
		a.checks = checks
		a.checksMu.Unlock()
	}()

	if a.OS != "darwin" {
		return errors.New("このApple runnerはDarwin専用です")
	}
	if _, err := a.Bootstrap.ReadToken(); err != nil {
		return err
	}
	if _, err := exec.LookPath(a.Config.Git); err != nil {
		return fmt.Errorf("指定されたGitコマンドがありません: %s", a.Config.Git)
	}
	repos, err := a.Config.NativeRepositories()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(a.Root) || filepath.Clean(a.Root) == filepath.Clean(a.Config.RuntimeDir) {
		return errors.New("Apple専用runtimeが必要です")
	}
	if err = os.MkdirAll(filepath.Join(a.Root, "control"), 0700); err != nil {
		return err
	}
	token, err := gitea.ReadToken(a.Config.WorkerTokenFile)
	if err != nil {
		return err
	}
	g, err := gitea.New(a.Config.GiteaURL, token)
	if err != nil {
		return err
	}
	if warning := gitea.PlainHTTPWarning(a.Config.GiteaURL); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	if err = verifyAppleAccess(ctx, a.Config, g, repos); err != nil {
		return err
	}
	for _, r := range repos {
		if _, err = a.toolRoots(r); err != nil {
			return err
		}
	}
	// A real deny/read/write probe must succeed before any request is claimed.
	tree, err := os.MkdirTemp(a.Root, probePrefix)
	if err != nil {
		return err
	}
	defer removeTemp(tree)
	sentinel := filepath.Join(a.Root, "control", "isolation-sentinel")
	if err = os.WriteFile(sentinel, []byte("private"), 0600); err != nil {
		return err
	}
	defer os.Remove(sentinel)
	script := `sentinel=$1; shift; if /bin/sh -c 'echo forbidden >> "$1"' sh "$sentinel" 2>/dev/null; then exit 32; fi; for p in "$sentinel" "$@"; do if /bin/cat "$p" >/dev/null 2>&1; then exit 31; fi; done; echo ok > probe-output`
	command := []string{"/bin/sh", "-c", script, "probe", sentinel, a.Bootstrap.ServerTokenFile, a.Config.WorkerTokenFile}
	for _, policy := range append([]fleetconfig.Repository{{}}, repos...) {
		result, err := a.runSandbox(ctx, tree, command, 30*time.Second, policy)
		if err != nil || result.Code != 0 {
			return fmt.Errorf("このApple runnerの資格情報・状態隔離検査に失敗しました: %s", a.diagnostic(result, err))
		}
	}
	return nil
}
func (a *Runner) Run(ctx context.Context) error {
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.heartbeat(c, false, false)
	}()

	if err := a.Check(ctx); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(a.Root, "control", "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("このApple runnerは既に起動しています")
	}
	// Holding the lock, no other run uses the runtime: remove what a crashed
	// run left (each test home carries a copy of the SDKs).
	if err := tempdir.Sweep([]string{a.Root}, tempPatterns()); err != nil {
		fmt.Fprintf(os.Stderr, "前回の一時領域を消せませんでした: %v\n", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// A temporary failure of the intake, Gitea or one request is retried on
	// the next round instead of ending the runner; only a change that needs a
	// fresh startup check ends it.
	failures := appleFailureLog()
	for {
		if err = a.round(ctx, failures); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// appleFailureLog records Apple runner failures without credentials.
func appleFailureLog() *failurelog.Log {
	return &failurelog.Log{Label: "Apple runnerの周期を完了できませんでした", Redact: redact.Known}
}

// round runs one heartbeat and cycle. It returns an error only when the runner
// must end; other failures are logged and left for the next round.
func (a *Runner) round(ctx context.Context, failures *failurelog.Log) error {
	err := a.heartbeat(ctx, true, false)
	if err == nil {
		err = a.cycle(ctx)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var restart *appleRestart
	if errors.As(err, &restart) {
		return err
	}
	// A refused credential does not recover by retrying; end the runner so
	// the restart runs the startup check again.
	var refused *intake.APIError
	if errors.As(err, &refused) && (refused.Status == 401 || refused.Status == 403) {
		return &appleRestart{err}
	}
	failures.Observe(err)
	return nil
}

var errNativeResultAccepted = errors.New("native result accepted")
var errNativeClaimLost = errors.New("依頼のclaimを喪失しました")

// keepAlive sends a heartbeat every 5 seconds until stop is called. With
// watch set, the returned context ends when a heartbeat or watch fails;
// without it, failures are left to the next round.
func (a *Runner) keepAlive(ctx context.Context, busy bool, watch func(context.Context) error) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				err := a.heartbeat(runCtx, true, busy)
				if watch == nil {
					continue
				}
				if err == nil {
					err = watch(runCtx)
				}
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, func() { cancel(); <-done }
}

func (a *Runner) heartbeat(ctx context.Context, ready, busy bool) error {
	a.checksMu.Lock()
	checks := a.checks.Value()
	a.checksMu.Unlock()
	return a.API.Heartbeat(ctx, intake.HostStatus{Ready: ready, Busy: busy, Capacity: 1, Capabilities: map[string]any{"nativeTest": true, "platform": a.OS, "providers": []string{}, "repositories": a.Config.HostAssignment().TestRepositories}, Checks: checks})
}

func verifyAppleAccess(ctx context.Context, c fleetconfig.Configuration, g *gitea.Client, repos []fleetconfig.Repository) error {
	if err := fleetconfig.VerifyWorkerAccount(ctx, c, g); err != nil {
		return err
	}
	for _, r := range repos {
		if _, err := g.Repo(ctx, r.Name); err != nil {
			return fmt.Errorf("許可リポジトリ%sを読めません: %w", r.Name, err)
		}
	}
	return nil
}

func encode(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
