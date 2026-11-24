package intake

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-worker/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	c, err := New(config.Bootstrap{ServerURL: server.URL, ServerTokenFile: "test", WorkerID: "test"}, "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestNextContractAndNoRetry(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/jobs/next" || r.Method != "POST" || r.Header.Get(ContractHeader) != ContractVersion || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer idk_") {
			t.Error("API契約が不一致")
		}
		var b map[string]int
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b["capacity"] != 1 {
			t.Error("capacity不一致")
		}
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"secret idk_`+strings.Repeat("a", 43)+`"}`)
	})
	_, err := c.Next(context.Background(), 1)
	if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "idk_") {
		t.Fatal("nextを再送したか秘密が漏れました", calls.Load(), err)
	}
}
func TestRedirectNeverLeaksToken(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	if _, err := c.Config(context.Background()); err == nil {
		t.Fatal("転送を受理しました")
	}
	if calls.Load() != 0 {
		t.Fatal("転送先に要求しました")
	}
}
func TestConfigAndHeartbeat(t *testing.T) {
	var seen atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/client-config":
			if r.URL.Query().Get("host") != "test" {
				t.Error("host照合なし")
			}
			_, _ = io.WriteString(w, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{"assignments":{}}}`)
		case "/api/v1/hosts/test":
			seen.Add(1)
			if r.Method != "PUT" {
				t.Error("heartbeat method")
			}
			var b HostStatus
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Ready || b.Capacity != 1 {
				t.Error("heartbeat値不一致")
			}
			_, _ = io.WriteString(w, `{"host":"test"}`)
		default:
			t.Error("予期しない経路", r.URL.Path)
		}
	})
	cfg, err := c.Config(context.Background())
	if err != nil || cfg.Version != "v" {
		t.Fatal(cfg, err)
	}
	if err := c.Heartbeat(context.Background(), HostStatus{Capacity: 1, Ready: false}); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 1 {
		t.Fatal("heartbeatなし")
	}
}
func TestLeaseFencingAndState(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["epoch"]) != "7" {
			t.Error("epochを保持していません")
		}
		if strings.HasSuffix(r.URL.Path, "/lease") {
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"code":"lease_lost","error":"secret"}`)
		} else if strings.HasSuffix(r.URL.Path, "/state") {
			if string(body["status"]) != `"running"` {
				t.Error("状態不一致")
			}
			w.WriteHeader(204)
		} else {
			t.Error(r.URL.Path)
		}
	})
	_, err := c.Renew(context.Background(), 2, 7)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.LeaseLost() {
		t.Fatal("lease喪失を区別できません", err)
	}
	if err := c.State(context.Background(), 2, 7, State{Status: "running"}); err != nil {
		t.Fatal(err)
	}
}
func TestAssignmentValidationAndRawJob(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var a Assignment
	if err := json.Unmarshal([]byte(`{"job":{"id":2,"repo":"a/b","payload":{"nested":true}},"lease":{"epoch":7,"expiresAt":"2026-11-01T00:01:00Z"}}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.Job.Raw), `"payload"`) {
		t.Fatal("未知のジョブ項目が消えました")
	}
	if err := a.Validate(now.Add(time.Minute)); err == nil {
		t.Fatal("期限境界のleaseを受理しました")
	}
}
func TestConfigRejectsMissingSnapshot(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{}`) })
	if _, err := c.Config(context.Background()); err == nil {
		t.Fatal("欠けた設定を受理しました")
	}
}
func TestCancellation(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Next(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNoteContract(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/api/v1/jobs/42/notes" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Epoch int64          `json:"epoch"`
			Kind  string         `json:"kind"`
			Data  map[string]any `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Epoch != 3 || body.Kind != "finding" || body.Data["text"] != "review" {
			t.Errorf("body: %+v, %v", body, err)
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	if err := c.Note(context.Background(), 42, 3, "finding", map[string]any{"text": "review"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, epoch int64
		kind      string
	}{{0, 1, "finding"}, {42, 0, "finding"}, {42, 1, "other"}} {
		if err := c.Note(context.Background(), tc.id, tc.epoch, tc.kind, map[string]any{}); err == nil {
			t.Fatal("invalid input accepted", tc)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid request sent", calls.Load())
	}
}
