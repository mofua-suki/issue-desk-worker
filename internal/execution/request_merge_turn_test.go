package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
)

// マージ依頼のマージ番も、受付が返した期限の3分の1ごとに更新する（以前は1分固定で、短い期限が切れていた）。
func TestRequestedMergeRenewsTurnBeforeExpiry(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	renewed := make(chan struct{}, 10)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/acquire"):
			fmt.Fprintf(w, `{"granted":true,"turn":{"epoch":7,"expiresAt":%q}}`, time.Now().Add(3*time.Second).Format(time.RFC3339Nano))
		case strings.HasSuffix(r.URL.Path, "/renew"):
			renewed <- struct{}{}
			fmt.Fprintf(w, `{"turn":{"epoch":7,"expiresAt":%q}}`, time.Now().Add(3*time.Second).Format(time.RFC3339Nano))
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	m := &requestedMerge{e: &Executor{API: api}, ctx: context.Background(), route: mergeTurnRoute("owner/project"), who: map[string]any{"requestId": "1-aabbccdd"}}
	granted, err := m.acquireTurn()
	if err != nil || !granted {
		t.Fatalf("acquire: %v %v", granted, err)
	}
	select {
	case <-renewed:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("期限が切れる前にマージ番を更新していません")
	}
	m.turn.release()
	if m.turnCtx.Err() == nil {
		t.Fatal("解放後もマージ番の文脈が続いています")
	}
	mu.Lock()
	defer mu.Unlock()
	if paths[0] != "/api/v1/merge-turns/owner/project/acquire" || paths[len(paths)-1] != "/api/v1/merge-turns/owner/project/release" {
		t.Fatalf("paths %v", paths)
	}
}
