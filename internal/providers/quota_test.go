package providers

import (
	"issue-desk-worker/internal/process"
	"testing"
	"time"
)

func TestLegacyQuotaResetAndErrorOnlyBoundary(t *testing.T) {
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{"try again in 1h 30m": "2026-09-28T01:30:00Z", "resets in 45 minutes": "2026-09-28T00:45:00Z", "5-hour limit reached ∙ resets 3pm (Asia/Tokyo)": "2026-09-28T06:00:00Z", "resets Sep 28, 2026 3:00 PM (Asia/Tokyo)": "2026-09-28T06:00:00Z", "resets Sep 28 3:00 PM (Asia/Tokyo)": "2026-09-28T06:00:00Z", "resets 3pm (UTC).": "2026-09-28T15:00:00Z", "resets (at) 3pm (UTC)": "2026-09-28T15:00:00Z", "resets 3pm (Mars/Olympus)": "", "usage limit": "", "Connection reset by peer": ""}
	for text, want := range cases {
		at := readResetAt(text, now)
		if want == "" {
			if !at.IsZero() {
				t.Fatalf("%s: %s", text, at)
			}
			continue
		}
		expected, _ := time.Parse(time.RFC3339, want)
		if !at.Equal(expected) {
			t.Fatalf("%s: got=%s want=%s", text, at, expected)
		}
	}
	for _, provider := range []string{"codex", "claude", "antigravity"} {
		for _, tc := range []struct {
			r                process.Result
			quota, transient bool
		}{{process.Result{Code: 1, Err: "usage limit; resets in 45 minutes"}, true, false}, {process.Result{Code: 1, Err: "rate limit 429"}, false, true}, {process.Result{Code: 1, Err: "403 unauthorized rate limit"}, false, false}, {process.Result{Code: 0, Out: `{"type":"message","message":"usage limit quota"}`}, false, false}} {
			q := usageFailure(provider, tc.r, now)
			if (q != nil) != tc.quota || transientFailure(provider, tc.r) != tc.transient {
				t.Fatalf("%s %+v: quota=%+v", provider, tc, q)
			}
		}
	}
	for _, text := range []string{"usage limit", "usage limit resets in 9 days", "usage limit reached|1"} {
		q := usageFailure("codex", process.Result{Code: 1, Err: text}, now)
		if q == nil || q.Source != "default" || !q.Until.Equal(now.Add(30*time.Minute)) {
			t.Fatalf("%s: %+v", text, q)
		}
	}
	for provider, out := range map[string]string{"codex": `{"type":"turn.failed","error":{"message":"usage limit resets in 2 hours"}}`, "claude": `{"type":"result","is_error":true,"result":"usage limit reached|1790647200"}`, "antigravity": `{"isError":true,"response":"out of credits"}`} {
		code := 0
		if provider == "codex" {
			code = 1
		}
		if q := usageFailure(provider, process.Result{Code: code, Out: out}, now); q == nil {
			t.Fatalf("structured error %s", provider)
		}
	}
}
