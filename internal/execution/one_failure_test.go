package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// 公開・マージの前の命令の確認で、ほかの仕事の命令を扱えないことは、この仕事を止めない。
func TestPollCommandsForIgnoresOtherJobsFailures(t *testing.T) {
	other := command{ID: "other-1", Kind: "stop"}
	other.Payload.JobID, other.Payload.Epoch = 2, 1
	own := command{ID: "own-1", Kind: "stop"}
	own.Payload.JobID, own.Payload.Epoch = 3, 1
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/commands":
			json.NewEncoder(w).Encode(map[string]any{"commands": []command{other, own}, "next": 2})
		case "/api/v1/jobs/2", "/api/v1/jobs/3":
			w.WriteHeader(503)
		default:
			t.Errorf("予期しない経路: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{API: api, Config: Configuration{Host: "fixture"}}
	if err := e.pollCommandsFor(context.Background(), 1); err != nil {
		t.Fatalf("ほかの仕事の失敗で止めました: %v", err)
	}
	// ほかの仕事の失敗と同じ回に出た自分の仕事の失敗は、見逃さない。
	if err := e.pollCommandsFor(context.Background(), 3); err == nil || !strings.Contains(err.Error(), "own-1") || strings.Contains(err.Error(), "other-1") {
		t.Fatalf("自分の仕事の命令の失敗だけを返していません: %v", err)
	}
}

// 未送信の最終結果が1件読めなくても、起動の復旧を止めず、ほかの報告を送る。
func TestRecoverSkipsUnreadableFinalState(t *testing.T) {
	var mu sync.Mutex
	var states []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/state") {
			mu.Lock()
			states = append(states, r.URL.Path)
			mu.Unlock()
		}
		io.WriteString(w, `{}`)
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
	for _, r := range []*journal.Record{{ID: 1, Repo: "owner/project", Issue: 7, Epoch: 1, Stage: "done", FinalState: json.RawMessage(`"not a state"`)}, {ID: 2, Repo: "owner/project", Issue: 8, Epoch: 1, Stage: "done", FinalState: json.RawMessage(`{"status":"held"}`)}} {
		if err := store.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	e := &Executor{API: api, Store: store, Config: Configuration{Host: "fixture", Fleet: Fleet{RuntimeDir: store.Root}}}
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("1件の不正な記録で起動を止めました: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 1 || !strings.Contains(states[0], "/jobs/2/") {
		t.Fatalf("ほかの報告を送っていません: %v", states)
	}
	if kept, err := store.Load(1); err != nil || kept == nil || kept.FinalReported {
		t.Fatalf("不正な記録を残していません: %+v %v", kept, err)
	}
}

// 起動時の権限の確認は、担当の1件が読めなくても、ほかで確かめられれば通す。
func TestWorkerScopesPassOnAnyAssignedRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/owner/gone") {
			w.WriteHeader(404)
			return
		}
		if r.Method == "PATCH" {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	g, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	c := Configuration{Host: "fixture"}
	c.Assignments.Implementers = map[string][]string{"owner/gone": {"fixture"}, "owner/live": {"fixture"}}
	// 名前順で owner/gone が先に試される。
	if err := verifyWorkerScopes(context.Background(), c, g); err != nil {
		t.Fatalf("1件の消えたリポジトリで起動を止めました: %v", err)
	}
	c.Assignments.Implementers = map[string][]string{"owner/gone": {"fixture"}}
	if err := verifyWorkerScopes(context.Background(), c, g); err == nil || !strings.Contains(err.Error(), "owner/gone") {
		t.Fatalf("どれも確かめられないのに通しました: %v", err)
	}
}
