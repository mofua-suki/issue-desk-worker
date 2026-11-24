package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"strings"
	"time"
)

// Tests retain the current tree and provider conversation. A provider switch gets its own repair budget.
func (e *Executor) implementationTests(ctx context.Context, d Descriptor, repository Repository, r *journal.Record, prompt string, prepare ...func() error) (*intake.State, error) {
	return e.implementationTestResult(ctx, d, repository, r, prompt, nil, prepare...)
}

// implementationTestResult handles an already executed result before running any
// further tests. After a repair, subsequent targets follow the normal test path.
func (e *Executor) implementationTestResult(ctx context.Context, d Descriptor, repository Repository, r *journal.Record, prompt string, initial *TestResult, prepare ...func() error) (*intake.State, error) {
	c := &testCycle{e: e, ctx: ctx, d: d, repository: repository, r: r, prompt: prompt, prepare: prepare}
	failures := r.FixedTestFailures
	for {
		if initial == nil && r.Stage == "test_report" {
			if state, done, err := c.acceptReport(); done {
				return state, err
			}
		}
		var test TestResult
		var err error
		if initial != nil {
			test = *initial
			initial = nil
		} else if test, err = c.run(); err != nil {
			return nil, err
		}
		test, state, done, err := c.settle(test)
		if done {
			return state, err
		}
		switched := false
		if test.countableFailure {
			switched, err = e.noteAntigravityFailure(ctx, d, r, "test", test.Detail)
			if err != nil {
				return nil, err
			}
		}
		failures++
		if switched {
			failures = 0
		}
		r.FixedTestFailures = failures
		if err := e.Store.Save(r); err != nil {
			return nil, err
		}
		if failures >= 3 {
			if e.fixedTestNotAttributable(r, test) {
				excludeGeneration(d, r)
			}
			return c.stop("failed", "", fixedTestMessage("固定テストが不合格です: ", test.Detail, 2000, test.evidenceReason), nil)
		}
		if err := e.checkImplementationUsage(ctx, d, r, true, true); err != nil {
			return c.stop("held", "question", err.Error(), map[string]any{"implementationUsageStop": true})
		}
		if state, done, err := c.repair(); done {
			return state, err
		}
	}
}

// testCycle is one run of fixed tests with up to three AI repairs.
type testCycle struct {
	e          *Executor
	ctx        context.Context
	d          Descriptor
	repository Repository
	r          *journal.Record
	prompt     string
	prepare    []func() error
}

func (c *testCycle) stop(status, reason, summary string, view map[string]any) (*intake.State, error) {
	if c.d.finalApprovalFixedTest && status == "held" && reason == "review_held" {
		if view == nil {
			view = map[string]any{}
		}
		view["finalApprovalHold"] = true
	}
	out, err := c.e.finish(c.r, status, reason, summary, view)
	return &out, err
}

// acceptReport checks the saved implementation report before testing it.
// done is true when the job must stop here.
func (c *testCycle) acceptReport() (*intake.State, bool, error) {
	e, ctx, d, r := c.e, c.ctx, c.d, c.r
	if d.finalApprovalFixedTest {
		var report Implementation
		reportErr := providers.ValidateReport(r.Result, true)
		decodeErr := json.Unmarshal(r.Result, &report)
		_, _, transferErr := transferReport(r.Result)
		if reportErr != nil || decodeErr != nil || transferErr != nil || report.Status != "completed" || len(report.Questions) > 0 {
			if err := e.checkpoint(ctx, d, r, "test_report"); err != nil {
				return nil, true, err
			}
			reason := "報告が未完了または質問を含みます"
			if reportErr != nil {
				reason = reportErr.Error()
			} else if decodeErr != nil {
				reason = decodeErr.Error()
			} else if transferErr != nil {
				reason = transferErr.Error()
			}
			state, err := c.stop("held", "review_held", "報告を安全に処理できません: "+clip(e.redact(reason), 1500)+"。headを変えると最終承認が未了に戻ります", nil)
			return state, true, err
		}
		return nil, false, nil
	}
	for {
		if terminal, err := e.completeImplementationReport(ctx, d, r, true); err != nil || terminal != nil {
			return terminal, true, err
		}
		continued, err := e.continueIncomplete(ctx, d, r, c.prompt, time.Now)
		if err != nil {
			return nil, true, err
		}
		if !continued {
			break
		}
	}
	if _, _, err := transferReport(r.Result); err != nil {
		state, err := c.stop("held", "review_held", "受入条件の移送を報告できません: "+err.Error(), nil)
		return state, true, err
	}
	var repaired Implementation
	_ = json.Unmarshal(r.Result, &repaired)
	if repaired.Status != "completed" || len(repaired.Questions) > 0 {
		state, err := c.stop("held", "question", incompleteHoldSummary(r, repaired), nil)
		return state, true, err
	}
	return nil, false, nil
}

// run checkpoints the test stage and runs the fixed test.
func (c *testCycle) run() (TestResult, error) {
	if err := c.e.checkpoint(c.ctx, c.d, c.r, "test"); err != nil {
		return TestResult{}, err
	}
	testCtx := c.ctx
	if c.d.finalApprovalFixedTest {
		testCtx = context.WithValue(c.ctx, finalApprovalTestKey{}, true)
	}
	return c.e.fixedTest(testCtx, c.repository, c.r, c.prepare...)
}

// settle records the test and decides it. A failure that is neither
// unavailable nor missing evidence is rerun once on the same tree before any
// repair; a pass there is recorded as flaky. done is false only for a failure
// that the AI should repair, returned as the test.
func (c *testCycle) settle(test TestResult) (TestResult, *intake.State, bool, error) {
	e, d, r := c.e, c.d, c.r
	firstFailure := ""
	for {
		if firstFailure != "" && test.Passed {
			test.Detail = flakyFailureDetail(firstFailure, test.Detail)
		}
		r.Test = encode(test)
		if d.finalApprovalFixedTest && !test.Passed {
			reason := "固定テストが不合格または実行不能です: "
			if test.evidenceMissing {
				reason = "固定テストの対象または証拠を確認できません: "
			}
			state, err := c.stop("held", "review_held", fixedTestMessage(reason, e.redact(test.Detail), 1800, test.evidenceReason)+"\nheadを変えると最終承認が未了に戻ります", nil)
			return test, state, true, err
		}
		if receipt := r.ReportCompletion; receipt != nil && receipt.Validated && receipt.EditsStarted && !receipt.EditsApplied {
			state, err := c.stop("held", "review_held", test.Detail, nil)
			return test, state, true, err
		}
		if test.Passed {
			// The repair budget belongs to this failing test cycle. A later
			// publish/merge retest on a new base starts with the original budget.
			r.FixedTestFailures = 0
			return test, nil, true, e.Store.Save(r)
		}
		if test.Unavailable {
			excludeGeneration(d, r)
			hold := test.environmentHold
			prefix := "実行環境の問題で固定テストを実行できません: 保留理由: " + e.redact(hold.Reason) + "、開始時刻: " + hold.Since + "\n"
			state, err := c.stop("failed", "", fixedTestMessage(prefix, test.Detail, 2000, test.evidenceReason), map[string]any{"environment": map[string]any{"kind": "permission", "autoRetry": false}})
			return test, state, true, err
		}
		if test.evidenceMissing {
			state, err := c.stop("failed", "", fixedTestMessage("固定テストの証拠が欠測です: ", test.Detail, 2000, true), nil)
			return test, state, true, err
		}
		if firstFailure != "" {
			return test, nil, false, nil
		}
		firstFailure = fixedTestFailureSummary(test)
		var err error
		if test, err = e.fixedTest(c.ctx, c.repository, r, c.prepare...); err != nil {
			return test, nil, true, err
		}
	}
}

// repair asks the AI to fix the failing test and receives its report. done is
// true when the job must stop here.
func (c *testCycle) repair() (*intake.State, bool, error) {
	e, ctx, d, r := c.e, c.ctx, c.d, c.r
	repairStarted := time.Now()
	answer, callErr := e.callAI(ctx, d, r, c.prompt+"\n固定テストの結果(データ): "+string(r.Test)+"\n原因と同種の経路を直し、テストの削除・省略・弱体化をせず同じ契約で結果を返してください。", false, nil)
	savedReport := callErr == nil && ctx.Err() == nil
	if savedReport {
		if err := e.receiveImplementationReport(r, answer.JSON, "test_report"); err != nil {
			return nil, true, err
		}
	}
	if r.Timing != nil {
		ms := time.Since(repairStarted).Milliseconds()
		if r.Timing.AIRepairMillis != nil {
			ms += *r.Timing.AIRepairMillis
		}
		r.Timing.AIRepairMillis = &ms
		if err := e.saveTiming(r); err != nil {
			return nil, true, err
		}
	}
	if err := e.flushUsage(ctx, r); err != nil {
		return nil, true, err
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	if callErr != nil {
		state, err := c.stop("failed", "", callErr.Error(), nil)
		return state, true, err
	}
	return nil, false, nil
}

// fixedTestFailureSummary names the first failure kept when a rerun passes.
func fixedTestFailureSummary(test TestResult) string {
	if test.Diagnostic != nil && test.Diagnostic.Reason == "timeout" {
		return "時間切れ"
	}
	// Observations are maps in memory and []any after a journal round trip.
	var failures []struct {
		Test string `json:"test"`
	}
	if raw, err := json.Marshal(test.Observation["failures"]); err == nil && json.Unmarshal(raw, &failures) == nil {
		parts := []string{}
		for _, failure := range failures {
			if failure.Test != "" && len(parts) < 10 {
				parts = append(parts, failure.Test)
			}
		}
		if len(parts) > 0 {
			return "失敗したテスト: " + strings.Join(parts, ", ")
		}
	}
	if detail := strings.TrimSpace(test.Detail); detail != "" {
		return clip(detail, 300)
	}
	return "不合格"
}

func flakyFailureDetail(first, detail string) string {
	note := []rune("不安定な失敗（再実行で合格）: 1回目=" + first + "\n")
	// Keep the note and the tail of the passing output, where the summary line is.
	rest := []rune(detail)
	if room := 4000 - len(note); len(rest) > room {
		rest = rest[len(rest)-max(room, 0):]
	}
	return clip(string(note)+string(rest), 4000)
}
