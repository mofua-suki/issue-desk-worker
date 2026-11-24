package execution

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
)

func distributedKeyExecutor(t *testing.T, handler http.HandlerFunc) *Executor {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	api, err := intake.New(config.Bootstrap{ServerURL: server.URL, ServerTokenFile: "test", WorkerID: "test"}, "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{API: api, Config: Configuration{Fleet: Fleet{WorkerTokenFile: filepath.Join(t.TempDir(), "token")}}}
}

func expireDistributedKey(e *Executor) {
	e.distributedKey.mu.Lock()
	e.distributedKey.next = time.Time{}
	e.distributedKey.mu.Unlock()
}

func TestDistributedClaudeAPIKeyLifecycle(t *testing.T) {
	key := "sk-ant-" + strings.Repeat("d", 24)
	var calls atomic.Int32
	var body atomic.Value
	body.Store(map[string]any{"state": "registered", "version": "v1", "key": key})
	e := distributedKeyExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/hosts/test/claude-api-key" || r.Header.Get("Authorization") != "Bearer idk_"+strings.Repeat("a", 43) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body.Load())
	})
	if got := e.claudeAPIKey(); got != key {
		t.Fatal("distributed key was not used without a local file")
	}
	if got := e.claudeAPIKey(); got != key || calls.Load() != 1 {
		t.Fatal("key was fetched again before the refresh interval", calls.Load())
	}
	body.Store(map[string]any{"state": "cleared", "version": "v2", "key": nil})
	expireDistributedKey(e)
	if e.claudeAPIKey() != "" {
		t.Fatal("cleared key was still used")
	}
	body.Store(map[string]any{"state": "registered", "version": "v3", "key": key})
	expireDistributedKey(e)
	if e.claudeAPIKey() != key {
		t.Fatal("re-registered key was not used")
	}
	body.Store(map[string]any{"state": "unregistered", "version": nil, "key": nil})
	expireDistributedKey(e)
	if e.claudeAPIKey() != "" {
		t.Fatal("unregistered state returned a key")
	}
}

func TestDistributedClaudeAPIKeyLocalFileWins(t *testing.T) {
	var calls atomic.Int32
	e := distributedKeyExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "registered", "version": "v1", "key": "sk-ant-" + strings.Repeat("d", 24)})
	})
	local := "sk-ant-" + strings.Repeat("l", 24)
	path := filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key")
	if err := os.WriteFile(path, []byte(local), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if e.claudeAPIKey() != local || calls.Load() != 0 {
		t.Fatal("local key file did not take precedence", calls.Load())
	}
}

func TestDistributedClaudeAPIKeyFailuresMeanNoKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"old intake without the route", 404, `{}`},
		{"unavailable", 503, `{"code":"TEMPORARY_UNAVAILABLE"}`},
		{"forbidden", 403, `{}`},
		{"registered without key", 200, `{"state":"registered","version":"v1","key":null}`},
		{"cleared with key", 200, `{"state":"cleared","version":"v1","key":"sk-ant-` + strings.Repeat("d", 24) + `"}`},
		{"invalid key format", 200, `{"state":"registered","version":"v1","key":"sk-ant-short"}`},
		{"oversize key", 200, `{"state":"registered","version":"v1","key":"sk-ant-` + strings.Repeat("d", 4090) + `"}`},
		{"unknown state", 200, `{"state":"other","version":null,"key":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			e := distributedKeyExecutor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			// The second call must reuse the failure instead of asking again.
			if first, second := e.claudeAPIKey(), e.claudeAPIKey(); first != "" || second != "" {
				t.Fatal("failure returned a key")
			}
			if calls.Load() != 1 {
				t.Fatal("failure was retried immediately", calls.Load())
			}
		})
	}
}

func TestDistributedClaudeAPIKeyOnlyDuringSubscriptionHold(t *testing.T) {
	key := "sk-ant-" + strings.Repeat("d", 24)
	var calls atomic.Int32
	e := distributedKeyExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "registered", "version": "v1", "key": key})
	})
	e.holds.Usage = map[string]providerHold{}
	if e.claudeAPIKeyForCall("claude") != "" || calls.Load() != 0 {
		t.Fatal("key was read while the subscription was available")
	}
	e.holds.Usage["claude"] = providerHold{Until: time.Now().Add(time.Hour).UnixMilli()}
	if e.claudeAPIKeyForCall("codex") != "" || e.claudeAPIKeyForCall("claude") != key {
		t.Fatal("distributed key was not used after the subscription limit")
	}
	if e.intakeHold(time.Now()) != nil {
		t.Fatal("claude stayed held although a distributed key is available")
	}
}

func TestDistributedClaudeAPIKeyDoesNotBlockConcurrentCallers(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	e := distributedKeyExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "registered", "version": "v1", "key": "sk-ant-" + strings.Repeat("d", 24)})
	})
	done := make(chan string)
	go func() { done <- e.claudeAPIKey() }()
	<-entered
	start := time.Now()
	if e.claudeAPIKey() != "" || time.Since(start) > time.Second {
		t.Fatal("a concurrent caller waited for the in-flight request")
	}
	close(release)
	if <-done == "" || calls.Load() != 1 {
		t.Fatal("in-flight request result was lost or duplicated", calls.Load())
	}
}
