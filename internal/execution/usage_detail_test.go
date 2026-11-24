package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/jobspec"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/providers"
)

func TestProviderSamplesReachSavedUsage(t *testing.T) {
	for _, tc := range []struct {
		name, stream         string
		calls, input, cached any
		partial              bool
		weighted             float64
	}{
		{"codex", `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2,"cached_input_tokens":0}}
{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":3}}`, float64(2), float64(20), nil, true, 35},
		{"claude", `{"type":"result","num_turns":3,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0},"result":"invalid"}`, float64(3), float64(10), float64(0), false, 20},
		{"claude", `{"type":"result","num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0}}`, float64(0), float64(0), float64(0), false, 0},
		{"claude", `{"type":"result","num_turns":-1,"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0}}`, nil, float64(10), float64(0), false, 20},
		{"claude", `{"type":"result","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0}}`, nil, float64(10), float64(0), false, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			dir := t.TempDir()
			exe := filepath.Join(dir, "provider")
			script := "#!/bin/sh\ncat <<'FIXTURE'\n" + tc.stream + "\nFIXTURE\n"
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			req := providers.Request{Settings: providers.Settings{Provider: tc.name}, Policy: providers.Policy{Root: dir, Tree: dir}, Codex: exe, Claude: exe, Prompt: "private prompt sentinel"}
			answer, _ := providers.Call(context.Background(), req) // The fixture intentionally omits a valid answer body.
			if err := e.recordUsage(r, d, req.Settings, "test", answer); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[len(saved.Usage)-1], &row); err != nil {
				t.Fatal(err)
			}
			if row["calls"] != tc.calls || row["inputTokens"] != tc.input || row["cacheReadTokens"] != tc.cached || (row["usagePartial"] == true) != tc.partial || saved.WeightedUsage != tc.weighted {
				t.Fatal(row, saved.WeightedUsage)
			}
			if row["contextMaxTokens"] != nil || row["contextTotalTokens"] != nil || row["usageMissingReason"] != nil {
				t.Fatal(row)
			}
			if strings.Contains(string(saved.Usage[len(saved.Usage)-1]), "invalid") || strings.Contains(string(saved.Usage[len(saved.Usage)-1]), "private prompt sentinel") {
				t.Fatal("prompt or answer body recorded")
			}
		})
	}
}

func TestUsageRowInvalidAndMeasuredZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   int64
		want    any
		reason  any
		partial bool
	}{
		// The intake validator accepts usage-unavailable together with usagePartial=true.
		{"invalid", -1, nil, "usage-unavailable", true},
		{"zero", 0, float64(0), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			v := tc.value
			u := providers.Usage{Input: &v, Output: &v, Cached: &v, CacheWrite: &v, Calls: &v, ContextMaxTokens: &v, ContextTotalTokens: &v}
			if err := e.recordUsage(r, d, providers.Settings{Provider: "codex"}, "test", providers.Answer{Usage: u}); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[0], &row); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "calls", "contextMaxTokens", "contextTotalTokens"} {
				if row[key] != tc.want {
					t.Fatalf("%s: %v", key, row)
				}
			}
			if row["usageMissingReason"] != tc.reason || (row["usagePartial"] == true) != tc.partial || saved.WeightedUsage != 0 {
				t.Fatal(row, saved.WeightedUsage)
			}
		})
	}
}

func TestCodexCallsOnProcessExitReachUsageRow(t *testing.T) {
	for _, tc := range []struct {
		name, stream, exit string
		want               any
	}{
		{"failed before completion", `{"type":"turn.started"}`, "1", nil},
		{"successful with no turns", `{"type":"thread.started"}`, "0", float64(0)},
		{"failed after completion", "{\"type\":\"turn.completed\"}\n{\"type\":\"turn.started\"}", "1", float64(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			dir := t.TempDir()
			exe := filepath.Join(dir, "provider")
			script := "#!/bin/sh\ncat <<'FIXTURE'\n" + tc.stream + "\nFIXTURE\nexit " + tc.exit + "\n"
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			settings := providers.Settings{Provider: "codex"}
			answer, _ := providers.Call(context.Background(), providers.Request{Settings: settings, Policy: providers.Policy{Root: dir, Tree: dir}, Codex: exe})
			if err := e.recordUsage(r, d, settings, "test", answer); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[0], &row); err != nil {
				t.Fatal(err)
			}
			if row["calls"] != tc.want {
				t.Fatalf("calls: got %v, want %v", row["calls"], tc.want)
			}
		})
	}
}

func TestSavedUsageGenerationAndCost(t *testing.T) {
	for _, tc := range []struct {
		name, provider, stream    string
		runs, reports, unanswered int
		generation                float64
		cost                      any
	}{
		{"first claude", "claude", "{\"type\":\"assistant\",\"usage\":{\"input_tokens\":1}}\n{\"type\":\"result\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1},\"total_cost_usd\":0.25}", 1, 0, 0, 1, float64(0.25)},
		{"second with excluded starts", "claude", `{"type":"result","usage":{"input_tokens":2,"output_tokens":1},"total_cost_usd":0}`, 5, 1, 2, 2, float64(0)},
		{"first with excluded starts", "claude", `{"type":"result","usage":{"input_tokens":2,"output_tokens":1}}`, 3, 1, 1, 1, nil},
		{"negative cost", "claude", `{"type":"result","total_cost_usd":-1}`, 1, 0, 0, 1, nil},
		{"invalid cost", "claude", `{"type":"result","total_cost_usd":"1"}`, 1, 0, 0, 1, nil},
		{"overflow cost", "claude", `{"type":"result","total_cost_usd":1e999}`, 1, 0, 0, 1, nil},
		{"codex", "codex", `{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":1},"total_cost_usd":4}`, 2, 0, 0, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			r.Runs, r.ReportCompletionRuns, r.UnansweredRuns = tc.runs, tc.reports, tc.unanswered
			dir := t.TempDir()
			exe := filepath.Join(dir, "provider")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\ncat <<'FIXTURE'\n"+tc.stream+"\nFIXTURE\n"), 0700); err != nil {
				t.Fatal(err)
			}
			settings := providers.Settings{Provider: tc.provider}
			answer, _ := providers.Call(context.Background(), providers.Request{Settings: settings, Policy: providers.Policy{Root: dir, Tree: dir}, Codex: exe, Claude: exe})
			if err := e.recordUsage(r, d, settings, "test", answer); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[len(saved.Usage)-1], &row); err != nil {
				t.Fatal(err)
			}
			if err := checkGenerationAndCost(row, tc.generation, tc.cost); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// checkGenerationAndCost reports both fields so a failing row is actionable.
func checkGenerationAndCost(row map[string]any, generation float64, cost any) error {
	if row["generation"] != generation || row["costUsd"] != cost {
		return fmt.Errorf("generation: got %v, want %v; costUsd: got %v, want %v", row["generation"], generation, row["costUsd"], cost)
	}
	return nil
}

func TestSavedCostValidation(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		value          *float64
		want           any
	}{
		{"missing", "claude", nil, nil},
		{"zero", "claude", costValue(0), float64(0)},
		{"positive", "claude", costValue(1.25), float64(1.25)},
		{"negative", "claude", costValue(-1), nil},
		{"nan", "claude", costValue(math.NaN()), nil},
		{"positive infinity", "claude", costValue(math.Inf(1)), nil},
		{"negative infinity", "claude", costValue(math.Inf(-1)), nil},
		{"other provider", "codex", costValue(1.25), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			if err := e.recordUsage(r, d, providers.Settings{Provider: tc.provider}, "test", providers.Answer{Usage: providers.Usage{CostUSD: tc.value}}); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[0], &row); err != nil {
				t.Fatal(err)
			}
			if err := checkSavedCost(tc.provider, row["costUsd"], tc.want); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func costValue(v float64) *float64 { return &v }

func checkSavedCost(provider string, got, want any) error {
	if got != want {
		return fmt.Errorf("provider %s: costUsd: got %v, want %v", provider, got, want)
	}
	return nil
}

func TestRecoveredCodexUsageSavedAndWeighted(t *testing.T) {
	e, r, d, _ := completionFixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	dir := filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-abc.jsonl")
	event := func(i, c, o int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d}}}}`, i, c, o)
	}
	if err := os.WriteFile(path, []byte(event(100, 20, 30)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nprintf '%s\\n' '" + event(120, 25, 33) + "' >> '" + path + "' || exit 1\nexit 1\n"
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	settings := providers.Settings{Provider: "codex"}
	answer, _ := providers.Call(context.Background(), providers.Request{Settings: settings, Policy: providers.Policy{Root: t.TempDir(), Tree: t.TempDir()}, Codex: exe, Session: "abc"})
	if err := e.recordUsage(r, d, settings, "実装", answer); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(saved.Usage[0], &row); err != nil {
		t.Fatal(err)
	}
	if row["inputTokens"] != float64(20) || row["cacheReadTokens"] != float64(5) || row["outputTokens"] != float64(3) || row["usageMissingReason"] != nil || saved.WeightedUsage != 30.5 {
		t.Fatal(row, saved.WeightedUsage)
	}
}

func TestBillingFromCallThroughJournalAndRetry(t *testing.T) {
	const key = "synthetic-billing-key"
	t.Setenv("ANTHROPIC_API_KEY", "inherited-billing-secret")
	for _, tc := range []struct {
		name, provider, kind, action string
		key, wantMode                bool
	}{
		{"claude api", "claude", "normal", "success", true, true},
		{"claude subscription inherited", "claude", "normal", "success", false, false},
		{"codex", "codex", "normal", "exit", true, false},
		{"antigravity", "antigravity", "normal", "exit", true, false},
		{"followup api", "claude", "followup", "success", true, true},
		{"followup subscription", "claude", "followup", "success", false, false},
		{"nonzero", "claude", "normal", "exit", true, true},
		{"limit", "claude", "normal", "limit", true, true},
		{"invalid result", "claude", "normal", "invalid", true, true},
		{"cancel after start", "claude", "normal", "cancel", true, true},
		{"deadline after start", "claude", "normal", "deadline", true, true},
		{"cancel before start", "claude", "normal", "precancel", true, false},
		{"deadline before start", "claude", "normal", "predeadline", true, false},
		{"rejected before start", "claude", "normal", "reject", true, false},
		{"start failure", "claude", "normal", "missing", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &limitServer{status: 503}
			e, r := limitExecutor(t, s)
			old := json.RawMessage(`{"provider":"claude","inputTokens":7}`)
			r.Usage = append(r.Usage, old)
			r.UsageSent = 1
			dir := t.TempDir()
			exe := filepath.Join(dir, "fake-cli")
			started := filepath.Join(dir, "started")
			event := `{"type":"result","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":4,"cache_creation_input_tokens":2},"total_cost_usd":0.25,"structured_output":` + string(implementationFixture()) + `}`
			if tc.action == "invalid" {
				event = `{"type":"result","usage":{"input_tokens":10,"output_tokens":2},"structured_output":{"summary":"invalid"}}`
			}
			body := "printf '%s' \"${ANTHROPIC_API_KEY-unset}\" > '" + filepath.Join(dir, "child-env") + "' || exit 1\nprintf '%s\\n' '" + event + "'\n"
			switch tc.action {
			case "exit":
				body += "exit 7\n"
			case "limit":
				body += "echo 'credit balance is too low' >&2\nexit 1\n"
			case "cancel", "deadline":
				body += "touch '" + started + "'\nexec sleep 30\n"
			}
			if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0700); err != nil {
				t.Fatal(err)
			}
			settings := providers.Settings{Provider: tc.provider}
			req := providers.Request{Settings: settings, Policy: providers.Policy{Root: dir, Tree: dir}, Claude: exe, Codex: exe, Antigravity: exe}
			if tc.provider == "antigravity" {
				req.AntigravitySettings = filepath.Join(dir, "agy", "settings.json")
				if err := providers.PrepareAntigravity(req.AntigravitySettings, req.Policy); err != nil {
					t.Fatal(err)
				}
			}
			if tc.key {
				req.APIKey = key
			}
			if tc.action == "reject" {
				req.Session = "invalid session"
			}
			if tc.action == "missing" {
				req.Claude = filepath.Join(dir, "missing")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.action == "precancel" {
				cancel()
			}
			if tc.action == "predeadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
			}
			if tc.action == "cancel" || tc.action == "deadline" {
				if tc.action == "deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
				}
				go func() {
					for {
						if _, err := os.Stat(started); err == nil {
							if tc.action == "cancel" {
								cancel()
							}
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
				}()
			}
			answer, callErr := providers.Call(ctx, req)
			if tc.action == "cancel" || tc.action == "deadline" {
				if _, err := os.Stat(started); err != nil {
					t.Fatal("child did not signal start", err)
				}
			}
			if tc.action == "success" && callErr != nil {
				t.Fatal(callErr)
			}
			if tc.action != "success" && callErr == nil {
				t.Fatal("expected call failure")
			}
			if answer.APIKeyMode != tc.wantMode {
				t.Fatalf("mode %v error %v", answer.APIKeyMode, callErr)
			}
			if tc.provider == "claude" && tc.action != "precancel" && tc.action != "predeadline" && tc.action != "reject" && tc.action != "missing" {
				env, err := os.ReadFile(filepath.Join(dir, "child-env"))
				wantEnv := "unset"
				if tc.key {
					wantEnv = key
				}
				if err != nil || string(env) != wantEnv {
					t.Fatal("child environment mismatch", err)
				}
			}
			d := Descriptor{Descriptor: jobspec.Descriptor{Repo: "o/r", Issue: 1, Kind: tc.kind}}
			if err := e.recordUsage(r, d, settings, "test", answer); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(saved.Usage[0]) != string(old) {
				t.Fatal("old row changed")
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[1], &row); err != nil {
				t.Fatal(err)
			}
			want := "subscription"
			if tc.wantMode {
				want = "api_credit"
			}
			if row["billing"] != want || row["kind"] != tc.kind || row["generation"] != float64(1) {
				t.Fatal(row)
			}
			if tc.provider == "claude" && tc.action != "invalid" && tc.action != "precancel" && tc.action != "predeadline" && tc.action != "reject" && tc.action != "missing" {
				if row["inputTokens"] != float64(10) || row["outputTokens"] != float64(2) || row["costUsd"] != float64(0.25) || saved.WeightedUsage != 22.9 {
					t.Fatal(row, saved.WeightedUsage)
				}
			}
			if err := e.flushUsage(context.Background(), saved); err == nil {
				t.Fatal("expected failed send")
			}
			reloaded, err := e.Store.Load(r.ID)
			if err != nil || reloaded.UsageSent != 1 {
				t.Fatal(reloaded, err)
			}
			s.status = 0
			if err := e.flushUsage(context.Background(), reloaded); err != nil {
				t.Fatal(err)
			}
			if len(s.bodies) != 2 || len(s.bodies[0]["usage"]) != 1 || len(s.bodies[1]["usage"]) != 1 {
				t.Fatal(s.bodies)
			}
			for _, body := range s.bodies {
				payload, _ := json.Marshal(body)
				sentRow, _ := json.Marshal(body["usage"][0])
				if string(sentRow) != string(saved.Usage[1]) || body["usage"][0]["billing"] != want || strings.Contains(string(payload), key) || strings.Contains(string(payload), "inherited-billing-secret") || strings.Contains(string(payload), dir) {
					t.Fatal("invalid payload", string(payload))
				}
			}
			again, err := e.Store.Load(r.ID)
			if err != nil || again.UsageSent != 2 || string(again.Usage[0]) != string(old) {
				t.Fatal(again, err)
			}
		})
	}
}

func TestBillingThroughExecutionEntrypoints(t *testing.T) {
	for _, entry := range []string{"implementation", "review", "completion"} {
		t.Run(entry, func(t *testing.T) {
			f, d, r, key := claudeKeyCallFixture(t)
			e := f.Executor
			completionAPI(t, f)
			setClaudeCallHold(t, e, true)
			writeClaudeCallCLI(t, e, "printf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"usage\":{\"input_tokens\":4,\"output_tokens\":1},\"result\":\"credit balance is too low\"}'\n")
			e.Call = providers.Call
			switch entry {
			case "implementation":
				_, _ = e.callAIOnce(context.Background(), d, r, "implement", false, nil)
			case "review":
				r.Provider = "codex"
				_, _ = e.callAIOnce(context.Background(), d, r, "review", true, nil)
			case "completion":
				if err := e.receiveImplementationReport(r, reportWith("assumptions", []any{}), "implement_report"); err != nil {
					t.Fatal(err)
				}
				_, _ = e.completeImplementationReport(context.Background(), d, r, true)
			}
			saved, err := e.Store.Load(r.ID)
			if err != nil || len(saved.Usage) != 1 {
				t.Fatal(saved, err)
			}
			var row map[string]any
			if err := json.Unmarshal(saved.Usage[0], &row); err != nil {
				t.Fatal(err)
			}
			if row["billing"] != "api_credit" || row["inputTokens"] != float64(4) || strings.Contains(string(saved.Usage[0]), key) {
				t.Fatal(row)
			}
		})
	}
}
