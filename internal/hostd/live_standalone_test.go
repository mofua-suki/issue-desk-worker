package hostd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneLivePreservesExistingTokenAndPendingResult(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	token := "idh_" + strings.Repeat("x", 43)
	file := filepath.Join(dir, "token")
	_ = os.WriteFile(file, []byte(token), 0600)
	stateFile := filepath.Join(dir, "existing-result.json")
	pending := map[string]any{"id": 21, "result": map[string]any{"pass": true, "summary": "確認済み", "metrics": map[string]any{"count": 2}, "failures": []any{}}}
	raw, _ := json.Marshal(pending)
	_ = os.WriteFile(stateFile, raw, 0600)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+token || r.Method != "POST" || r.URL.Path != "/api/v1/hosts/nas/live-check-result" {
			t.Error("旧予約結果・本人・送信先を保持していません")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["id"] != float64(21) {
			t.Error("予約IDを変えました")
		}
		w.Write([]byte(`{"saved":true}`))
	}))
	defer server.Close()
	cfg := map[string]any{"url": server.URL, "host": "nas", "tokenFile": file, "stateFile": stateFile, "liveChecks": []any{}}
	raw, _ = json.Marshal(cfg)
	configFile := filepath.Join(dir, "config.json")
	_ = os.WriteFile(configFile, raw, 0600)
	if err := liveCheckOnce(context.Background(), configFile, server.Client().Transport); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(stateFile)
	if strings.TrimSpace(string(after)) != "null" || calls != 1 {
		t.Fatalf("保留結果の回収が不正: %s calls=%d", after, calls)
	}
	stored, _ := os.ReadFile(file)
	if string(stored) != token {
		t.Fatal("既存Bearerを書換えました")
	}
}
