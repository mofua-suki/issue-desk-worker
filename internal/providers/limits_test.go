package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Captured from Claude Code 2.1.316 (2026-11-03) with session_id and uuid removed.
const claudeLimitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794024000,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.29,"resetsAt":1794024000,"status":"allowed"}}}}`

// Captured from a Codex rollout on the NAS (2026-11-03).
const codexRateLimits = `"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":22.0,"window_minutes":10080,"resets_at":1794126998},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"individual_limit":null,"spend_control_reached":null,"plan_type":"pro","rate_limit_reached_type":null}`

func text(v *string) string {
	if v == nil {
		return "<nil>"
	}
	return *v
}

func TestClaudeLimitsReadsCapturedEvent(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	rows := ClaudeLimits(claudeLimitEvent, now)
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	row := rows[0]
	if row.Provider != "claude" || row.Limit != "seven_day" || text(row.Status) != "allowed" || row.Percent == nil || *row.Percent != 29 || row.Utilization == nil || *row.Utilization != 0.29 || text(row.ResetsAt) != "2026-11-07T04:00:00.000Z" || row.WindowMinutes != nil {
		t.Fatalf("%+v", row)
	}
}

func TestClaudeLimitsWindowsAndMissingValues(t *testing.T) {
	now := time.Now()
	rows := ClaudeLimits(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","utilization":0.5,"resets_at":"2026-11-07T04:00:00Z","rate_limit_type":"five_hour","unified_windows":{"five_hour":{"utilization":0.1},"seven_day":{"utilization":0.2,"resetsAt":1794024000},"broken":3}}}`, now)
	if len(rows) != 2 || rows[0].Limit != "five_hour" || *rows[0].Percent != 50 || text(rows[0].Status) != "allowed_warning" || text(rows[0].ResetsAt) != "2026-11-07T04:00:00.000Z" || rows[1].Limit != "seven_day" || rows[1].Status != nil || *rows[1].Percent != 20 {
		t.Fatalf("%+v", rows)
	}
	rows = ClaudeLimits(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"seven_day","resetsAt":"unknown"}}`, now)
	if len(rows) != 1 || rows[0].Percent != nil || rows[0].Utilization != nil || rows[0].ResetsAt != nil {
		t.Fatalf("読めない値を埋めています: %+v", rows)
	}
	for _, line := range []string{`{"type":"rate_limit_event"}`, `{"type":"result","note":"rate_limit_event"}`, `rate_limit_event`} {
		if rows := ClaudeLimits(line, now); len(rows) != 0 {
			t.Fatalf("%s: %+v", line, rows)
		}
	}
}

func writeRollout(t *testing.T, home, session string, day time.Time, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func tokenCount(at time.Time, limits string) string {
	return `{"timestamp":"` + at.UTC().Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"token_count","info":{},` + limits + `}}`
}

func TestCodexLimitsReadsLastSampleOfThisCall(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	start := now.Add(-time.Minute)
	writeRollout(t, home, "abc", now.AddDate(0, 0, -3),
		tokenCount(start.Add(-time.Hour), `"rate_limits":{"primary":{"used_percent":99,"window_minutes":10080}}`),
		tokenCount(start.Add(time.Second), `"rate_limits":{"primary":{"used_percent":10,"window_minutes":10080}}`),
		tokenCount(start.Add(2*time.Second), codexRateLimits),
		tokenCount(start.Add(3*time.Second), `"rate_limits":null`),
		`not json token_count`)
	rows := CodexLimits(home, "abc", start, now)
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	row := rows[0]
	if row.Provider != "codex" || row.Limit != "seven_day" || *row.Percent != 22 || *row.Utilization != 0.22 || *row.WindowMinutes != 10080 || row.Status != nil || text(row.ResetsAt) != time.Unix(1794126998, 0).UTC().Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("%+v", row)
	}
}

func TestCodexLimitsWindowsAndAbsentRecords(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	writeRollout(t, home, "abc", now, tokenCount(now, `"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_in_seconds":60},"secondary":{"used_percent":6,"window_minutes":90}}`))
	rows := CodexLimits(home, "abc", now.Add(-time.Minute), now)
	if len(rows) != 2 || rows[0].Limit != "five_hour" || text(rows[0].ResetsAt) != "2026-11-03T08:01:00.000Z" || rows[1].Limit != "window_90m" {
		t.Fatalf("%+v", rows)
	}
	for name, session := range map[string]string{"無い会話": "other", "不正な会話ID": "../abc", "空": ""} {
		if rows := CodexLimits(home, session, now.Add(-time.Minute), now); len(rows) != 0 {
			t.Fatalf("%s: %+v", name, rows)
		}
	}
	if rows := CodexLimits(home, "abc", now.Add(time.Minute), now); len(rows) != 0 {
		t.Fatalf("開始前の記録を読みました: %+v", rows)
	}
}

func TestCodexLimitsIgnoresPartialLineAtTailStart(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	padding := strings.Repeat("x", limitTailBytes)
	writeRollout(t, home, "abc", now, `{"pad":"`+padding+`"}`, tokenCount(now, codexRateLimits))
	if rows := CodexLimits(home, "abc", now.Add(-time.Minute), now); len(rows) != 1 || *rows[0].Percent != 22 {
		t.Fatalf("%+v", rows)
	}
}

func TestCodexLimitsBoundsOldSessionSearch(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeRollout(t, home, "abc", now.AddDate(0, 0, -45), tokenCount(now, codexRateLimits))
	if rows := CodexLimits(home, "abc", now.Add(-time.Minute), now); len(rows) != 1 {
		t.Fatalf("old session: %+v", rows)
	}
	dir := filepath.Join(home, "sessions", "unrelated")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= limitRolloutEntries; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if rows := CodexLimits(home, "abc", now.Add(-time.Minute), now); len(rows) != 0 {
		t.Fatalf("partial search must not select a record: %+v", rows)
	}
	if rows := CodexLimits(home, "missing", now.Add(-time.Minute), now); len(rows) != 0 {
		t.Fatalf("missing session: %+v", rows)
	}
	writeRollout(t, home, "recent", now, tokenCount(now, codexRateLimits))
	if rows := CodexLimits(home, "recent", now.Add(-time.Minute), now); len(rows) != 1 {
		t.Fatalf("recent session: %+v", rows)
	}
}

func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCallCollectsLimitsEvenWhenTheAnswerFails(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	writeRollout(t, home, "thread-1", now, tokenCount(now.Add(time.Hour), codexRateLimits))
	for name, request := range map[string]Request{
		"claude": {Settings: Settings{Provider: "claude", Model: "m", Effort: "low"}, Claude: fakeCLI(t, "cat <<'E'\n"+claudeLimitEvent+"\nE\n")},
		"codex":  {Settings: Settings{Provider: "codex", Model: "m", Effort: "low"}, Codex: fakeCLI(t, "echo '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}'\n")},
	} {
		request.Policy = Policy{Root: root, Tree: root}
		request.Prompt = "p"
		request.Timeout = 20 * time.Second
		answer, err := Call(t.Context(), request)
		if err == nil {
			t.Fatalf("%s: 回答が無いのに成功しました", name)
		}
		if len(answer.Limits) != 1 || answer.Limits[0].Provider != name || answer.Limits[0].Limit != "seven_day" {
			t.Fatalf("%s: %+v (%v)", name, answer.Limits, err)
		}
	}
}

func usageEvent(input, cached, output int) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d},"last_token_usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}}}`, input, cached, output)
}

func TestCodexUsageResumeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, baseline, appended string
		want                     bool
		input                    int64
	}{
		{"increment", usageEvent(100, 20, 30), usageEvent(120, 25, 33), true, 20},
		{"zero", usageEvent(100, 20, 30), usageEvent(100, 20, 30), true, 0},
		{"missing baseline", `{}`, usageEvent(120, 25, 33), false, 0},
		{"reverse", usageEvent(100, 20, 30), usageEvent(99, 20, 33), false, 0},
		{"invalid", usageEvent(100, 20, 30), usageEvent(-1, 20, 33), false, 0},
		{"broken", usageEvent(100, 20, 30), `{broken`, false, 0},
		{"no sample", usageEvent(100, 20, 30), `{}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			now := time.Now()
			writeRollout(t, home, "abc", now, tc.baseline)
			b := snapshotCodexUsage(home, "abc", now)
			f, err := os.OpenFile(b.path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(tc.appended + "\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			u := recoverCodexUsage(home, "abc", now, true, b)
			if (u.Input != nil) != tc.want {
				t.Fatalf("%+v", u)
			}
			if tc.want && *u.Input != tc.input {
				t.Fatal(*u.Input)
			}
		})
	}
}

func TestCodexUsageProcessInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cancel, stdout bool
	}{
		{"timeout recovery", false, false},
		{"cancel recovery", true, false},
		{"timeout stdout delta", false, true},
		{"cancel stdout delta", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			now := time.Now()
			writeRollout(t, home, "abc", now, usageEvent(100, 20, 30))
			path := codexRollout(home, "abc", now)
			script := "printf '%s\\n' '" + usageEvent(120, 25, 33) + "' >> '" + path + "' || exit 1\n"
			want := codexUsagePoint{20, 5, 3}
			if tc.stdout {
				script += completedUsage(150, 30, 40)
				want = codexUsagePoint{50, 10, 10}
			}
			cli := fakeCLI(t, script+"sleep 10\n")
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			outcome := "timeout"
			if tc.cancel {
				outcome = "aborted"
				go func() { time.Sleep(150 * time.Millisecond); stop() }()
			}
			answer, err := Call(ctx, Request{Settings: Settings{Provider: "codex"}, Policy: Policy{Root: t.TempDir(), Tree: t.TempDir()}, Codex: cli, Session: "abc", Timeout: 300 * time.Millisecond})
			if err == nil || answer.Outcome != outcome {
				t.Fatalf("%v %+v", err, answer)
			}
			checkCodexTokens(t, answer.Usage, &want)
		})
	}
}

func TestCodexUsageTruncationAndNewSession(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeRollout(t, home, "abc", now, usageEvent(1, 0, 1))
	b := snapshotCodexUsage(home, "abc", now)
	if err := os.WriteFile(b.path, []byte(usageEvent(10, 0, 1)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if u := recoverCodexUsage(home, "abc", now, true, b); u.Input != nil {
		t.Fatal(u)
	}
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"new"}}`, now.Add(time.Second).UTC().Format(time.RFC3339Nano))
	writeRollout(t, home, "new", now, meta, usageEvent(0, 0, 0))
	u := recoverCodexUsage(home, "new", now, false, codexUsageBaseline{})
	if u.Input == nil || *u.Input != 0 || u.Output == nil {
		t.Fatal(u)
	}
}

func TestCodexUsageStdoutTakesPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, stream, appended string
		input, cached, output  int64
		missing                bool
	}{
		{"stdout delta", `{"input_tokens":150,"output_tokens":40,"cached_input_tokens":30}`, usageEvent(120, 25, 33), 50, 10, 10, false},
		{"partial stdout recovers", `{"input_tokens":150}`, usageEvent(120, 25, 33), 20, 5, 3, false},
		{"invalid stdout unavailable", `{"input_tokens":7}`, `{}`, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			now := time.Now()
			writeRollout(t, home, "abc", now, usageEvent(100, 20, 30))
			path := codexRollout(home, "abc", now)
			cli := fakeCLI(t, "printf '%s\\n' '"+tc.appended+"' >> '"+path+"' || exit 1\necho '{\"type\":\"turn.completed\",\"usage\":"+tc.stream+"}'\nexit 1\n")
			answer, err := Call(context.Background(), Request{Settings: Settings{Provider: "codex"}, Policy: Policy{Root: t.TempDir(), Tree: t.TempDir()}, Codex: cli, Session: "abc"})
			if err == nil || answer.Outcome != "failed" {
				t.Fatalf("process failure lost: %v %+v", err, answer)
			}
			if tc.missing {
				checkCodexTokens(t, answer.Usage, nil)
			} else {
				checkCodexTokens(t, answer.Usage, &codexUsagePoint{tc.input, tc.cached, tc.output})
			}
		})
	}
}
