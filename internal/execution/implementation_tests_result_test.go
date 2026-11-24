package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestImplementationTestResultHandoffPreservesBehavior(t *testing.T) {
	t.Run("final approval failure holds without retest or repair", func(t *testing.T) {
		e, r, d, git := finalApprovalFixture(t)
		head := r.Head
		marker := filepath.Join(t.TempDir(), "runs")
		repo := Repository{TestCommand: []string{"/bin/sh", "-c", "printf x >> " + strconv.Quote(marker) + "; exit 1"}}
		result, err := e.fixedTest(context.WithValue(context.Background(), finalApprovalTestKey{}, true), repo, r)
		if err != nil || result.Passed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		calls, prepared := 0, 0
		e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
			calls++
			return providers.Answer{}, nil
		}
		state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &result, func() error { prepared++; return nil })
		saved, loadErr := e.Store.Load(r.ID)
		runs, readErr := os.ReadFile(marker)
		if err != nil || loadErr != nil || readErr != nil || string(runs) != "x" || state == nil || state.Status != "held" || state.HoldReason != "review_held" || calls != 0 || prepared != 0 || r.FixedTestFailures != 0 || string(saved.Test) != string(encode(result)) || git("rev-parse", "HEAD") != head {
			t.Fatalf("state=%+v err=%v load=%v calls=%d prepared=%d", state, err, loadErr, calls, prepared)
		}
	})
	t.Run("provider switch resets repair budget", func(t *testing.T) {
		e, r, d, tr := completionFixture(t)
		r.Result = implementationFixture()
		r.Stage = "test"
		r.Provider = "antigravity"
		r.FixedTestFailures = 2
		r.AntigravityStrikes = []journal.ProviderFailure{{Kind: "test", Detail: "first"}}
		tr.reason = "usage limit"
		result := TestResult{Detail: "second", countableFailure: true}
		repo, runs := countingTest(t, 100, false)
		state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &result)
		saved, loadErr := e.Store.Load(r.ID)
		if err != nil || loadErr != nil || state == nil || state.HoldReason != "question" || saved.FixedTestFailures != 0 || saved.ProviderSwitch == nil || saved.ProviderSwitch.To != "claude" || runs() != 1 {
			t.Fatalf("state=%+v saved=%+v err=%v load=%v", state, saved, err, loadErr)
		}
	})
	for _, limit := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair", true: "usage limit"}[limit], func(t *testing.T) {
			e, r, d, tr := completionFixture(t)
			r.Result = implementationFixture()
			r.Stage = "test"
			if limit {
				tr.reason = "usage limit"
			}
			calls := 0
			e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
				calls++
				if !strings.Contains(req.Prompt, "rerun failure") {
					t.Fatal("rerun failure missing from repair prompt")
				}
				return providers.Answer{}, errors.New("repair stopped")
			}
			// The handoff failure is rerun once; the repair sees the rerun failure.
			test := TestResult{Detail: "handoff failure"}
			repo := Repository{TestCommand: []string{"/bin/sh", "-c", "echo rerun failure; exit 1"}, TestTimeoutSeconds: 10}
			state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &test)
			if err != nil || state == nil || r.FixedTestFailures != 1 {
				t.Fatalf("state=%+v err=%v failures=%d", state, err, r.FixedTestFailures)
			}
			saved, err := e.Store.Load(r.ID)
			var got TestResult
			if err != nil || json.Unmarshal(saved.Test, &got) != nil || got.Passed || !strings.Contains(got.Detail, "rerun failure") || saved.FixedTestFailures != 1 {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
			if limit && (calls != 0 || state.HoldReason != "question" || !strings.Contains(string(state.View), "implementationUsageStop")) {
				t.Fatalf("limit state=%+v calls=%d", state, calls)
			}
			if !limit && (calls != 1 || state.Status != "failed") {
				t.Fatalf("repair state=%+v calls=%d", state, calls)
			}
		})
	}
}

func TestImplementationTestResultClassification(t *testing.T) {
	t.Run("retest after repair", func(t *testing.T) {
		e, r, d, _ := completionFixture(t)
		r.Result = implementationFixture()
		r.Stage = "test"
		calls := 0
		e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
			calls++
			return providers.Answer{JSON: implementationFixture(), Session: req.Session, Outcome: "completed"}, nil
		}
		// The rerun fails, so the repair runs and the following retest passes.
		result := TestResult{Detail: "repair needed"}
		repo, runs := countingTest(t, 1, false)
		state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &result)
		var got TestResult
		if err != nil || state != nil || calls != 1 || runs() != 2 || json.Unmarshal(r.Test, &got) != nil || !got.Passed || strings.Contains(got.Detail, "不安定な失敗") {
			t.Fatalf("state=%+v err=%v calls=%d runs=%d failures=%d", state, err, calls, runs(), r.FixedTestFailures)
		}
	})
	for _, tc := range []struct {
		name           string
		test           TestResult
		failures       int
		status, reason string
	}{
		{"pass", TestResult{}, 2, "", ""},
		{"environment", TestResult{Unavailable: true, Detail: "unavailable", environmentHold: &environmentHold{Reason: "permission", Since: "now"}}, 1, "failed", "実行環境"},
		{"missing evidence", TestResult{Detail: "missing", evidenceMissing: true}, 1, "failed", "証拠が欠測"},
		{"repair limit", TestResult{Detail: "failure"}, 2, "failed", "固定テストが不合格"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			r.Result = implementationFixture()
			r.Stage = "test"
			r.FixedTestFailures = tc.failures
			if tc.name == "pass" {
				var err error
				tc.test, err = e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/true"}, TestTimeoutSeconds: 10}, r)
				if err != nil || !tc.test.Passed || tc.test.Observation == nil {
					t.Fatalf("fixed test: result=%+v err=%v", tc.test, err)
				}
			}
			calls := 0
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, errors.New("unexpected")
			}
			// Only an ordinary failure is rerun before the repair limit is applied.
			repo, runs := countingTest(t, 100, false)
			state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &tc.test)
			wantRuns, wantDetail := 0, tc.test.Detail
			if tc.name == "repair limit" {
				wantRuns, wantDetail = 1, "TestFlaky"
			}
			if err != nil || calls != 0 || runs() != wantRuns {
				t.Fatalf("state=%+v err=%v calls=%d runs=%d", state, err, calls, runs())
			}
			saved, err := e.Store.Load(r.ID)
			var got TestResult
			if err != nil || json.Unmarshal(saved.Test, &got) != nil || !strings.Contains(got.Detail, wantDetail) {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
			if tc.status == "" {
				if state != nil || saved.FixedTestFailures != 0 || string(saved.Test) != string(encode(tc.test)) {
					t.Fatal(state, saved)
				}
				return
			}
			if state == nil || state.Status != tc.status || !strings.Contains(string(state.Result), tc.reason) {
				t.Fatal(state)
			}
			expected := tc.failures
			if tc.name == "repair limit" {
				expected++
			}
			if saved.FixedTestFailures != expected {
				t.Fatal(saved.FixedTestFailures)
			}
		})
	}
}

// countingTest fails until the marker holds passAfter runs, so each run is counted.
func countingTest(t *testing.T, passAfter int, firstSleep bool) (Repository, func() int) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "runs")
	script := `n=$(cat "$1" 2>/dev/null | wc -c); printf x >> "$1"; ` +
		`if [ "$n" -eq 0 ] && [ "$2" = sleep ]; then sleep 5; fi; ` +
		`echo "--- FAIL: TestFlaky (0.00s)"; [ "$n" -ge "$3" ]`
	mode := "fail"
	if firstSleep {
		mode = "sleep"
	}
	repo := Repository{TestCommand: []string{"/bin/sh", "-c", script, "sh", marker, mode, strconv.Itoa(passAfter)}, TestTimeoutSeconds: 10}
	if firstSleep {
		repo.TestTimeoutSeconds = 1
	}
	return repo, func() int {
		data, err := os.ReadFile(marker)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return len(data)
	}
}

func TestFixedTestRerun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sleep bool
		first string
	}{{"flaky failure passes on rerun", false, "--- FAIL: TestFlaky"}, {"timeout passes on rerun", true, "時間切れ"}} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, d, _ := completionFixture(t)
			r.Result = implementationFixture()
			r.Stage = "test"
			repo, runs := countingTest(t, 1, tc.sleep)
			calls := 0
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, errors.New("unexpected repair")
			}
			state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", nil)
			saved, loadErr := e.Store.Load(r.ID)
			var got TestResult
			if err != nil || loadErr != nil || state != nil || calls != 0 || runs() != 2 || saved.FixedTestFailures != 0 || json.Unmarshal(saved.Test, &got) != nil || !got.Passed {
				t.Fatalf("state=%+v err=%v load=%v calls=%d runs=%d saved=%+v", state, err, loadErr, calls, runs(), saved)
			}
			if !strings.Contains(got.Detail, "不安定な失敗（再実行で合格）") || !strings.Contains(got.Detail, "1回目="+tc.first) {
				t.Fatalf("detail=%q", got.Detail)
			}
		})
	}
	t.Run("two failures repair once after one rerun with the preparer", func(t *testing.T) {
		e, r, d, _ := completionFixture(t)
		r.Result = implementationFixture()
		r.Stage = "test"
		committedTree(t, r)
		repo, runs := countingTest(t, 100, false)
		prepared := 0
		calls := 0
		e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
			calls++
			return providers.Answer{}, errors.New("repair stopped")
		}
		state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", nil, func() error { prepared++; return nil })
		saved, loadErr := e.Store.Load(r.ID)
		if err != nil || loadErr != nil || state == nil || state.Status != "failed" || calls != 1 || runs() != 2 || prepared != 2 || saved.FixedTestFailures != 1 || strings.Contains(string(saved.Test), "不安定な失敗") {
			t.Fatalf("state=%+v err=%v load=%v calls=%d runs=%d prepared=%d", state, err, loadErr, calls, runs(), prepared)
		}
	})
	t.Run("each failure after a repair gets its own rerun", func(t *testing.T) {
		e, r, d, _ := completionFixture(t)
		r.Result = implementationFixture()
		r.Stage = "test"
		// Runs: rerun of the handoff failure, retest after the repair, its rerun.
		repo, runs := countingTest(t, 2, false)
		calls := 0
		e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
			calls++
			return providers.Answer{JSON: implementationFixture(), Session: req.Session, Outcome: "completed"}, nil
		}
		result := TestResult{Detail: "handoff failure"}
		state, err := e.implementationTestResult(context.Background(), d, repo, r, "repair", &result)
		var got TestResult
		if err != nil || state != nil || calls != 1 || runs() != 3 || r.FixedTestFailures != 0 || json.Unmarshal(r.Test, &got) != nil || !got.Passed || !strings.Contains(got.Detail, "不安定な失敗（再実行で合格）") {
			t.Fatalf("state=%+v err=%v calls=%d runs=%d test=%s", state, err, calls, runs(), r.Test)
		}
	})
}

// committedTree makes r.Tree a clean commit so the preparer path can verify the head.
func committedTree(t *testing.T, r *journal.Record) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", r.Tree, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "base")
	r.Head = run("rev-parse", "HEAD")
}

func TestFlakyFailureDetailKeepsPassingSummary(t *testing.T) {
	detail := flakyFailureDetail("TestFlaky", strings.Repeat("x", 5000)+"\nok  summary")
	if !strings.HasPrefix(detail, "不安定な失敗（再実行で合格）: 1回目=TestFlaky\n") || !strings.HasSuffix(detail, "ok  summary") || len([]rune(detail)) != 4000 {
		t.Fatalf("detail head=%q tail=%q runes=%d", clip(detail, 40), detail[len(detail)-20:], len([]rune(detail)))
	}
}
