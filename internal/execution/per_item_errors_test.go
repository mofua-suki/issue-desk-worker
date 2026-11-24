package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// 1件の命令の確認に失敗しても、ほかの仕事の命令は処理する。同じ仕事の後の命令はこの回は待たせ、順番を保つ。
func TestCommandFailureDoesNotHoldOtherJobs(t *testing.T) {
	stuck := command{ID: "stuck-1", Kind: "stop"}
	stuck.Payload.JobID, stuck.Payload.Epoch = 2, 1
	later := command{ID: "stuck-2", Kind: "stop"}
	later.Payload.JobID, later.Payload.Epoch = 2, 1
	stale := command{ID: "stale", Kind: "stop"}
	stale.Payload.JobID, stale.Payload.Epoch = 1, 9
	var mu sync.Mutex
	lookups := map[string]int{}
	acked := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/commands":
			json.NewEncoder(w).Encode(map[string]any{"commands": []command{stuck, later, stale}, "next": 3})
		case r.URL.Path == "/api/v1/jobs/2":
			lookups["2"]++
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"temporarily_unavailable"}`))
		case r.URL.Path == "/api/v1/jobs/1":
			lookups["1"]++
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 1, "status": "done"}})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			acked = append(acked, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/commands/"), "/ack"))
			w.Write([]byte(`{}`))
		default:
			t.Errorf("予期しない経路: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	store, err := journal.Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{API: api, Store: store, Config: Configuration{Host: "fixture"}}
	err = e.pollCommands(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stuck-1") || !strings.Contains(err.Error(), "仕事2") {
		t.Fatalf("失敗した命令が分かりません: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acked) != 1 || acked[0] != "stale" {
		t.Fatalf("ほかの仕事の命令を処理していません: %v", acked)
	}
	if lookups["2"] != 1 {
		t.Fatalf("同じ仕事の後の命令を先に処理しました: %v", lookups)
	}
}

// 1種類の依頼の受け取りに失敗しても、ほかの種類の依頼は受け取る。
func TestRequestKindFailureDoesNotHoldOtherKinds(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/requests/next" {
			t.Errorf("予期しない経路: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		kind := r.URL.Query().Get("kind")
		mu.Lock()
		asked = append(asked, kind)
		mu.Unlock()
		if kind == "merge_manual" {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		w.Write([]byte(`{"request":null}`))
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{API: api, Config: Configuration{Host: "fixture"}}
	e.Config.Repositories = []Repository{{Name: "owner/project"}}
	e.Config.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
	err = e.processRequests(context.Background(), []string{"owner/project"}, []string{"merge_manual", "pr_create"}, true, nil)
	if err == nil || !strings.Contains(err.Error(), "merge_manual") {
		t.Fatalf("失敗した種類が分かりません: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(asked, ",") != "merge_manual,pr_create" {
		t.Fatalf("ほかの種類を受け取っていません: %v", asked)
	}
}

// 保存済みの依頼の読み直しは、壊れた記録と受付との食い違いを分け、依頼IDを示す。
func TestLoadRequestNamesRequestAndCause(t *testing.T) {
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: t.TempDir()}}}
	r := monitorRequest{ID: "123-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: json.RawMessage(`{"branch":"b"}`)}
	if err := os.MkdirAll(filepath.Dir(e.requests().File(r.ID)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.requests().File(r.ID), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := e.loadRequest(r)
	var syntax *json.SyntaxError
	if err == nil || !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), "壊れています") || !errors.As(err, &syntax) {
		t.Fatalf("壊れた記録と分かりません: %v", err)
	}
	other := r
	other.Repo = "owner/other"
	if err := e.saveRequest(&requestProgress{Request: other, Stage: "claimed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.loadRequest(r); err == nil || !strings.Contains(err.Error(), r.ID) || !strings.Contains(err.Error(), "リポジトリが違います") {
		t.Fatalf("食い違いの項目が分かりません: %v", err)
	}
}
