package execution

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"issue-desk-worker/internal/journal"
)

// 固定テストが裏で起こしたプロセスは、テストの終わりに止める。常駐するWorkerに溜めない。
func TestSandboxStopsBackgroundProcesses(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "runtime", "workspaces", "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "pid")
	executable := filepath.Join(root, "sandbox")
	// codex sandbox の代役: 裏で長く動くプロセスを残して、本体はすぐ終わる。
	script := "#!/bin/sh\nsleep 300 </dev/null >/dev/null 2>&1 &\necho $! > " + pidFile + "\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: filepath.Join(root, "runtime"), WorkerTokenFile: filepath.Join(root, "private", "token"), Codex: executable}}}
	if _, err := e.sandbox(context.Background(), &journal.Record{Tree: tree}, []string{"/bin/true"}, 10); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	alive := func() bool {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		// 止まっても回収されていない（zombie）ものは、残っていないとみなす。
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true
		}
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		return len(fields) == 0 || fields[0] != "Z"
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive() {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("裏で起こしたプロセスが残っています")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
