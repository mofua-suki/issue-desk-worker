package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func claudeKeyCallFixture(t *testing.T) (*pipelineFixture, Descriptor, *journal.Record, string) {
	t.Helper()
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	e := f.Executor
	e.Config.WorkerTokenFile = filepath.Join(t.TempDir(), "token")
	key := strings.Join([]string{"sk", "ant", strings.Repeat("fixture", 4)}, "-")
	if err := os.WriteFile(filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key"), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	_ = json.Unmarshal(f.Job.Raw, &d)
	d.Provider = "claude"
	r := &journal.Record{ID: 1, Epoch: 3, Repo: d.Repo, Issue: d.Issue, Provider: "claude", Session: "implementation-session", Runs: 1, Tree: f.root}
	e.Call = providers.Call
	return f, d, r, key
}

func setClaudeCallHold(t *testing.T, e *Executor, active bool) providerHold {
	t.Helper()
	until := time.Now().Add(time.Hour)
	if !active {
		until = time.Now().Add(-time.Hour)
	}
	h := providerHold{Until: until.UnixMilli(), Since: time.Now().UTC().Format(time.RFC3339Nano), Source: "cli", Summary: "subscription hold"}
	e.holdsMu.Lock()
	e.holds.Usage["claude"] = h
	err := e.saveHoldsLocked()
	e.holdsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func writeClaudeCallCLI(t *testing.T, e *Executor, body string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	e.Config.Claude, e.Config.Codex = exe, exe
	return exe
}

func TestClaudeAPIKeyCallSelectionChildEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "inherited-key-must-be-removed")
	for _, entry := range []string{"implementation", "review", "completion"} {
		for _, state := range []string{"held", "no-hold", "missing-key", "invalid-key", "key-held", "expired-key-hold", "expired", "codex", "antigravity", "disabled-to-codex", "disabled-to-claude"} {
			// Completion does not admit these providers/switches; the existing
			// TestReportCompletionBudgetAndIsolationGuards covers those rejections.
			if entry == "completion" && (state == "antigravity" || strings.HasPrefix(state, "disabled-")) {
				continue
			}
			t.Run(entry+"/"+state, func(t *testing.T) {
				f, d, r, key := claudeKeyCallFixture(t)
				e := f.Executor
				var request providers.Request
				if entry == "completion" {
					if state == "codex" {
						r.Provider = "codex"
					}
					if err := e.receiveImplementationReport(r, implementationFixture(), "implement_report"); err != nil {
						t.Fatal(err)
					}
					var err error
					request, err = e.reportCompletionRequest(context.Background(), d, r, errors.New("report data"))
					if err != nil {
						t.Fatal(err)
					}
					request.APIKey = "stale-key" // Selection must happen at dispatch, not construction.
				}
				original := setClaudeCallHold(t, e, state != "no-hold" && state != "expired")
				if state == "no-hold" {
					delete(e.holds.Usage, "claude")
					original = providerHold{}
				}
				if state == "missing-key" {
					if err := os.Remove(filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key")); err != nil {
						t.Fatal(err)
					}
				}
				if state == "invalid-key" {
					if err := os.Chmod(filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if state == "key-held" || state == "expired-key-hold" {
					until := original.Until
					if state == "expired-key-hold" {
						until = time.Now().Add(-time.Hour).UnixMilli()
					}
					e.holds.APIKeyHold = &apiKeyHold{Until: until}
				}
				provider := "claude"
				if state == "codex" || state == "antigravity" {
					provider = state
				}
				if strings.HasPrefix(state, "disabled-") {
					tr := completionAPI(t, f)
					tr.disabled = state == "disabled-to-claude"
					if state == "disabled-to-claude" {
						provider = "codex"
					} else {
						// A fresh config setting disables Claude at the actual call entrance.
						e.API = claudeDisabledAPI(t, e.API)
					}
				}
				d.Provider, r.Provider = provider, provider
				review := entry == "review"
				if review {
					if provider == "claude" {
						r.Provider = "codex"
					} else {
						r.Provider = "claude"
					}
				}
				capture := filepath.Join(t.TempDir(), "environment")
				exe := writeClaudeCallCLI(t, e, "printf '%s' \"${ANTHROPIC_API_KEY-unset}\" > '"+capture+"'\nprintf '%s\\n' '{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}'\necho 'fixture failure' >&2\nexit 1\n")
				if provider == "antigravity" && !review {
					settings := filepath.Join(t.TempDir(), "settings.json")
					if err := json.Unmarshal(encode(map[string]any{"antigravity": map[string]string{"command": exe, "settingsFile": settings, "model": "fixture"}}), &e.Config.Fleet); err != nil {
						t.Fatal(err)
					}
					if err := providers.PrepareAntigravity(settings, providers.Policy{Root: e.Config.RuntimeDir, Tree: r.Tree, TokenFile: e.Config.WorkerTokenFile}); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if entry == "completion" {
					request.Claude, request.Codex = exe, exe
					_, err = e.callReportCompletion(context.Background(), d, r, request)
				} else {
					_, err = e.callAIOnce(context.Background(), d, r, "fixture", review, nil)
				}
				if err == nil {
					t.Fatal("fake CLI failure disappeared")
				}
				wantKey := state == "held" || state == "expired-key-hold" || state == "disabled-to-claude"
				got, readErr := os.ReadFile(capture)
				if readErr != nil || (string(got) == key) != wantKey || !wantKey && string(got) != "unset" {
					t.Fatal("unexpected child credential selection", readErr)
				}
				if e.holds.Usage["claude"] != original {
					t.Fatal("call changed subscription hold")
				}
				if state == "missing-key" && e.CanAcquire() || state == "key-held" && e.CanAcquire() {
					t.Fatal("unavailable key bypassed intake hold")
				}
			})
		}
	}
}

type claudeDisabledTransport struct{ next *intake.Client }

func (tr claudeDisabledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	out, err := tr.next.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
	if err != nil {
		return nil, err
	}
	if r.URL.Path == "/api/v1/client-config" {
		var snapshot intake.ConfigSnapshot
		_ = json.Unmarshal(out.Body, &snapshot)
		snapshot.Settings = encode(map[string]any{"providerDisable.claude": map[string]any{"until": "2099-01-01T00:00:00Z"}})
		out.Body = encode(snapshot)
	}
	return &http.Response{StatusCode: out.Status, Body: io.NopCloser(strings.NewReader(string(out.Body))), Request: r}, nil
}

func claudeDisabledAPI(t *testing.T, next *intake.Client) *intake.Client {
	t.Helper()
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), claudeDisabledTransport{next})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func TestClaudeAPIKeyCallRechecksHoldsAfterRead(t *testing.T) {
	for _, change := range []string{"key-hold", "subscription-expired"} {
		t.Run(change, func(t *testing.T) {
			e := &Executor{holds: hostHolds{Usage: map[string]providerHold{"claude": {Until: time.Now().Add(time.Hour).UnixMilli()}}}}
			reading, resume := make(chan struct{}), make(chan struct{})
			result := make(chan string, 1)
			go func() {
				result <- e.claudeAPIKeyForCallWithReader("claude", func() string {
					close(reading)
					<-resume
					return "fixture-key"
				})
			}()
			<-reading
			e.holdsMu.Lock()
			if change == "key-hold" {
				e.holds.APIKeyHold = &apiKeyHold{Until: time.Now().Add(time.Hour).UnixMilli()}
			} else {
				e.holds.Usage["claude"] = providerHold{Until: time.Now().Add(-time.Second).UnixMilli()}
			}
			e.holdsMu.Unlock()
			close(resume)
			if <-result != "" {
				t.Fatal("hold update during key read was ignored")
			}
		})
	}
}

func TestClaudeAPIKeyCallReturnsToSubscription(t *testing.T) {
	f, d, r, _ := claudeKeyCallFixture(t)
	e := f.Executor
	capture := filepath.Join(t.TempDir(), "environment")
	event := encode(map[string]any{"type": "result", "structured_output": json.RawMessage(implementationFixture())})
	writeClaudeCallCLI(t, e, "printf '%s\\n' \"${ANTHROPIC_API_KEY+key}\" >> '"+capture+"' || exit 1\nprintf '%s\\n' '"+string(event)+"'\n")
	setClaudeCallHold(t, e, true)
	if _, err := e.callAIOnce(context.Background(), d, r, "fixture", false, nil); err != nil {
		t.Fatal(err)
	}
	setClaudeCallHold(t, e, false)
	if _, err := e.callAIOnce(context.Background(), d, r, "fixture", false, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != "key\n\n" || len(r.Usage) != 2 {
		t.Fatal("next call did not return to subscription", err)
	}
	for _, raw := range r.Usage {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil || row["usageMissingReason"] != "usage-unavailable" {
			t.Fatal("missing usage was changed", err)
		}
	}
}

func TestClaudeAPIKeyCallLimitDoesNotExtendSubscription(t *testing.T) {
	f, d, r, _ := claudeKeyCallFixture(t)
	e := f.Executor
	original := setClaudeCallHold(t, e, true)
	calls := 0
	writeClaudeCallCLI(t, e, "echo 'usage limit resets in 2 hours' >&2\nexit 1\n")
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		return providers.Call(ctx, req)
	}
	_, err := e.callAIOnce(context.Background(), d, r, "fixture", false, nil)
	var limit *providers.UsageLimit
	if !errors.As(err, &limit) || !limit.APIKey || calls != 1 || e.holds.Usage["claude"] != original || e.holds.APIKeyHold == nil || e.CanAcquire() || len(r.ProviderHold) == 0 {
		t.Fatal("direct key limit routing failed", err)
	}
	_, err = e.callAIOnce(context.Background(), d, r, "fixture", false, nil)
	if !errors.As(err, &limit) || limit.APIKey || calls != 2 {
		t.Fatal("held key used on following call", err)
	}
}
