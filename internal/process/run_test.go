package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("WORKER_PROCESS_HELPER") == "" {
		return
	}
	mode := os.Getenv("WORKER_PROCESS_MODE")
	switch mode {
	case "echo":
		data := make([]byte, 10)
		n, _ := os.Stdin.Read(data)
		os.Stdout.Write(data[:n])
	case "wait":
		os.Stdout.WriteString("ready\n")
		<-time.After(time.Hour)
	case "flood":
		os.Stdout.WriteString(strings.Repeat("x", 4096))
	case "env":
		os.Stdout.WriteString(os.Getenv("ANTHROPIC_API_KEY"))
	case "exit":
		os.Exit(7)
	case "cancel-success":
		os.Stdout.WriteString("ready\n")
		for {
			if _, err := os.Stat(os.Getenv("WORKER_PROCESS_RELEASE")); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	os.Exit(0)
}

func TestCancellationErrorAfterSuccessfulProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := filepath.Join(t.TempDir(), "release")
	o := options("cancel-success")
	o.Env["WORKER_PROCESS_RELEASE"] = release
	o.OnLine = func(line string) {
		if line == "ready" {
			cancel()
		}
	}
	called := make(chan struct{})
	var command *exec.Cmd
	result, err := run(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, o, func(cmd *exec.Cmd) func() error {
		command = cmd
		return func() error {
			defer close(called)
			if err := os.WriteFile(release, nil, 0600); err != nil {
				return err
			}
			return context.Canceled
		}
	})
	select {
	case <-called:
	default:
		t.Fatal("cancel hook was not called")
	}
	if command.ProcessState == nil || !command.ProcessState.Success() || err != nil || !result.Started || result.Interrupted || result.Code != 0 || result.Out != "ready\n" {
		t.Fatalf("result=%+v err=%v state=%v", result, err, command.ProcessState)
	}
}
func options(mode string) Options {
	return Options{Env: map[string]string{"WORKER_PROCESS_HELPER": "1", "WORKER_PROCESS_MODE": mode}, Timeout: 5 * time.Second}
}
func TestCommandInputAndExitAreObserved(t *testing.T) {
	o := options("echo")
	o.Input = "input"
	result, err := Run(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, o)
	if err != nil || !result.Started || result.Out != "input" || result.Code != 0 {
		t.Fatal(result, err)
	}
	result, err = Run(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, options("exit"))
	if err != nil || !result.Started || result.Code != 7 {
		t.Fatal(result, err)
	}
}
func TestCancellationStopsRunningProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := options("wait")
	started := make(chan struct{})
	o.OnLine = func(line string) {
		if line == "ready" {
			close(started)
		}
	}
	type observed struct {
		result Result
		err    error
	}
	done := make(chan observed, 1)
	go func() {
		result, err := Run(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, o)
		done <- observed{result, err}
	}()
	select {
	case <-started:
	case got := <-done:
		t.Fatalf("子の開始合図がありません: %+v %v", got.result, got.err)
	}
	cancel()
	select {
	case got := <-done:
		if !got.result.Started || !got.result.Interrupted || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("実行が止まりません")
	}
}

func TestSuccessfulCommandBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := options("echo")
	o.Input = "finished"
	// The final unterminated line is delivered after cmd.Wait has returned.
	o.OnLine = func(string) { cancel() }
	result, err := Run(ctx, os.Args[0], []string{"-test.run=TestProcessHelper"}, o)
	if ctx.Err() != context.Canceled || err != nil || !result.Started || result.Code != 0 || result.Interrupted || result.Out != "finished" {
		t.Fatalf("result=%+v err=%v ctx=%v", result, err, ctx.Err())
	}
}

func TestProcessNotStarted(t *testing.T) {
	for _, name := range []string{"missing-executable", "invalid-directory", "canceled", "expired"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			exe := os.Args[0]
			o := options("echo")
			var want error
			switch name {
			case "missing-executable":
				exe = filepath.Join(t.TempDir(), "missing")
			case "invalid-directory":
				o.Dir = filepath.Join(t.TempDir(), "missing")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "expired":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				want = context.DeadlineExceeded
			}
			result, err := Run(ctx, exe, []string{"-test.run=TestProcessHelper"}, o)
			if result.Started || result.Out != "" || result.Err != "" || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestProcessStartSurvivesDeadlineAndOutputLimit(t *testing.T) {
	for _, name := range []string{"deadline", "output-limit"} {
		t.Run(name, func(t *testing.T) {
			o := options("wait")
			want := context.DeadlineExceeded
			if name == "output-limit" {
				o = options("flood")
				o.MaxOutputBytes = 64
				want = ErrOutputLimit
			}
			ready := false
			o.OnLine = func(line string) { ready = ready || line == "ready" }
			result, err := Run(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, o)
			if !result.Started || !result.Interrupted || !errors.Is(err, want) || (name == "deadline" && !ready) {
				t.Fatalf("ready=%v result=%+v err=%v", ready, result, err)
			}
		})
	}
}
func TestAPIKeyIsNotPassedToProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "private-api-key")
	result, err := Run(context.Background(), os.Args[0], []string{"-test.run=TestProcessHelper"}, options("env"))
	if err != nil || strings.Contains(result.Out, "private-api-key") {
		t.Fatal("APIキーが子へ渡りました", err)
	}
}

func TestVerificationCancellationAllowsCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{})
	out, err := func() (Result, error) {
		return Run(ctx, "/bin/sh", []string{"-c", `trap 'printf cleaned > "$1"; exit 0' TERM; printf 'ready\n'; sleep 60 & wait`, "verify", marker}, Options{Dir: t.TempDir(), Timeout: time.Second, GracefulCancel: true, KillGroupOnExit: true, OnLine: func(line string) {
			if line == "ready" {
				close(ready)
				cancel()
			}
		}})
	}()
	if !out.Interrupted || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "cleaned" {
		t.Fatal("検証の後片付けが実行されません", err)
	}
}

func TestVerificationCancellationKillsDetachedOutputChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-alive")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var child int
	out, err := Run(ctx, "/bin/sh", []string{"-c", `(trap '' TERM; sleep 1; printf alive > "$1") >/dev/null 2>&1 & printf 'child:%s\n' "$!"; wait`, "verify", marker}, Options{Dir: t.TempDir(), Timeout: 5 * time.Second, GracefulCancel: true, KillGroupOnExit: true, OnLine: func(line string) {
		if strings.HasPrefix(line, "child:") {
			child, _ = strconv.Atoi(strings.TrimPrefix(line, "child:"))
			cancel()
		}
	}})
	if child > 0 {
		defer syscall.Kill(child, syscall.SIGKILL)
	}
	if !out.Interrupted || !errors.Is(err, context.Canceled) || child == 0 {
		t.Fatal(out, err, child)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("中断後も子が実行されました", err)
	}
}
