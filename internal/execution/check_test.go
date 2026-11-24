package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/journal"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckUsesRealReadinessWithoutJobOrJournalLock(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	_ = os.WriteFile(token, []byte("worker-secret"), 0600)
	marker := filepath.Join(root, "probed")
	codex := filepath.Join(root, "codex")
	_ = os.WriteFile(codex, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo codex-cli 0.160.1; exit; fi\nif [ \"$1\" = login ]; then echo 'Logged in using ChatGPT'; exit; fi\nprintf yes > '"+marker+"' || exit 1\necho ISOLATION_OK\n"), 0700)
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch {
		case r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "worker"})
		case r.URL.Path == "/api/v1/repos/a/b" && r.Method == "GET":
			w.Write([]byte(`{}`))
		case r.Method == "PATCH" && (r.URL.Path == "/api/v1/repos/a/b/issues/0" || r.URL.Path == "/api/v1/repos/a/b/pulls/0"):
			w.WriteHeader(404)
		default:
			t.Errorf("予定外の要求: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := Configuration{Host: "test"}
	cfg.RuntimeDir = root
	cfg.WorkerTokenFile = token
	cfg.Codex = codex
	cfg.GiteaURL = server.URL
	cfg.Assignments.Implementers = map[string][]string{"a/b": {"test"}}
	cfg.Assignments.WorkerAccounts = map[string]string{"test": "worker"}
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := Check(context.Background(), cfg, "/unused/probe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("実隔離検査を呼びませんでした")
	}
	if len(calls) != 4 {
		t.Fatal(calls)
	}
	cfg.Assignments.WorkerAccounts["test"] = "different"
	if err := Check(context.Background(), cfg, "/unused/probe"); err == nil || !strings.Contains(err.Error(), "一致") {
		t.Fatalf("所有者の違いを見逃しました: %v", err)
	}
}
