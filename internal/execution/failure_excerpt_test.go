package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailureExcerptGoTimeout(t *testing.T) {
	stack := strings.Repeat("goroutine 42 [running]:\nexample.com/project.longStackFrame()\n", 200)
	output := "panic: test timed out after 10m0s\nrunning tests:\n\tTestDatabase/idk_secret123 (9m58s)\n\tTestOther (1m0s)\n\n" + stack
	for _, limit := range []int{4000, 2000} {
		got := failureExcerpt(output, limit)
		for _, want := range []string{"panic: test timed out after 10m0s", "running tests:", "\tTestDatabase/[REDACTED] (9m58s)", "\tTestOther (1m0s)"} {
			if !strings.Contains(got, want) {
				t.Errorf("limit %d: missing %q", limit, want)
			}
		}
		if strings.Contains(got, "idk_secret123") || len([]rune(got)) > limit {
			t.Fatalf("limit %d: invalid excerpt %q", limit, got)
		}
	}
	// runMeasuredTest first stores a 4000-character excerpt; callers shorten it again.
	message := fixedTestMessage("固定テスト: ", failureExcerpt(output, 4000), 2000, false)
	if !strings.Contains(message, "TestDatabase/[REDACTED] (9m58s)") || len([]rune(message)) > 2000 {
		t.Fatal(message)
	}
}

func TestFailureExcerptGoTimeoutLongList(t *testing.T) {
	var list strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&list, "\tTestDatabase%03d (9m58s)\n", i)
	}
	output := "panic: test timed out after 10m0s\nrunning tests:\n" + list.String() + "\n" + strings.Repeat("goroutine stack\n", 400)
	got := failureExcerpt(output, 4000)
	for _, want := range []string{"panic: test timed out", "running tests:", "TestDatabase000 (9m58s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len([]rune(got)) > 4000 || strings.Contains(got, "TestDatabase299") {
		t.Fatalf("invalid long-list excerpt: %q", got)
	}
}

func TestFailureExcerptTimeoutListBudgetAndDiagnostics(t *testing.T) {
	const header = "panic: test timed out after 10m0s\nrunning tests:\n"
	const diagnostic = "EACCES: permission denied, lstat /outside/cache"
	var list strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&list, "\tTestDatabase%03d (9m58s)\n", i)
	}
	stack := "\ngoroutine 42 [running]:\n" + strings.Repeat("stack frame\n", 400)
	for _, limit := range []int{2000, 4000} {
		output := header + list.String() + stack
		got := failureExcerpt(output, limit)
		lineSize := len([]rune("\tTestDatabase000 (9m58s)\n"))
		wantCount := (limit - len([]rune(header)) - len([]rune(excerptOmission)) + 1) / lineSize
		count := strings.Count(got, " (9m58s)")
		if count != wantCount || len([]rune(got)) > limit {
			t.Errorf("limit %d: complete names = %d, want %d; length = %d", limit, count, wantCount, len([]rune(got)))
		}
		got = failureExcerpt(output+diagnostic+"\n", limit)
		for _, want := range []string{header, "\tTestDatabase000 (9m58s)", diagnostic} {
			if !strings.Contains(got, want) {
				t.Errorf("limit %d: missing %q", limit, want)
			}
		}
		if len([]rune(got)) > limit {
			t.Errorf("limit %d: length = %d", limit, len([]rune(got)))
		}
	}
}

func TestFailureExcerptRetainsAssertionContext(t *testing.T) {
	output := "not ok 2 - broken test\n  ---\n  error: AssertionError\n  operator: strictEqual\n  expected: 7\n  actual: 9\n  stack: fixture\n  ...\n" + strings.Repeat("unrelated successful output\n", 1000)
	got := failureExcerpt(output, 1000)
	for _, want := range []string{"broken test", "expected: 7", "actual: 9"} {
		if !strings.Contains(got, want) {
			t.Fatalf("アサーションの根拠がありません: %q", got)
		}
	}
	if len([]rune(got)) > 1000 {
		t.Fatal("上限超え")
	}
}

func TestFailureExcerptTimeoutManyDiagnostics(t *testing.T) {
	var output strings.Builder
	output.WriteString("panic: test timed out after 10m0s\nrunning tests:\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&output, "\tTestDatabase%03d/idk_secret123 (9m58s)\n", i)
	}
	output.WriteString("\ngoroutine stack\n")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&output, "EACCES: permission denied diagnostic %02d %s\n", i, strings.Repeat("長", 250))
	}
	check := func(t *testing.T, got string, limit int) {
		t.Helper()
		for _, want := range []string{"panic: test timed out", "running tests:", "\tTestDatabase000/[REDACTED] (9m58s)", "EACCES: permission denied diagnostic 00"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q", want)
			}
		}
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "\tTestDatabase") && !strings.HasSuffix(line, " (9m58s)") {
				t.Errorf("incomplete test name: %q", line)
			}
		}
		if strings.Contains(got, "idk_secret123") || len([]rune(got)) > limit {
			t.Errorf("invalid excerpt: %q", got)
		}
	}
	for _, limit := range []int{2000, 4000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) { check(t, failureExcerpt(output.String(), limit), limit) })
	}
	t.Run("shortened twice", func(t *testing.T) {
		check(t, fixedTestMessage("固定テスト: ", failureExcerpt(output.String(), 4000), 2000, false), 2000)
	})
}

func TestFailureExcerptTimeoutPartialName(t *testing.T) {
	output := "panic: test timed out after 10m0s\nrunning tests:\n\tTestFirst (1m0s)\n\tTest" + strings.Repeat("Long", 100) + " (1m0s)\n\n" + strings.Repeat("stack\n", 100)
	got := failureExcerpt(output, 120)
	if !strings.Contains(got, "\tTestFirst (1m0s)") || strings.Contains(got, "\tTestLong") || len([]rune(got)) > 120 {
		t.Fatalf("invalid excerpt: %q", got)
	}
}

func TestFailureExcerpt(t *testing.T) {
	passes := strings.Repeat("ok  example/package 0.01s\n", 150)
	t.Run("failure markers and redaction", func(t *testing.T) {
		out := failureExcerpt(passes+"--- FAIL: TestExample (0.01s)\npanic: broken idk_secret123\nFAIL example/package\n", 2000)
		for _, want := range []string{"--- FAIL: TestExample", "panic: broken [REDACTED]", "FAIL example/package", "省略"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q: %s", want, out)
			}
		}
		if strings.Contains(out, "idk_secret123") || len([]rune(out)) > 2000 {
			t.Fatal(out)
		}
	})
	t.Run("environment diagnostic", func(t *testing.T) {
		out := failureExcerpt(passes+"EACCES: permission denied, lstat '/outside/cache'\n"+strings.Repeat("PASS\n", 500), 2000)
		if !strings.Contains(out, "EACCES: permission denied") || len([]rune(out)) > 2000 {
			t.Fatal(out)
		}
	})
	t.Run("short", func(t *testing.T) {
		if got := failureExcerpt("short\nPASS", 2000); got != "short\nPASS" {
			t.Fatal(got)
		}
	})
	t.Run("no markers keeps tail", func(t *testing.T) {
		out := failureExcerpt(passes+"last line", 100)
		if !strings.HasSuffix(out, "last line") || !strings.Contains(out, "省略") {
			t.Fatal(out)
		}
	})
}

func TestFailureExcerptNodeOutput(t *testing.T) {
	passes := strings.Repeat("✔ successful test (1ms)\n", 150)
	output := passes + "  ✖ broken test (2ms)\n  AssertionError: 0 !== 127\n" + passes + "ℹ fail 1\n"
	got := failureExcerpt(output, 2000)
	for _, want := range []string{"✖ broken test", "AssertionError: 0 !== 127", "ℹ fail 1", "省略"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	if len([]rune(got)) > 2000 {
		t.Fatal("excerpt exceeds limit")
	}
}

func TestFailureExcerptTAPOutput(t *testing.T) {
	passes := strings.Repeat("ok 1 - successful test\n", 150)
	got := failureExcerpt(passes+"  not ok 2 - broken test\n"+passes, 2000)
	if !strings.Contains(got, "not ok 2 - broken test") || !strings.Contains(got, "省略") || len([]rune(got)) > 2000 {
		t.Fatal(got)
	}
}

func TestFailureExcerptTAPNonMarkersKeepTail(t *testing.T) {
	for _, line := range []string{"not ok", "not okay ordinary log"} {
		t.Run(line, func(t *testing.T) {
			tail := strings.Repeat("ok 1 - successful test\n", 150) + "last line"
			got := failureExcerpt(line+"\n"+tail, 2000)
			if strings.Contains(got, line) || !strings.HasSuffix(got, "last line") || !strings.Contains(got, "省略") || len([]rune(got)) > 2000 {
				t.Fatal(got)
			}
		})
	}
}

func TestFailureExcerptNodeSuccessKeepsTail(t *testing.T) {
	for _, summary := range []string{"ℹ fail 0", "ℹ fail invalid", "ℹ fail 999999999999999999999999999999"} {
		t.Run(summary, func(t *testing.T) {
			tail := strings.Repeat("✔ successful test (1ms)\n", 150) + "last line"
			got := failureExcerpt(summary+"\n"+tail, 2000)
			if strings.Contains(got, summary) || !strings.HasSuffix(got, "last line") || !strings.Contains(got, "省略") || len([]rune(got)) > 2000 {
				t.Fatal(got)
			}
		})
	}
}

// The hold was recorded earlier by an AI call; the refusal seen in this test run makes the result unavailable.
func TestFailureExcerptEnvironmentHoldWithDeniedTest(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "i=0; while [ $i -lt 150 ]; do echo 'running example/package 0.01s'; i=$((i+1)); done; echo '--- FAIL: TestBroken'; echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, request)
		if holdErr := f.Executor.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/private/idk_secret123", Err: os.ErrPermission}); holdErr != nil {
			return answer, errors.Join(err, holdErr)
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatalf("%s: %v", state.Status, err)
	}
	reloaded := &Executor{Config: f.Executor.Config}
	if err := reloaded.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	hold := reloaded.holds.Environment
	if hold == nil || !strings.Contains(hold.Reason, "open /private/[REDACTED]") || strings.Contains(hold.Reason, "idk_secret123") {
		t.Fatalf("保留が保存されていません: %+v", hold)
	}
	if _, err := os.Stat(filepath.Join(f.Executor.Config.RuntimeDir, "control", "host-holds.json")); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"summary", "error"} {
		message := result[key].(string)
		for _, want := range []string{hold.Reason, hold.Since, "--- FAIL: TestBroken"} {
			if !strings.Contains(message, want) {
				t.Fatalf("%s に %q がありません: %s", key, want, message)
			}
		}
		if strings.Contains(message, "idk_secret123") || len([]rune(message)) > 2000 {
			t.Fatal(message)
		}
	}
}

func TestFailureExcerptImplementationEnvironmentFinish(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "i=0; while [ $i -lt 150 ]; do echo 'running example/package 0.01s'; i=$((i+1)); done; echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatalf("%s: %v", state.Status, err)
	}
	var result map[string]any
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"summary", "error"} {
		if !strings.Contains(result[key].(string), "EACCES: permission denied") {
			t.Fatalf("%s: %v", key, result[key])
		}
	}
	if result["fixedTest"] == nil || result["fixedTestCause"] == nil {
		t.Fatal(result)
	}
}

func TestFailureExcerptMessageFinish(t *testing.T) {
	f := newPipeline(t, "approve")
	detail := strings.Repeat("ok example/package\n", 180) + "--- FAIL: TestBroken\nFAIL example/package\n"
	for _, prefix := range []string{"固定テストが不合格です: ", "実行環境の問題で固定テストを実行できません: "} {
		// This is the same message constructor used by implementation and merge.
		message := fixedTestMessage(prefix, detail, 2000, false)
		record := &journal.Record{ID: 100, Result: json.RawMessage(`{}`)}
		state, err := f.Executor.finish(record, "failed", "", message, nil)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(state.Result, &result); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"summary", "error"} {
			if !strings.Contains(result[key].(string), "--- FAIL: TestBroken") {
				t.Fatalf("%s: %v", key, result[key])
			}
		}
		if !strings.Contains(message, "--- FAIL: TestBroken") || len([]rune(message)) > 2000 {
			t.Fatal(message)
		}
	}
}

func TestMergeEnvironmentHoldWithDeniedTest(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Branch = "chat/merge-environment"
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "feature")
	f.git(source, "push", f.bare, f.Branch)
	f.head = f.git(source, "rev-parse", "HEAD")
	f.pull = true
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "i=0; while [ $i -lt 150 ]; do echo 'running example/package 0.01s'; i=$((i+1)); done; echo '--- FAIL: TestBroken'; echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
	if err := f.Executor.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/private/idk_secret123", Err: os.ErrPermission}); err != nil {
		t.Fatal(err)
	}
	hold := f.Executor.holds.Environment
	r := monitorRequest{ID: "125-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}
	result, err := f.Executor.mergeRequestedPull(context.Background(), &requestProgress{Request: r, Stage: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	message, ok := result["message"].(string)
	if !ok {
		t.Fatal(result)
	}
	for _, want := range []string{hold.Reason, hold.Since, "--- FAIL: TestBroken"} {
		if !strings.Contains(message, want) {
			t.Fatalf("%q がありません: %s", want, message)
		}
	}
	if strings.Contains(message, "idk_secret123") || len([]rune(message)) > 2000 || f.merged {
		t.Fatal(result)
	}
}

func TestMissingEvidenceMessagePreservesReason(t *testing.T) {
	result := TestResult{Detail: strings.Repeat("test_progress.py . [ 50% ]\n", 200) + "idk_secret123\n"}
	rejectTestEvidence(&result, "新しい証拠拒否理由")
	for _, prefix := range []string{"固定テストの証拠が欠測です: ", "固定テストが不合格です: "} {
		message := fixedTestMessage(prefix, result.Detail, 2000, result.evidenceReason)
		if !strings.HasPrefix(message, prefix+"新しい証拠拒否理由\n") || strings.Contains(message, "idk_secret123") || !strings.Contains(message, "[REDACTED]") || len([]rune(message)) > 2000 {
			t.Fatal(message)
		}
	}
}
