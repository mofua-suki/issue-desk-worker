package hostd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/process"
)

// 中断した配置は前のコミットへ戻し、起動を確かめられたら rolled_back、できなければ rollback_failed として送る。
func TestFinishAppsRollsBackInterruptedDeploy(t *testing.T) {
	previous := strings.Repeat("b", 40)
	for name, tc := range map[string]struct {
		upFails        bool
		status, reason string
	}{
		"healthy": {false, "rolled_back", "rollback_healthy"},
		"failed":  {true, "failed", "rollback_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			home, app := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var sent []appResult
			var commands []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/deploy-result") {
					t.Errorf("予期しない経路: %s", r.URL.Path)
				}
				var result appResult
				_ = json.NewDecoder(r.Body).Decode(&result)
				mu.Lock()
				sent = append(sent, result)
				mu.Unlock()
				w.Write([]byte(`{}`))
			}))
			defer server.Close()
			a := App{Repo: "owner/app", Path: app, ComposeFiles: []string{"compose.yaml"}, Health: json.RawMessage(`["/bin/true"]`)}
			r := &Runtime{Config: Configuration{URL: server.URL, Host: "pc", Home: home, Apps: []App{a}}, HTTP: server.Client(), Now: time.Now,
				Sleep: func(context.Context, time.Duration) error { return nil },
				Run: func(_ context.Context, name string, args []string, _ process.Options) (process.Result, error) {
					mu.Lock()
					commands = append(commands, name+" "+strings.Join(args, " "))
					mu.Unlock()
					if name == "docker" && tc.upFails {
						return process.Result{Started: true, Code: 1}, nil
					}
					return process.Result{Started: true}, nil
				}}
			state := &appState{Tried: []string{}, Pending: &appResult{ID: "7", Status: "failed", Reason: "interrupted", Previous: previous}, Recovery: &appRecovery{Repo: "owner/app", Previous: previous}}
			if err := r.finishApps(context.Background(), state, map[string]App{"owner/app": a}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotSent, gotCommands := append([]appResult(nil), sent...), strings.Join(commands, "\n")
			mu.Unlock()
			if len(gotSent) != 1 || gotSent[0].Status != tc.status || gotSent[0].Reason != tc.reason {
				t.Fatalf("送った結果: %+v", gotSent)
			}
			checkout := strings.Index(gotCommands, "checkout --detach --no-overwrite-ignore "+previous)
			if checkout < 0 || strings.Index(gotCommands, "docker compose") < checkout {
				t.Fatalf("前のコミットへ戻してから起動していません: %v", gotCommands)
			}
			if state.Pending != nil || state.Recovery != nil {
				t.Fatalf("送った後も保留が残っています: %+v %+v", state.Pending, state.Recovery)
			}
		})
	}
}
