package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 配置が失敗した理由を捨てず、受付へ送る前の保存状態に残す（受付へは理由の分類だけを送る）。
func TestRunAppsKeepsDeployFailureReason(t *testing.T) {
	home, app := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/deploy-next"):
			fmt.Fprintf(w, `{"job":{"id":7,"repo":"owner/other","commit":"%s"}}`, strings.Repeat("a", 40))
		case strings.HasSuffix(r.URL.Path, "/deploy-result"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = append(sent, fmt.Sprint(body))
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	r := &Runtime{Config: Configuration{URL: server.URL, Host: "pc", Home: home, Apps: []App{{Repo: "owner/app", Path: app, ComposeFiles: []string{"compose.yaml"}, Health: json.RawMessage(`"http://localhost/"`)}}}, HTTP: server.Client()}
	if err := r.runApps(context.Background()); err == nil {
		t.Fatal("受付へ送れなかったのに成功しています")
	}
	var state appState
	data, err := os.ReadFile(filepath.Join(home, "apps-state.json"))
	if err != nil || json.Unmarshal(data, &state) != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.Reason != "not_allowed" || !strings.Contains(state.PendingError, "許可されていません") {
		t.Fatalf("理由が残っていません: %+v %q", state.Pending, state.PendingError)
	}
	if len(sent) != 1 || strings.Contains(sent[0], "許可されていません") {
		t.Fatalf("受付へ送った結果: %v", sent)
	}
}
