package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
)

func TestClaudeAPIKeySameConversationRetry(t *testing.T) {
	for _, scenario := range []string{"success", "retry-save-failure", "failure", "limit", "credit", "missing-key", "key-held", "no-session", "cancel-first", "cancel-retry", "cancel-running-retry", "hold-save-failure", "marker-save-failure", "usage-refused", "usage-unavailable", "key-held-before-retry", "expired-before-retry"} {
		t.Run(scenario, func(t *testing.T) {
			f, d, r, key := claudeKeyCallFixture(t)
			e := f.Executor
			tr := completionAPI(t, f)
			originalStoreRoot := e.Store.Root
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			until := time.Now().Add(time.Hour).Truncate(time.Second)
			capture := filepath.Join(t.TempDir(), "calls")
			if scenario == "cancel-running-retry" {
				fifo := capture + ".ready"
				if err := syscall.Mkfifo(fifo, 0600); err != nil {
					t.Fatal(err)
				}
				go func() {
					ready, err := os.Open(fifo)
					if err == nil {
						var b [1]byte
						_, _ = io.ReadFull(ready, b[:])
						_ = ready.Close()
					}
					cancel()
				}()
			}
			// A fresh Claude session is persisted by the real driver before invocation.
			r.Session = ""
			if scenario == "missing-key" {
				if err := os.Remove(filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "key-held" {
				e.holds.APIKeyHold = &apiKeyHold{Until: until.UnixMilli()}
			}
			if scenario == "usage-refused" {
				tr.reason = "missing usage"
			}
			if scenario == "usage-unavailable" {
				tr.fail = true
			}
			script := "printf '%s\\n' \"${ANTHROPIC_API_KEY+key}\" >> '" + capture + "' || exit 1\nprintf '%s\\n' \"$@\" >> '" + capture + ".args' || exit 1\n" +
				"if [ -z \"${ANTHROPIC_API_KEY-}\" ]; then\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"usage\":{\"input_tokens\":10,\"output_tokens\":2},\"result\":\"usage limit reached|" + strconv.FormatInt(until.Unix(), 10) + "\"}'\nexit 1\nfi\n"
			switch scenario {
			case "cancel-running-retry":
				script += "printf x > '" + capture + ".ready' || exit 1\nexec /bin/sleep 600\n"
			case "failure":
				script += "printf '%s\\n' '{\"usage\":{\"input_tokens\":20,\"output_tokens\":3}}'\necho \"$ANTHROPIC_API_KEY failed\" >&2\nexit 1\n"
			case "limit", "credit":
				message := "usage limit resets in 2 hours"
				if scenario == "credit" {
					message = "credit balance is too low"
				}
				script += "printf '%s\\n' '{\"usage\":{\"input_tokens\":20,\"output_tokens\":3}}'\necho \"$ANTHROPIC_API_KEY " + message + "\" >&2\nexit 1\n"
			default:
				event := encode(map[string]any{"type": "result", "usage": map[string]int{"input_tokens": 20, "output_tokens": 3}, "structured_output": json.RawMessage(reportWith("summary", key))})
				script += "printf '%s\\n' '" + string(event) + "'\n"
			}
			writeClaudeCallCLI(t, e, script)
			calls := 0
			var firstSession string
			var originalMarker json.RawMessage
			e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				if calls == 2 {
					if req.APIKey != key || req.Session == "" || req.Session != firstSession || req.ReportOnly || req.Review {
						t.Fatal("retry lost authentication/session/implementation contract")
					}
					saved, err := e.Store.Load(r.ID)
					if err != nil || len(saved.Usage) != 1 || len(saved.ProviderHold) == 0 || saved.Runs != 1 {
						t.Fatal("retry started before original usage and hold were saved", err)
					}
					originalMarker = append(json.RawMessage(nil), saved.ProviderHold...)
					if scenario == "cancel-retry" {
						cancel()
					}
				}
				if scenario == "no-session" {
					// Simulate a driver which cannot secure a resumable conversation.
					req.OnSession = nil
				}
				answer, err := providers.Call(ctx, req)
				if calls == 2 && scenario == "retry-save-failure" {
					e.Store.Root = filepath.Join(t.TempDir(), "missing")
				}
				if calls == 1 {
					firstSession = r.Session
					switch scenario {
					case "no-session":
						answer.Session = ""
					case "cancel-first":
						cancel()
					case "hold-save-failure":
						if err := os.Remove(e.holdFile()); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(e.holdFile(), 0700); err != nil {
							t.Fatal(err)
						}
					case "marker-save-failure":
						r.ID = 0
					}
				}
				return answer, err
			}
			// Exercise a hold change during usage checks, before the retry's final selection.
			if scenario == "expired-before-retry" || scenario == "key-held-before-retry" {
				api, apiErr := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), keyRetryTransitionTransport{next: tr.next, after: func() {
					e.holdsMu.Lock()
					defer e.holdsMu.Unlock()
					if scenario == "expired-before-retry" {
						e.holds.Usage["claude"] = providerHold{Until: time.Now().Add(-time.Second).UnixMilli()}
					} else {
						e.holds.APIKeyHold = &apiKeyHold{Until: until.UnixMilli()}
					}
				}})
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				tr.next = api
			}
			answer, err := e.callAIOnce(ctx, d, r, "fixture", false, nil)
			e.Store.Root = originalStoreRoot
			if scenario == "retry-save-failure" {
				saved, loadErr := e.Store.Load(r.ID)
				if err == nil || calls != 2 || len(r.ProviderHold) == 0 || loadErr != nil || len(saved.ProviderHold) == 0 || len(saved.Usage) != 1 || r.Runs != 1 {
					t.Fatal("failed retry save lost original hold or retried again", err, loadErr)
				}
				return
			}
			wantCalls := 1
			if scenario == "success" || scenario == "failure" || scenario == "limit" || scenario == "credit" || scenario == "cancel-retry" || scenario == "cancel-running-retry" {
				wantCalls = 2
			}
			if calls != wantCalls || (err == nil) != (scenario == "success") || r.Runs != 1 {
				t.Fatal("retry result/count mismatch", scenario, calls, err, r.Runs)
			}
			if scenario == "cancel-first" || scenario == "cancel-retry" || scenario == "cancel-running-retry" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			}
			if scenario == "hold-save-failure" || scenario == "marker-save-failure" {
				var limit *providers.UsageLimit
				if errors.As(err, &limit) {
					t.Fatal("persistence error replaced with original limit")
				}
				return
			}
			if scenario != "expired-before-retry" && e.holds.Usage["claude"].Until != until.UnixMilli() {
				t.Fatal("API call extended subscription hold")
			}
			saved, loadErr := e.Store.Load(r.ID)
			if loadErr != nil || len(saved.Usage) != wantCalls || saved.Runs != 1 {
				t.Fatal("usage/run persistence mismatch", loadErr)
			}
			if scenario == "success" {
				if len(r.ProviderHold) != 0 || len(saved.ProviderHold) != 0 || answer.Session != firstSession || e.CanAcquire() == false {
					t.Fatal("successful retry kept job hold or changed conversation")
				}
			} else if len(saved.ProviderHold) == 0 || len(originalMarker) > 0 && string(saved.ProviderHold) != string(originalMarker) {
				t.Fatal("failure lost original subscription marker")
			}
			if scenario == "limit" || scenario == "credit" {
				if e.holds.APIKeyHold == nil || e.holds.APIKeyHold.Until != until.UnixMilli() || e.CanAcquire() {
					t.Fatal("key limit did not stop intake")
				}
				again := &Executor{Config: e.Config}
				if err := again.loadHostHolds(); err != nil || again.CanAcquire() {
					t.Fatal("key hold not restored", err)
				}
			}
			var first, second struct {
				Input      int64 `json:"inputTokens"`
				Output     int64 `json:"outputTokens"`
				Generation int   `json:"generation"`
			}
			_ = json.Unmarshal(saved.Usage[0], &first)
			if first.Input != 10 || first.Output != 2 || first.Generation != 1 {
				t.Fatal("first usage not retained")
			}
			if wantCalls == 2 && scenario != "cancel-retry" && scenario != "cancel-running-retry" {
				_ = json.Unmarshal(saved.Usage[1], &second)
				if second.Input != 20 || second.Output != 3 || second.Generation != 1 || saved.WeightedUsage != 55 {
					t.Fatal("second usage not separately retained", saved.WeightedUsage)
				}
			}
			for _, raw := range []string{string(encode(saved)), string(encode(e.holds)), string(answer.JSON)} {
				if strings.Contains(raw, key) {
					t.Fatal("credential leaked into persisted record or result")
				}
			}
			if err != nil && strings.Contains(err.Error(), key) {
				t.Fatal("credential leaked into error")
			}
			if wantCalls == 2 && scenario != "cancel-retry" {
				args, readErr := os.ReadFile(capture + ".args")
				if readErr != nil || strings.Count(string(args), "--session-id\n") != 1 || !strings.Contains(string(args), "--resume\n"+firstSession+"\n") {
					t.Fatal("CLI did not resume original conversation", readErr)
				}
			}
		})
	}
}

// Change shared hold state while the existing usage decision is in flight.
type keyRetryTransitionTransport struct {
	next  *intake.Client
	after func()
}

func (tr keyRetryTransitionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	out, err := tr.next.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(r.URL.Path, "/usage") {
		tr.after()
	}
	return &http.Response{StatusCode: out.Status, Body: io.NopCloser(strings.NewReader(string(out.Body))), Request: r}, nil
}
