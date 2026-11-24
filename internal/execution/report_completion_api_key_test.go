package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	"issue-desk-worker/internal/providers"
)

func TestClaudeAPIKeyReviewAndReportCompletionLimitRouting(t *testing.T) {
	for _, entry := range []string{"review", "completion"} {
		for _, withKey := range []bool{false, true} {
			name := entry + "/subscription"
			if withKey {
				name = entry + "/api-key"
			}
			t.Run(name, func(t *testing.T) {
				f, d, r, key := claudeKeyCallFixture(t)
				e := f.Executor
				completionAPI(t, f)
				original := providerHold{}
				if withKey {
					original = setClaudeCallHold(t, e, true)
				}
				writeClaudeCallCLI(t, e, "printf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"usage\":{\"input_tokens\":4,\"output_tokens\":1},\"result\":\"credit balance is too low\"}'\n")
				calls := 0
				e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					calls++
					if (req.APIKey == key) != withKey || !req.Policy.ReadOnly || req.Settings.Provider != "claude" {
						t.Fatal("incorrect key or read-only routing")
					}
					return providers.Call(ctx, req)
				}
				if entry == "review" {
					r.Provider = "codex"
					_, err := e.callAIOnce(context.Background(), d, r, "review", true, nil)
					var limit *providers.UsageLimit
					if !errors.As(err, &limit) || e.holds.Usage["claude"] != original || r.Runs != 1 || len(r.ProviderHold) != 0 {
						t.Fatal("review modified subscription hold or run", err)
					}
				} else {
					if err := e.receiveImplementationReport(r, reportWith("assumptions", []any{}), "implement_report"); err != nil {
						t.Fatal(err)
					}
					state, err := e.completeImplementationReport(context.Background(), d, r, true)
					if err != nil || state == nil || r.Runs != 2 || r.ReportCompletionRuns != 1 || !r.ReportCompletion.Attempted || len(r.ProviderHold) == 0 {
						t.Fatal("completion limit/count/receipt changed", state, err)
					}
					if withKey && e.holds.Usage["claude"] != original {
						t.Fatal("completion extended subscription hold")
					}
					if !withKey && e.holds.Usage["claude"].Until == 0 {
						t.Fatal("completion subscription limit not saved")
					}
					_, _ = e.completeImplementationReport(context.Background(), d, r, true)
				}
				if calls != 1 || len(r.Usage) != 1 || (e.holds.APIKeyHold != nil) != withKey {
					t.Fatal("limit routing/retry/usage changed", calls)
				}
				if withKey && (e.holds.APIKeyHold.Until != original.Until || e.CanAcquire()) {
					t.Fatal("key limit did not stop acquisition")
				}
				stored, err := e.Store.Load(r.ID)
				if err != nil || len(stored.Usage) != 1 || strings.Contains(string(encode(stored)), key) {
					t.Fatal("usage persistence or credential isolation failed", err)
				}
			})
		}
	}
}

func TestExecutionReportClaudeAPIKeyRetryKeepsImplementationConversation(t *testing.T) {
	f, d, r, key := claudeKeyCallFixture(t)
	e := f.Executor
	completionAPI(t, f)
	d.reportOnly = true
	calls := 0
	e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		if req.Settings.Provider != "claude" || !req.Policy.ReadOnly || req.Session != "" || req.OnSession != nil {
			t.Fatal("報告に実装会話が渡されました", req.Session)
		}
		if calls == 1 {
			if req.APIKey != "" {
				t.Fatal("初回は契約認証を使う必要があります")
			}
			return providers.Answer{Session: "subscription-report-session"}, &providers.UsageLimit{Provider: "claude"}
		}
		if req.APIKey != key {
			t.Fatal("APIキーへの切替が行われませんでした")
		}
		return providers.Answer{Session: "api-report-session", JSON: implementationFixture()}, nil
	}
	_, err := e.callAI(t.Context(), d, r, "report", false, nil)
	saved, loadErr := e.Store.Load(r.ID)
	if err != nil || loadErr != nil || calls != 2 || saved.Session != "implementation-session" || r.Session != saved.Session {
		t.Fatal("認証切替で実装会話が変わりました", calls, err, loadErr)
	}
}
