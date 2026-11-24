package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandaloneCheckout(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, b, err)
		}
		return strings.TrimSpace(string(b))
	}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	git(source, "init", "-b", "main")
	git(source, "config", "user.email", "test@example.test")
	git(source, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(source, "file"), []byte("old"), 0600)
	git(source, "add", "file")
	git(source, "commit", "-m", "old")
	old := git(source, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(source, "file"), []byte("new"), 0600)
	git(source, "commit", "-am", "new")
	target := git(source, "rev-parse", "HEAD")
	git(root, "clone", "--bare", source, "remote.git")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	daemon := exec.Command("git", "daemon", "--reuseaddr", "--export-all", "--base-path="+root, "--listen=127.0.0.1", fmt.Sprintf("--port=%d", port), root)
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { daemon.Process.Kill(); daemon.Wait() }()
	// Wait for the daemon to accept connections.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	work := filepath.Join(root, "work")
	git(root, "clone", fmt.Sprintf("git://127.0.0.1:%d/remote.git", port), work)
	git(work, "checkout", "--detach", old)
	state := filepath.Join(root, "state")
	os.Mkdir(state, 0700)
	tokenFile := filepath.Join(state, "token")
	os.WriteFile(tokenFile, []byte("idh_"+strings.Repeat("x", 43)), 0600)
	marker := filepath.Join(work, "ran")
	command := []string{"sh", "-c", "touch ran" + `; printf '{"pass":true,"summary":"ok","metrics":{},"failures":[]}'`}
	var commit string
	var result liveResult
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "live-check-next") {
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 1, "repo": "owner/project", "commit": commit, "host": "test", "command": command, "timeoutSeconds": 10}})
			return
		}
		var p livePending
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		result = p.Result
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	config := filepath.Join(state, "config")
	run := func(checkout bool) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"url": server.URL, "host": "test", "tokenFile": tokenFile, "stateFile": filepath.Join(state, "result"), "liveChecks": []any{map[string]any{"repo": "owner/project", "path": work, "command": command, "timeoutSeconds": 10, "checkout": checkout}}})
		os.WriteFile(config, raw, 0600)
		if err := liveCheckOnce(context.Background(), config, server.Client().Transport); err != nil {
			t.Fatal(err)
		}
	}
	commit = target
	run(true)
	if !result.Pass || git(work, "rev-parse", "HEAD") != target {
		t.Fatal("checkout did not precede successful command", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	os.Remove(marker)
	git(work, "checkout", "--detach", old)
	run(false)
	if result.Pass || result.Failures[0].Reason != "revision_mismatch" {
		t.Fatal(result)
	}
	commit = strings.Repeat("a", 40)
	run(true)
	if result.Pass || result.Failures[0].Reason != "checkout_failed" {
		t.Fatal(result)
	}
	commit = target
	os.WriteFile(filepath.Join(work, "untracked"), []byte("dirty"), 0600)
	run(true)
	if result.Pass || result.Failures[0].Reason != "checkout_failed" {
		t.Fatal(result)
	}
	os.Remove(filepath.Join(work, "untracked"))
	git(work, "remote", "set-url", "origin", "git://127.0.0.1:1/missing")
	run(true)
	if result.Pass || result.Failures[0].Reason != "checkout_failed" {
		t.Fatal(result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran on failure")
	}
}
