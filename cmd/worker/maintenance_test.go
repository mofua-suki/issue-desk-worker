package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/journal"
)

func TestMaintenanceRunsCleanupAndIsolatesItsFailure(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tree := filepath.Join(store.Root, "workspaces", "owner__project", "job-1")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Tree: tree, Stage: "done", FinalReported: true}); err != nil {
		t.Fatal(err)
	}
	var order []string
	var mu sync.Mutex
	event := func(name string) { mu.Lock(); defer mu.Unlock(); order = append(order, name) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { event("cleanup"); w.WriteHeader(503) }))
	defer server.Close()
	g, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &execution.WorkspaceCleaner{Executor: &execution.Executor{Store: store, Gitea: g, Config: execution.Configuration{Fleet: execution.Fleet{RuntimeDir: store.Root}}}}
	err = maintainWorkspaces(context.Background(), []string{"owner/project"}, func(context.Context) error { event("environment"); return nil }, func(_ context.Context, eligible []string) error {
		event("requests")
		if !reflect.DeepEqual(eligible, []string{"owner/project"}) {
			t.Error("eligible changed", eligible)
		}
		return nil
	}, cleaner.Cleanup)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || !reflect.DeepEqual(order, []string{"environment", "requests", "cleanup"}) {
		t.Fatal(order, err)
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("cleanup failure lost tree", err)
	}
}

func TestMaintenancePreservesExistingErrors(t *testing.T) {
	for _, fail := range []string{"environment", "requests"} {
		t.Run(fail, func(t *testing.T) {
			want := errors.New(fail)
			cleaned := false
			err := maintainWorkspaces(context.Background(), nil, func(context.Context) error {
				if fail == "environment" {
					return want
				}
				return nil
			}, func(context.Context, []string) error { return want }, func(context.Context, []string) { cleaned = true })
			if !errors.Is(err, want) {
				t.Fatal("existing error hidden", err)
			}
			// 依頼の失敗では片付けを止めない。環境を読み直せないときは従来どおり止める。
			if cleaned != (fail == "requests") {
				t.Fatalf("片付けの実行が違います: %t", cleaned)
			}
		})
	}
}
