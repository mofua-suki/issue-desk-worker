package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"issue-desk-worker/internal/hostd"
)

func TestHostdLockContention(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "client")
	if err := os.Mkdir(client, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"token": "idh_" + strings.Repeat("a", 43), "registry-password": "synthetic"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := json.Marshal(map[string]any{"url": "https://example.invalid", "host": "test", "clientDir": client})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, config, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(client, ".linux-hostd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "hostd")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"immediate", []string{"update-now", "--config", file, "--wait", "1m"}, 2},
		{"timer", []string{"--config", file}, 0},
		{"check", []string{"--config", file, "--check"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(binary, tc.args...).CombinedOutput()
			code := 0
			if err != nil {
				e, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = e.ExitCode()
			}
			if code != tc.code || !strings.Contains(string(out), "ロック") {
				t.Fatalf("code=%d output=%q", code, out)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 5 {
				t.Fatalf("unexpected changes while locked: %v", entries)
			}
		})
	}
}

func TestImmediateResultExitStatus(t *testing.T) {
	for _, tc := range []struct {
		result hostd.ImmediateUpdateResult
		code   int
	}{
		{hostd.ImmediateUpdateResult{Reason: "success"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "same_version"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "no_delivery"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "pinned"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "failed_tag"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "report_retried", ReportStatus: "success"}, 0},
		{hostd.ImmediateUpdateResult{Reason: "report_retried", ReportStatus: "failed"}, 1},
		{hostd.ImmediateUpdateResult{Reason: "digest_mismatch"}, 1},
		{hostd.ImmediateUpdateResult{Reason: "ready_failed"}, 1},
		{hostd.ImmediateUpdateResult{Reason: "recovered"}, 1},
		{hostd.ImmediateUpdateResult{Reason: "wait_timeout", WaitReason: "busy", Publishing: 1}, 2},
		{hostd.ImmediateUpdateResult{Reason: "new_implementation"}, 2},
		{hostd.ImmediateUpdateResult{Reason: "drain_timeout"}, 2},
	} {
		t.Run(tc.result.Reason+tc.result.ReportStatus, func(t *testing.T) {
			if got := printUpdateResult(tc.result); got != tc.code {
				t.Fatalf("result=%+v code=%d", tc.result, got)
			}
		})
	}
}

func TestUpdateNowInterruptProcess(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "client")
	if err := os.Mkdir(client, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"token": "idh_" + strings.Repeat("a", 43), "registry-password": "synthetic", "client/.env": "FLAVOR=\nISSUE_DESK_CLIENT_TAG=" + strings.Repeat("a", 12) + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "desired-image") {
			http.Error(w, "unexpected", 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"desired": map[string]any{"tag": strings.Repeat("b", 12), "revision": strings.Repeat("b", 40), "registry": "registry.example:5443", "digest": "sha256:" + strings.Repeat("d", 64), "kind": "lazy"}, "heartbeat": map[string]any{"at": time.Now(), "implementing": 1, "publishing": 2}})
	}))
	defer server.Close()
	cert := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{"url": server.URL, "host": "test", "clientDir": client})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, config, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "hostd")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cmd := exec.Command(binary, "update-now", "--config", file, "--wait", "1m")
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+cert)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("waiting output: %v %q %s", err, line, stderr.String())
	}
	if !strings.Contains(line, "lazyの空き") || !strings.Contains(line, "実行中 1、公開中 2") {
		t.Fatalf("waiting output: %q", line)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(rest), "待機を中止しました") {
		t.Fatalf("exit=%v output=%q stderr=%q", err, rest, stderr.String())
	}
}

// ロック中でも、資格情報の誤りは --check の失敗として残す。
func TestHostdCheckWhileLockedStillChecksCredentials(t *testing.T) {
	dir := t.TempDir()
	client := filepath.Join(dir, "client")
	if err := os.Mkdir(client, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"token": "not-a-host-token", "registry-password": "synthetic"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config, err := json.Marshal(map[string]any{"url": "https://example.invalid", "host": "test", "clientDir": client})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, config, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(client, ".linux-hostd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "hostd")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	out, err := exec.Command(binary, "--config", file, "--check").CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || strings.Contains(string(out), "ロック") {
		t.Fatalf("資格情報の誤りを見逃しました: %v %q", err, out)
	}
	// 同じ手順で資格情報だけを正すと、ロック中の待ちとして0で終える。
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("idh_"+strings.Repeat("a", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(binary, "--config", file, "--check").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "実行ロック中です") {
		t.Fatalf("ロック中の待ちになりません: %v %q", err, out)
	}
}
