package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyChildEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "inherited-secret")
	for _, tc := range []struct{ provider, key, want string }{
		{"claude", "test-key", "test-key"}, {"claude", "", "unset"},
		{"codex", "test-key", "unset"}, {"antigravity", "test-key", "unset"},
	} {
		t.Run(tc.provider+"_"+tc.want, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "runtime", "workspaces", "job")
			if err := os.MkdirAll(tree, 0700); err != nil {
				t.Fatal(err)
			}
			p := Policy{Root: filepath.Join(root, "runtime"), Tree: tree, TokenFile: filepath.Join(root, "private", "token")}
			capture := filepath.Join(root, "env")
			exe := filepath.Join(root, "fake")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s' \"${ANTHROPIC_API_KEY-unset}\" > '"+capture+"' || exit 1\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			settings := filepath.Join(root, "agy", "settings.json")
			if tc.provider == "antigravity" {
				if err := PrepareAntigravity(settings, p); err != nil {
					t.Fatal(err)
				}
			}
			answer, callErr := Call(context.Background(), Request{Settings: Settings{Provider: tc.provider}, Policy: p, Claude: exe, Codex: exe, Antigravity: exe, AntigravitySettings: settings, APIKey: tc.key})
			if callErr == nil || answer.APIKeyMode != (tc.provider == "claude" && tc.key != "") {
				t.Fatalf("mode=%v err=%v", answer.APIKeyMode, callErr)
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != tc.want {
				t.Fatalf("environment = %q, err = %v", got, err)
			}
		})
	}
}

// Exercise the shipped Call -> process.Run path, observing the actual child
// environment rather than inferring the mode from the request or usage values.
func TestAPIKeyModeCallBoundaries(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "inherited-secret")
	const key = "synthetic-api-mode-key"
	for _, withKey := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			started    bool
			successful bool
		}{
			{"success", true, true}, {"zero-cost", true, true}, {"missing-cost", true, true},
			{"nonzero", true, false}, {"invalid-result", true, false},
			{"text-limit", true, false}, {"structured-limit", true, false},
			{"review", true, true}, {"report-only", true, true},
			{"report-invalid-result", true, false}, {"report-session-mismatch", true, false},
			{"invalid-session", false, false}, {"invalid-report", false, false},
			{"session-save-failure", false, false}, {"schema-save-failure", false, false},
			{"canceled", false, false}, {"expired", false, false},
			{"cancel-after-session", false, false}, {"expire-after-session", false, false},
			{"unconfigured", false, false}, {"missing-cli", false, false},
			{"not-executable", false, false}, {"missing-directory", false, false},
		} {
			name := tc.name + "/subscription"
			if withKey {
				name = tc.name + "/api-key"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				capture := filepath.Join(root, "child-env")
				r := Request{Settings: Settings{Provider: "claude"}, Policy: Policy{Root: root, Tree: root}, Timeout: 5 * time.Second}
				if withKey {
					r.APIKey = key
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				report := strings.Replace(emptyAssumptionsReport, "report", key, 1)
				event := map[string]any{"type": "result", "structured_output": json.RawMessage(report), "usage": map[string]int{"input_tokens": 3, "output_tokens": 2}, "num_turns": 1, "total_cost_usd": 0.75}
				body := ""
				switch tc.name {
				case "zero-cost":
					event["total_cost_usd"] = 0
				case "missing-cost":
					delete(event, "total_cost_usd")
				case "nonzero":
					body = "printf '%s\\n' '" + key + " failed' >&2\nexit 7\n"
				case "invalid-result", "report-invalid-result":
					event["structured_output"] = map[string]string{"summary": key}
				case "text-limit":
					body = "echo 'credit balance is too low' >&2\nexit 1\n"
				case "structured-limit":
					event = map[string]any{"type": "result", "is_error": true, "result": "credit balance is too low"}
				case "review":
					r.Review = true
					event["structured_output"] = json.RawMessage(`{"decision":"approve","summary":"checked","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`)
				case "invalid-session":
					r.Session = "invalid session"
				case "invalid-report":
					r.ReportOnly = true
				case "session-save-failure":
					r.OnSession = func(string) error { return errors.New("cannot save session " + r.APIKey) }
				case "schema-save-failure":
					if err := os.WriteFile(filepath.Join(root, "control"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "canceled":
					cancel()
				case "expired":
					var stop context.CancelFunc
					ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer stop()
				case "expire-after-session":
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
					defer stop()
					r.OnSession = func(string) error {
						<-ctx.Done()
						return nil
					}
				case "cancel-after-session":
					r.OnSession = func(string) error { cancel(); return nil }
				case "missing-directory":
					r.Policy.Tree = filepath.Join(root, "missing")
				}
				if strings.HasPrefix(tc.name, "report-") {
					r.ReportOnly, r.Policy.ReadOnly, r.Session = true, true, "same-session"
					event["session_id"] = r.Session
					if tc.name == "report-session-mismatch" {
						event["session_id"] = "other-session"
					}
				}
				if body == "" {
					raw, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					// Preserve the existing decoded-string redaction path, including
					// a key whose first character is JSON-escaped by the CLI.
					encoded := strings.ReplaceAll(string(raw), key, `\u0073`+key[1:])
					body = "printf '%s\\n' '" + encoded + "'\n"
				}
				r.Claude = fakeCLI(t, "printf '%s' \"${ANTHROPIC_API_KEY-unset}\" > '"+capture+"' || exit 1\n"+body)
				switch tc.name {
				case "unconfigured":
					r.Claude = ""
				case "missing-cli":
					r.Claude = filepath.Join(root, "missing-cli")
				case "not-executable":
					if err := os.Chmod(r.Claude, 0600); err != nil {
						t.Fatal(err)
					}
				}
				answer, err := Call(ctx, r)
				if (err == nil) != tc.successful || answer.APIKeyMode != (withKey && tc.started) {
					t.Fatalf("mode=%v successful=%v err=%v", answer.APIKeyMode, tc.successful, err)
				}
				got, readErr := os.ReadFile(capture)
				if tc.started {
					want := "unset"
					if withKey {
						want = key
					}
					if readErr != nil || string(got) != want {
						t.Fatal("child environment disagrees with mode")
					}
				} else if !os.IsNotExist(readErr) {
					t.Fatalf("CLI ran before rejection: %v", readErr)
				}
				if strings.HasSuffix(tc.name, "limit") {
					var limit *UsageLimit
					if !errors.As(err, &limit) || limit.APIKey != withKey {
						t.Fatalf("limit semantics changed: %v", err)
					}
				}
				if tc.name == "canceled" || tc.name == "cancel-after-session" {
					if !errors.Is(err, context.Canceled) || answer.Outcome != "aborted" {
						t.Fatal("cancel semantics changed", err)
					}
				}
				if tc.name == "expired" || tc.name == "expire-after-session" {
					if !errors.Is(err, context.DeadlineExceeded) || answer.Outcome != "timeout" {
						t.Fatal("deadline semantics changed", err)
					}
				}
				if withKey {
					raw, marshalErr := json.Marshal(answer)
					if marshalErr != nil || strings.Contains(string(raw), key) || (err != nil && strings.Contains(err.Error(), key)) {
						t.Fatal("secret leaked in answer/error")
					}
				}
				if tc.name == "success" && withKey && !strings.Contains(string(answer.JSON), "[redacted]") {
					t.Fatal("escaped key was not redacted")
				}
				if tc.successful {
					if answer.Usage.Input == nil || *answer.Usage.Input != 3 || answer.Usage.Output == nil || *answer.Usage.Output != 2 || answer.Usage.Calls == nil || *answer.Usage.Calls != 1 || answer.Elapsed <= 0 || answer.Outcome != "completed" || (!r.Review && answer.Session == "") {
						t.Fatal("usage/session/elapsed/outcome changed")
					}
					wantCost, costMissing := 0.75, tc.name == "missing-cost"
					if tc.name == "zero-cost" {
						wantCost = 0
					}
					if (answer.Usage.CostUSD == nil) != costMissing || (!costMissing && *answer.Usage.CostUSD != wantCost) {
						t.Fatal("cost semantics changed")
					}
				}
			})
		}
	}
}

func TestAPIKeyModeAfterObservedStartInterruption(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		for _, kind := range []string{"cancel", "deadline"} {
			name := kind + "/subscription"
			if withKey {
				name = kind + "/api-key"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				ready := filepath.Join(root, "ready")
				r := Request{Settings: Settings{Provider: "claude"}, Policy: Policy{Root: root, Tree: root}, Timeout: 5 * time.Second}
				if withKey {
					r.APIKey = "synthetic-interruption-key"
				}
				r.Claude = fakeCLI(t, "printf '%s' \"${ANTHROPIC_API_KEY-unset}\" > '"+ready+"' || exit 1\nexec /bin/sleep 60\n")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type result struct {
					answer Answer
					err    error
				}
				done := make(chan result, 1)
				go func() {
					answer, err := Call(ctx, r)
					done <- result{answer, err}
				}()
				// A child-written file proves start; polling time never proves it.
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					raw, err := os.ReadFile(ready)
					want := r.APIKey
					if want == "" {
						want = "unset"
					}
					if err == nil && string(raw) == want {
						break
					}
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					select {
					case <-ticker.C:
					case got := <-done:
						t.Fatalf("CLI ended without observed start: %v", got.err)
					}
				}
				wantErr, outcome := context.DeadlineExceeded, "timeout"
				if kind == "cancel" {
					cancel()
					wantErr, outcome = context.Canceled, "aborted"
				}
				got := <-done
				if !errors.Is(got.err, wantErr) || got.answer.APIKeyMode != withKey || got.answer.Outcome != outcome || got.answer.Elapsed <= 0 {
					t.Fatalf("mode=%v outcome=%s err=%v", got.answer.APIKeyMode, got.answer.Outcome, got.err)
				}
			})
		}
	}
}

func TestAPIKeyUsageLimitsAndRedaction(t *testing.T) {
	const key = "test-secret-key"
	for _, structured := range []bool{false, true} {
		for _, withKey := range []bool{false, true} {
			root := t.TempDir()
			exe := filepath.Join(root, "claude")
			script := "#!/bin/sh\necho 'credit balance is too low' >&2\nexit 1\n"
			if structured {
				script = "#!/bin/sh\necho '{\"type\":\"result\",\"is_error\":true,\"result\":\"credit balance is too low\"}'\n"
			}
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			req := Request{Settings: Settings{Provider: "claude"}, Policy: Policy{Root: root, Tree: root}, Claude: exe}
			if withKey {
				req.APIKey = key
			}
			_, err := Call(context.Background(), req)
			var limit *UsageLimit
			if !errors.As(err, &limit) || limit.APIKey != withKey {
				t.Fatalf("limit = %#v, err = %v", limit, err)
			}
		}
	}
	root := t.TempDir()
	exe := filepath.Join(root, "claude")
	report := strings.Replace(emptyAssumptionsReport, "report", key, 1)
	event, _ := json.Marshal(map[string]any{"type": "result", "structured_output": json.RawMessage(report)})
	script := "#!/bin/sh\nprintf '%s\\n' '" + claudeLimitEvent + "' '" + string(event) + "'\n"
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	req := Request{Settings: Settings{Provider: "claude"}, Policy: Policy{Root: root, Tree: root}, Claude: exe, APIKey: key}
	answer, err := Call(context.Background(), req)
	if err != nil || strings.Contains(string(answer.JSON), key) || !strings.Contains(string(answer.JSON), "[redacted]") || len(answer.Limits) != 0 {
		t.Fatalf("answer %s, limits %v, err %v", answer.JSON, answer.Limits, err)
	}
	req.APIKey = ""
	answer, err = Call(context.Background(), req)
	if err != nil || len(answer.Limits) == 0 {
		t.Fatalf("subscription limits %v, err %v", answer.Limits, err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho '"+key+" failed' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	req.APIKey = key
	_, err = Call(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v", err)
	}
	req.OnSession = func(string) error { return &ReportOnlyError{Err: errors.New(key)} }
	_, err = Call(context.Background(), req)
	var reportErr *ReportOnlyError
	if !errors.As(err, &reportErr) || strings.Contains(err.Error(), key) {
		t.Fatalf("report error = %v", err)
	}
}

func TestClaudePolicyDeniesProc(t *testing.T) {
	var cfg struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte((Policy{}).ClaudeSettings()), &cfg); err != nil {
		t.Fatal(err)
	}
	for _, rule := range cfg.Permissions.Deny {
		if rule == "Read(//proc/**)" {
			return
		}
	}
	t.Fatal("/proc read rule missing")
}
