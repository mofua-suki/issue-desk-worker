package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Options struct {
	GracefulCancel bool
	// KillGroupOnExit also stops background descendants after the main command exits.
	KillGroupOnExit bool
	MaxOutputBytes  int
	Dir             string
	Input           string
	Env             map[string]string
	Timeout         time.Duration
	OnLine          func(string)
}
type Result struct {
	// Started reports a successful exec.Cmd.Start, even if execution later fails.
	Started      bool
	Code         int
	Out          string
	Err          string
	Elapsed      time.Duration
	Interrupted  bool
	Termination  string
	UserMillis   *int64
	SystemMillis *int64
}

var ErrOutputLimit = errors.New("コマンドの出力が上限を超えました")

func CleanEnv(extra map[string]string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LC_ALL": true, "TZ": true, "TMPDIR": true, "CODEX_HOME": true, "CLAUDE_CONFIG_DIR": true}
	values := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && keep[key] {
			values[key] = value
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

type capture struct {
	limit    int
	mu       sync.Mutex
	buf      strings.Builder
	line     string
	onLine   func(string)
	cancel   context.CancelFunc
	overflow bool
}

func (c *capture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len()+len(data) > c.limit {
		c.overflow = true
		c.cancel()
		return 0, ErrOutputLimit
	}
	c.buf.Write(data)
	if c.onLine != nil {
		c.line += string(data)
		for {
			line, rest, ok := strings.Cut(c.line, "\n")
			if !ok {
				break
			}
			c.line = rest
			c.onLine(line)
		}
	}
	return len(data), nil
}
func Run(ctx context.Context, exe string, args []string, options Options) (Result, error) {
	return run(ctx, exe, args, options, nil)
}

// cancelHook allows tests to control cancellation without changing Run's API.
func run(ctx context.Context, exe string, args []string, options Options, cancelHook func(*exec.Cmd) func() error) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if options.Timeout <= 0 {
		options.Timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	limit := options.MaxOutputBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	out, stderr := &capture{cancel: cancel, onLine: options.OnLine, limit: limit}, &capture{cancel: cancel, limit: limit}
	cmd := exec.CommandContext(runCtx, exe, args...)
	cmd.Dir = options.Dir
	cmd.Env = CleanEnv(options.Env)
	cmd.Stdin = strings.NewReader(options.Input)
	cmd.Stdout = out
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var cancelKill *time.Timer
	defer func() {
		if cancelKill != nil {
			cancelKill.Stop()
			// 親が先に終了しても、TERMを無視する子を残さない。
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		signal := syscall.SIGKILL
		if options.GracefulCancel {
			signal = syscall.SIGTERM
			cancelKill = time.AfterFunc(500*time.Millisecond, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		}
		err := syscall.Kill(-cmd.Process.Pid, signal)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if cancelHook != nil {
		cmd.Cancel = cancelHook(cmd)
	}
	cmd.WaitDelay = 2 * time.Second
	started := time.Now()
	err := cmd.Start()
	processStarted := err == nil
	if processStarted {
		err = cmd.Wait()
		if options.KillGroupOnExit {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	out.mu.Lock()
	if out.onLine != nil && out.line != "" {
		out.onLine(out.line)
		out.line = ""
	}
	out.mu.Unlock()
	result := Result{Started: processStarted, Code: 0, Out: out.buf.String(), Err: stderr.buf.String(), Elapsed: time.Since(started)}
	if cmd.ProcessState != nil {
		result.Termination = cmd.ProcessState.String()
		user, system := cmd.ProcessState.UserTime().Milliseconds(), cmd.ProcessState.SystemTime().Milliseconds()
		result.UserMillis, result.SystemMillis = &user, &system
	}
	if out.overflow || stderr.overflow {
		result.Interrupted = true
		return result, ErrOutputLimit
	}
	if options.GracefulCancel && runCtx.Err() != nil {
		result.Interrupted = true
		return result, runCtx.Err()
	}
	// Once the command and its output handling have succeeded, a later context
	// cancellation cannot undo its effects (notably a successful git push).
	if err == nil || (cmd.ProcessState != nil && cmd.ProcessState.Success() &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))) {
		return result, nil
	}
	if runCtx.Err() != nil {
		result.Interrupted = true
		return result, runCtx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.Code = exit.ExitCode()
			return result, nil
		}
		// The command exited, but a descendant kept its output open past
		// WaitDelay. That is the command's own exit, not a failure to start;
		// the descendant is left as it was (KillGroupOnExit stops it).
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			result.Code = cmd.ProcessState.ExitCode()
			return result, nil
		}
		return result, &LaunchError{Name: filepath.Base(exe), Err: err}
	}
	return result, nil
}

// LaunchError is a command that could not be started. Its message names the
// executable and the kind of failure, but not the full path or arguments.
type LaunchError struct {
	Name string
	Err  error
}

func (e *LaunchError) Error() string {
	kind := "起動に失敗しました"
	switch {
	case errors.Is(e.Err, exec.ErrNotFound), errors.Is(e.Err, os.ErrNotExist):
		kind = "見つかりません"
	case errors.Is(e.Err, os.ErrPermission):
		kind = "実行の権限がありません"
	}
	return "コマンド" + e.Name + "を起動できません（" + kind + "）"
}
func (e *LaunchError) Unwrap() error { return e.Err }
