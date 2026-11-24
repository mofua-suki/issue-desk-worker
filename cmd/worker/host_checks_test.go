package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
	workerruntime "issue-desk-worker/internal/runtime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStartupFailureReportsNotReadyWithoutAcquiringJob(t *testing.T) {
	var status intake.HostStatus
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/client-config":
			json.NewEncoder(w).Encode(map[string]any{"version": "config-1", "configurationVersion": "fleet-2", "settingsVersion": "settings-3", "config": map[string]any{"runtimeDir": "not-absolute"}})
		case "PUT /api/v1/hosts/test":
			if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("起動不成立で仕事を取得しました: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	api, err := intake.New(config.Bootstrap{ServerURL: server.URL, WorkerID: "test", ServerTokenFile: "/unused"}, "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if err := runController(context.Background(), api, "test"); err == nil {
		t.Fatal("不正な設定を受け入れました")
	}
	if calls != 2 || status.Ready || status.Checks["ok"] != false || status.Checks["settingsVersion"] != "" {
		t.Fatalf("calls=%d status=%+v", calls, status)
	}
	items, ok := status.Checks["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatal(status.Checks)
	}
	failed := items[1].(map[string]any)
	if failed["name"] != "config" || failed["ok"] != false || failed["label"] == "" || failed["detail"] == "" {
		t.Fatal(failed)
	}
}

// 設定の更新による再起動と停止は、起動の失敗として受付へ送らない。
func TestConfigRestartIsNotStartupFailure(t *testing.T) {
	ctx := context.Background()
	if isStartupFailure(ctx, fmt.Errorf("周期: %w", workerruntime.ErrConfigChanged)) {
		t.Fatal("設定の更新による再起動を起動の失敗にしました")
	}
	if !isStartupFailure(ctx, errors.New("Webhookの配信設定がありません")) {
		t.Fatal("起動の失敗を送りません")
	}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if isStartupFailure(stopped, errors.New("x")) || isStartupFailure(ctx, nil) {
		t.Fatal("停止または成功を起動の失敗にしました")
	}
}
