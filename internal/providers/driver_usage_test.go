package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageInvalidAndZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"input_tokens":null,"output_tokens":-1,"cached_input_tokens":"0"}`, `{"input_tokens":1.5,"output_tokens":{},"cached_input_tokens":null}`} {
		u := readUsage(json.RawMessage(raw), "codex")
		if u.Input != nil || u.Output != nil || u.Cached != nil {
			t.Fatalf("%s: %+v", raw, u)
		}
	}
	u := readUsage(json.RawMessage(`{"input_tokens":0,"output_tokens":0,"cached_input_tokens":0}`), "codex")
	if u.Input == nil || u.Output == nil || u.Cached == nil || *u.Input != 0 || *u.Output != 0 || *u.Cached != 0 {
		t.Fatal(u)
	}
	for _, raw := range []string{"null", "-1", `"1"`, "1.5", ""} {
		if nonnegativeInt(json.RawMessage(raw)) != nil {
			t.Fatal(raw)
		}
	}
}

func contextEvent(value string) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":` + value + `}}}}`
}

func contextCall(t *testing.T, resume bool, action string, ctx context.Context, timeout time.Duration) (Answer, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	baseline := strings.Replace(usageEvent(900, 0, 1), `"last_token_usage":{"input_tokens":1,`, `"last_token_usage":{"input_tokens":900,`, 1)
	writeRollout(t, home, "abc", now, baseline)
	path := codexRollout(home, "abc", now)
	if !resume {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		action = "printf '%s\\n' '" + fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"abc"}}`, now.Add(time.Minute).UTC().Format(time.RFC3339Nano)) + "' > '" + path + "' || exit 1\n" + action
	}
	action = strings.ReplaceAll(action, "$ROLLOUT", path)
	script := "while [ \"$#\" -gt 0 ]; do case \"$1\" in -o) shift; printf '%s' '" + emptyAssumptionsReport + "' > \"$1\" || exit 1;; esac; shift; done\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"abc\"}'\n" + action + "\necho '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"cached_input_tokens\":1}}'\n"
	r := Request{Settings: Settings{Provider: "codex"}, Policy: Policy{Root: t.TempDir(), Tree: t.TempDir()}, Codex: fakeCLI(t, script), Timeout: timeout}
	if resume {
		r.Session = "abc"
	}
	return Call(ctx, r)
}

func appendContext(values ...string) string {
	var s string
	for _, value := range values {
		s += "printf '%s\\n' '" + contextEvent(value) + "' >> '$ROLLOUT'\n"
	}
	return s
}

func checkContext(t *testing.T, a Answer, max, last, calls int64) {
	t.Helper()
	u := a.Usage
	if u.ContextMaxTokens == nil || *u.ContextMaxTokens != max || u.ContextTotalTokens == nil || *u.ContextTotalTokens != last || u.Calls == nil || *u.Calls != calls {
		t.Fatalf("context: %+v", u)
	}
}

func TestCallCodexContextNewSession(t *testing.T) {
	a, err := contextCall(t, false, appendContext("100", "300", "200"), context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkContext(t, a, 300, 200, 3)
	if *a.Usage.Input != 7 || *a.Usage.Output != 2 || *a.Usage.Cached != 1 {
		t.Fatal(a.Usage)
	}
}

func TestCallCodexContextResume(t *testing.T) {
	a, err := contextCall(t, true, appendContext("400", "500"), context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkContext(t, a, 500, 500, 2)
}

func TestCallCodexContextUnavailable(t *testing.T) {
	for name, action := range map[string]string{
		"empty info":   `printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","info":{}}}' >> '$ROLLOUT'`,
		"missing last": `printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{}}}}' >> '$ROLLOUT'`,
		"no record":    "rm '$ROLLOUT'",
		"replace":      "rm '$ROLLOUT'\n" + appendContext("10"),
		"modify":       "printf '%s\\n' '{}' > '$ROLLOUT'\n" + appendContext("10"),
		"truncate":     ": > '$ROLLOUT'",
		"limit":        "head -c 4194305 /dev/zero >> '$ROLLOUT'",
		"broken":       appendContext("10") + "printf '%s\\n' '{broken' >> '$ROLLOUT'",
		"partial":      appendContext("10") + "printf '%s' '{' >> '$ROLLOUT'",
		"negative":     appendContext("10", "-1"),
		"string":       appendContext("10", `"1"`),
		"overflow":     appendContext("10", "9223372036854775808"),
		"null":         appendContext("10", "null"),
	} {
		t.Run(name, func(t *testing.T) {
			a, err := contextCall(t, true, action, context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			u := a.Usage
			if u.ContextMaxTokens != nil || u.ContextTotalTokens != nil || u.Calls == nil || *u.Calls != 1 || u.Input != nil || u.Output != nil || u.Cached != nil {
				t.Fatal(u)
			}
		})
	}
}

func TestCallCodexContextObservedSamples(t *testing.T) {
	action := appendContext("300") + `printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","info":{}}}' >> '$ROLLOUT'` + "\n" + appendContext("0", "200")
	a, err := contextCall(t, true, action, context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkContext(t, a, 300, 200, 3)
	a, err = contextCall(t, true, appendContext("0"), context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkContext(t, a, 0, 0, 1)
}

func TestCallCodexContextProcessExit(t *testing.T) {
	for _, mode := range []string{"exit", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			action := appendContext("42")
			timeout := time.Duration(0)
			if mode == "exit" {
				action += "exit 3\n"
			} else if mode == "timeout" {
				// 負荷の高いときでも、時間切れの前に文脈の記録まで進むだけの余裕を取る。
				action += "sleep 10\n"
				timeout = 3 * time.Second
			} else {
				// 文脈の記録を書き終えたのを確かめてから止める（固定の待ち時間だと、負荷の高いときに書く前に止まる）。
				written := filepath.Join(t.TempDir(), "written")
				action += "touch '" + written + "'\nsleep 10\n"
				go func() {
					for ctx.Err() == nil {
						if _, err := os.Stat(written); err == nil {
							cancel()
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}
			a, err := contextCall(t, true, action, ctx, timeout)
			if err == nil {
				t.Fatal("expected process error")
			}
			checkContext(t, a, 42, 42, 1)
			want := map[string]string{"exit": "failed", "cancel": "aborted", "timeout": "timeout"}[mode]
			if a.Outcome != want {
				t.Fatalf("outcome %s, want %s", a.Outcome, want)
			}
		})
	}
}

func checkCodexTokens(t *testing.T, u Usage, want *codexUsagePoint) {
	t.Helper()
	if want == nil {
		if u.Input != nil || u.Cached != nil || u.Output != nil {
			t.Fatalf("expected unavailable tokens: %+v", u)
		}
		return
	}
	if u.Input == nil || u.Cached == nil || u.Output == nil || *u.Input != want.input || *u.Cached != want.cached || *u.Output != want.output {
		t.Fatalf("tokens: %+v, want %+v", u, want)
	}
}

func codexUsageFixture(t *testing.T, baseline string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	writeRollout(t, home, "abc", now, baseline)
	return home, codexRollout(home, "abc", now)
}

func callCodexUsage(t *testing.T, session, action string) Answer {
	t.Helper()
	script := "while [ \"$#\" -gt 0 ]; do case \"$1\" in -o) shift; printf '%s' '" + emptyAssumptionsReport + "' > \"$1\" || exit 1;; esac; shift; done\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"abc\"}'\n" + action
	a, err := Call(t.Context(), Request{Settings: Settings{Provider: "codex"}, Policy: Policy{Root: t.TempDir(), Tree: t.TempDir()}, Session: session, Codex: fakeCLI(t, script)})
	if err != nil || a.Outcome != "completed" {
		t.Fatalf("usage measurement changed execution result: %v %+v", err, a)
	}
	return a
}

func completedUsage(input, cached, output int) string {
	return fmt.Sprintf("echo '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"cached_input_tokens\":%d,\"output_tokens\":%d}}'\n", input, cached, output)
}

func TestCallCodexUsageResumeDelta(t *testing.T) {
	_, path := codexUsageFixture(t, usageEvent(900, 700, 50))
	a := callCodexUsage(t, "abc", "printf '%s\\n' '"+usageEvent(1500, 1200, 80)+"' >> '"+path+"' || exit 1\n"+completedUsage(1500, 1200, 80))
	checkCodexTokens(t, a.Usage, &codexUsagePoint{600, 500, 30})
	checkContext(t, a, 1, 1, 1)
}

func TestCallCodexUsageConsecutiveResumes(t *testing.T) {
	_, path := codexUsageFixture(t, usageEvent(900, 700, 50))
	var total codexUsagePoint
	for _, end := range []codexUsagePoint{{1500, 1200, 80}, {1900, 1400, 100}} {
		action := "printf '%s\\n' '" + usageEvent(int(end.input), int(end.cached), int(end.output)) + "' >> '" + path + "' || exit 1\n" + completedUsage(int(end.input), int(end.cached), int(end.output))
		a := callCodexUsage(t, "abc", action)
		if a.Usage.Input == nil || a.Usage.Cached == nil || a.Usage.Output == nil {
			t.Fatal(a.Usage)
		}
		total.input += *a.Usage.Input
		total.cached += *a.Usage.Cached
		total.output += *a.Usage.Output
	}
	if total != (codexUsagePoint{1000, 700, 50}) {
		t.Fatalf("sum %+v, want final minus initial", total)
	}
}

func TestCallCodexUsageMultipleCompletions(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		want         codexUsagePoint
	}{
		{"last cumulative", completedUsage(1100, 800, 60) + completedUsage(1500, 1200, 80), codexUsagePoint{600, 500, 30}},
		{"measured zero", completedUsage(900, 700, 50) + completedUsage(900, 700, 50), codexUsagePoint{0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexUsageFixture(t, usageEvent(900, 700, 50))
			a := callCodexUsage(t, "abc", tc.events)
			checkCodexTokens(t, a.Usage, &tc.want)
			if a.Usage.Calls == nil || *a.Usage.Calls != 2 {
				t.Fatal(a.Usage)
			}
		})
	}
}

func TestCallCodexUsageResumeUnavailable(t *testing.T) {
	// These executions emit otherwise valid stdout totals. A damaged or absent
	// baseline/range must never make those totals look like per-execution usage.
	for _, tc := range []struct{ name, baseline, action string }{
		{"no record", "", ""},
		{"no baseline sample", `{}`, ""},
		{"baseline partial", usageEvent(900, 700, 50), ""},
		{"baseline missing field", `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":900,"output_tokens":50}}}}`, ""},
		{"baseline invalid type", strings.Replace(usageEvent(900, 700, 50), `"input_tokens":900`, `"input_tokens":"900"`, 1), ""},
		{"baseline invalid masked by alias", strings.Replace(usageEvent(900, 700, 50), `"input_tokens":900`, `"input_tokens":"900","inputTokens":900`, 1), ""},
		{"baseline negative", usageEvent(-1, 0, 50), ""},
		{"baseline overflow", strings.Replace(usageEvent(900, 700, 50), `"input_tokens":900`, `"input_tokens":9223372036854775808`, 1), ""},
		{"replace", usageEvent(900, 700, 50), "printf '%s\\n' '" + usageEvent(1500, 1200, 80) + "' > '$ROLLOUT.new'\nmv '$ROLLOUT.new' '$ROLLOUT'"},
		{"modify", usageEvent(900, 700, 50), "printf '%s\\n' '" + usageEvent(901, 700, 50) + "' > '$ROLLOUT'"},
		{"truncate", usageEvent(900, 700, 50), ": > '$ROLLOUT'"},
		{"unreadable", usageEvent(900, 700, 50), "rm '$ROLLOUT'\nln -s '$ROLLOUT.missing' '$ROLLOUT'"},
		{"read limit", usageEvent(900, 700, 50), "head -c 4194305 /dev/zero >> '$ROLLOUT'"},
		{"broken line", usageEvent(900, 700, 50), "printf '%s\\n' '{broken' >> '$ROLLOUT'"},
		{"partial line", usageEvent(900, 700, 50), "printf '%s' '{' >> '$ROLLOUT'"},
		{"invalid sample", usageEvent(900, 700, 50), "printf '%s\\n' '" + usageEvent(-1, 0, 50) + "' >> '$ROLLOUT'"},
		{"invalid info", usageEvent(900, 700, 50), `printf '%s\n' '{"type":"event_msg","payload":{"type":"token_count","info":[]}}' >> '$ROLLOUT'`},
		{"reverse sample", usageEvent(900, 700, 50), "printf '%s\\n' '" + usageEvent(899, 700, 50) + "' >> '$ROLLOUT'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, path := codexUsageFixture(t, tc.baseline)
			if tc.name == "baseline partial" {
				if err := os.WriteFile(path, []byte(tc.baseline), 0600); err != nil {
					t.Fatal(err)
				}
			}
			action := strings.ReplaceAll(tc.action, "$ROLLOUT", path)
			// The no-record case removes the file before the baseline is taken.
			if tc.name == "no record" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				action = ""
			}
			a := callCodexUsage(t, "abc", action+"\n"+completedUsage(1500, 1200, 80))
			checkCodexTokens(t, a.Usage, nil)
			if a.Usage.Calls == nil || *a.Usage.Calls != 1 {
				t.Fatal(a.Usage)
			}
		})
	}
	for _, tc := range []struct{ name, usage string }{
		{"absent", ""},
		{"null", "null"},
		{"missing input", `{"cached_input_tokens":1200,"output_tokens":80}`},
		{"missing input masked by alias", `{"inputTokens":1500,"cached_input_tokens":1200,"output_tokens":80}`},
		{"invalid input masked by alias", `{"input_tokens":"1500","inputTokens":1500,"cached_input_tokens":1200,"output_tokens":80}`},
		{"missing cached", `{"input_tokens":1500,"output_tokens":80}`},
		{"missing output", `{"input_tokens":1500,"cached_input_tokens":1200}`},
		{"type", `{"input_tokens":"1500","cached_input_tokens":1200,"output_tokens":80}`},
		{"negative value", `{"input_tokens":-1,"cached_input_tokens":1200,"output_tokens":80}`},
		{"fraction", `{"input_tokens":1500.5,"cached_input_tokens":1200,"output_tokens":80}`},
		{"overflow", `{"input_tokens":9223372036854775808,"cached_input_tokens":1200,"output_tokens":80}`},
		{"negative input delta", `{"input_tokens":899,"cached_input_tokens":700,"output_tokens":80}`},
		{"negative cached delta", `{"input_tokens":1500,"cached_input_tokens":699,"output_tokens":80}`},
		{"negative output delta", `{"input_tokens":1500,"cached_input_tokens":1200,"output_tokens":49}`},
		{"cached delta exceeds input", `{"input_tokens":910,"cached_input_tokens":720,"output_tokens":80}`},
		{"cached total exceeds input", `{"input_tokens":1500,"cached_input_tokens":1501,"output_tokens":80}`},
	} {
		for _, recoverable := range []bool{false, true} {
			t.Run(fmt.Sprintf("stdout %s/recover=%t", tc.name, recoverable), func(t *testing.T) {
				_, path := codexUsageFixture(t, usageEvent(900, 700, 50))
				action := completedUsage(1100, 800, 60)
				if recoverable {
					action += "printf '%s\\n' '" + usageEvent(920, 705, 53) + "' >> '" + path + "' || exit 1\n"
				}
				event := `{"type":"turn.completed"`
				if tc.usage != "" {
					event += `,"usage":` + tc.usage
				}
				a := callCodexUsage(t, "abc", action+"echo '"+event+"}'\n")
				if recoverable {
					checkCodexTokens(t, a.Usage, &codexUsagePoint{20, 5, 3})
					checkContext(t, a, 1, 1, 1)
				} else {
					checkCodexTokens(t, a.Usage, nil)
					if a.Usage.Calls == nil || *a.Usage.Calls != 2 {
						t.Fatal(a.Usage)
					}
				}
			})
		}
	}
}

func TestCallCodexUsageNewSessionUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        *codexUsagePoint
	}{
		{"stdout", completedUsage(1500, 1200, 80), &codexUsagePoint{1500, 1200, 80}},
		{"aliases", `echo '{"type":"turn.completed","usage":{"inputTokens":1500,"cacheReadTokens":1200,"outputTokens":80}}'`, &codexUsagePoint{1500, 1200, 80}},
		{"zero", completedUsage(0, 0, 0), &codexUsagePoint{0, 0, 0}},
		{"partial", `echo '{"type":"turn.completed","usage":{"input_tokens":7}}'`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexUsageFixture(t, usageEvent(900, 700, 50))
			a := callCodexUsage(t, "", tc.event)
			if tc.name == "partial" {
				if a.Usage.Input == nil || *a.Usage.Input != 7 || a.Usage.Output != nil || a.Usage.Cached != nil {
					t.Fatal(a.Usage)
				}
			} else {
				checkCodexTokens(t, a.Usage, tc.want)
			}
		})
	}
}

func TestCallCodexUsageLongOldRollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	old := time.Now().AddDate(0, 0, -45)
	filler := strings.Repeat("{}\n", (limitTailBytes/3)+1)
	writeRollout(t, home, "abc", old, usageEvent(900, 700, 50), strings.TrimSuffix(filler, "\n"))
	path := codexRollout(home, "abc", time.Now())
	action := "yes '{}' | head -n 1400000 >> '" + path + "' || exit 1\n" + "printf '%s\\n' '" + usageEvent(1500, 1200, 80) + "' >> '" + path + "' || exit 1\n" + completedUsage(1500, 1200, 80)
	a := callCodexUsage(t, "abc", action)
	checkCodexTokens(t, a.Usage, &codexUsagePoint{600, 500, 30})
}

func TestCallCodexUsageNewSessionRecoverySizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		padding int
		want    *codexUsagePoint
	}{
		{"within limit", 100, &codexUsagePoint{12, 2, 3}},
		{"over limit", limitTailBytes, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			now := time.Now()
			meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"abc"}}`, now.Add(time.Minute).UTC().Format(time.RFC3339Nano))
			writeRollout(t, home, "abc", now, meta, strings.TrimSuffix(strings.Repeat("{}\n", tc.padding/3), "\n"), usageEvent(12, 2, 3))
			a := callCodexUsage(t, "", "")
			checkCodexTokens(t, a.Usage, tc.want)
		})
	}
}

func TestCallCodexUsageOversizedBaselineUnavailable(t *testing.T) {
	_, path := codexUsageFixture(t, usageEvent(900, 700, 50))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat("{}\n", 1<<16)
	for n := 0; n <= maxCodexBaselineBytes/len(block); n++ {
		if _, err := f.WriteString(block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString(usageEvent(900, 700, 50) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	action := "printf '%s\\n' '" + usageEvent(1500, 1200, 80) + "' >> '" + path + "' || exit 1\n" + completedUsage(1500, 1200, 80)
	a := callCodexUsage(t, "abc", action)
	checkCodexTokens(t, a.Usage, nil)
	checkContext(t, a, 1, 1, 1)
}

func TestCallCodexUsageMissingInfo(t *testing.T) {
	missing := `{"type":"event_msg","payload":{"type":"token_count"}}`
	_, path := codexUsageFixture(t, usageEvent(900, 700, 50)+"\n"+missing)
	action := "printf '%s\\n' '" + missing + "' '" + usageEvent(1500, 1200, 80) + "' >> '" + path + "' || exit 1\n" + completedUsage(1500, 1200, 80)
	a := callCodexUsage(t, "abc", action)
	checkCodexTokens(t, a.Usage, &codexUsagePoint{600, 500, 30})
}
