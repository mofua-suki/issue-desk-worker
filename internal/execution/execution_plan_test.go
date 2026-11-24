package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func planJob(f *pipelineFixture, kind, owner, phase string) {
	f.Job.Raw = encode(Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project", Issue: 7, Provider: "codex", Generation: 0, View: map[string]json.RawMessage{
		"executionPlanSHA": encode(strings.Repeat("a", 64)),
		"executionPlan":    encode(executionPlan{Version: 1, Requirements: []executionRequirement{{ID: "one", Criterion: "条件の全文", Owner: owner, Phase: phase, Method: "根拠を確認", Expected: "一致", EvidenceKind: kind}}}),
	}}})
}

func TestExternalPlanNeverStartsImplementation(t *testing.T) {
	f := newPipeline(t, "approve")
	planJob(f, "external_check", "controller", "deployment")
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		t.Fatal("外部確認を実装AIへ渡した")
		return providers.Answer{}, nil
	}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 {
		t.Fatal(state, err)
	}
	var view map[string]any
	_ = json.Unmarshal(state.View, &view)
	if view["executionWait"] != "controller" {
		t.Fatal(view)
	}
}

func TestReportPlanUsesReadOnlyCallWithoutProductTests(t *testing.T) {
	f := newPipeline(t, "approve")
	planJob(f, "report_comment", "reporter", "report")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	const priorSummary = "以前に投稿した検証報告"
	d.Result = encode(map[string]any{"previousImplementationResult": Implementation{Status: "completed", Summary: priorSummary}})
	f.Job.Raw = encode(d)
	calls := 0
	f.Executor.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		if !req.Policy.ReadOnly || req.Review {
			t.Fatal("報告専用の読み取りではありません")
		}
		_, data, ok := strings.Cut(req.Prompt, "\n報告対象(データ): ")
		var report struct {
			PreviousResult Implementation
		}
		if !ok || json.Unmarshal([]byte(data), &report) != nil || report.PreviousResult.Summary != priorSummary {
			t.Fatal("受付に保存された過去の報告を照合できません")
		}
		return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: "訂正: 対象の実装と記録を照合しました。根拠はREADME.md。"})}, nil
	}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || calls != 1 || f.MergePosts != 0 {
		t.Fatal(state, err, calls)
	}
	if len(state.ControllerReport) == 0 || len(state.ControllerTest) != 0 {
		t.Fatal("投稿証拠と固定検証を混同", state)
	}
	saved, _ := f.Executor.Store.Load(1)
	if hasJSON(saved.Test) {
		t.Fatal("報告訂正に製品テストを要求", saved.Test)
	}
}

func TestMixedPlanReportsAfterCodeMerge(t *testing.T) {
	testMixedPlanReports(t, "")
}
func TestMixedPlanResumesReportAfterPostingFailure(t *testing.T) {
	testMixedPlanReports(t, "read")
}
func TestMixedPlanReconcilesLostPostResponseAcrossGeneration(t *testing.T) {
	testMixedPlanReports(t, "response")
}
func TestMixedPlanUsesApprovedRunLimitOnReportResume(t *testing.T) {
	testMixedPlanReports(t, "approval")
}
func TestMixedPlanRecoversIncompleteReport(t *testing.T) {
	for _, failure := range []string{"question", "saved-incomplete", "missing-summary"} {
		t.Run(failure, func(t *testing.T) { testMixedPlanReports(t, failure) })
	}
}

func TestMixedPlanRetriesUsageDeliveryWithoutRegenerating(t *testing.T) {
	testMixedPlanReports(t, "usage")
}

func TestMixedPlanTransientRetryUsesFreshConversation(t *testing.T) {
	testMixedPlanReports(t, "transient")
}

func TestMixedPlanSendsUsageAfterFinalReportError(t *testing.T) {
	testMixedPlanReports(t, "final-error")
}

func testMixedPlanReports(t *testing.T, failure string) {
	f := newPipeline(t, "approve")
	f.CurrentReviewNotes = json.RawMessage(`{"owner/project":[{"kind":"security","text":"通常の実装で参照する注意点"}]}`)
	f.AllowUsage = true
	if failure == "approval" {
		f.Executor.Config.Assignments.ImplementationRunLimit = 1
	}
	tr := completionAPI(t, f)
	f.Executor.transientWait = func(context.Context, time.Duration) error { return nil }
	f.Executor.secret = "fixture-report-private-value"
	var d Descriptor
	_ = json.Unmarshal(f.Job.Raw, &d)
	d.View["executionPlanSHA"] = encode(strings.Repeat("a", 64))
	plan := executionPlan{Version: 1, Requirements: []executionRequirement{
		{ID: "code", Criterion: "機能を実装", Owner: "implementer", Phase: "verification", Method: "固定検証", Expected: "成功", EvidenceKind: "fixed_test"},
		{ID: "report", Criterion: "訂正を投稿", Owner: "reporter", Phase: "report", Method: "根拠を照合", Expected: "投稿済み", EvidenceKind: "report_comment"},
	}}
	if failure == "" {
		plan.Requirements = append(plan.Requirements, executionRequirement{
			ID: "deploy", Criterion: "配備先の稼働", Owner: "controller", Phase: "deployment",
			Method: "配備先の稼働記録を確認", Expected: "対象版が稼働中", EvidenceKind: "external_check",
		})
	}
	d.View["executionPlan"] = encode(plan)
	d.View["instructions"] = encode([]instructionContent{{Text: "前回の報告を訂正", Context: "固定検証の結果を根拠にする"}})
	f.Job.Raw = encode(d)
	wantTitle, wantBody := "Implement feature", "受入条件: feature.txtを生成する"
	var initialIssueReads atomic.Int32
	if failure == "" {
		wantTitle, wantBody = "Implement feature (updated)", "受入条件: feature.txtとマージ結果を確認する"
		f.GiteaOverride = func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method != http.MethodGet || req.URL.Path != "/api/v1/repos/owner/project/issues/7" {
				return false
			}
			initialIssueReads.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.merged {
				return false
			}
			_, _ = w.Write(encode(map[string]any{"number": 7, "title": wantTitle, "body": wantBody, "state": "closed"}))
			return true
		}
	}
	base := f.Executor.Call
	reportInputTokens := int64(37)
	reportOutputTokens := int64(5)
	reports := 0
	reviewedPlan := false
	var incomplete json.RawMessage
	wantEvidence := ""
	switch failure {
	case "question":
		wantEvidence = "どの固定版を照合しますか"
		incomplete = encode(Implementation{Status: "question", Questions: []string{wantEvidence + " " + f.Executor.secret + strings.Repeat("あ", 1500)}})
	case "saved-incomplete":
		wantEvidence = "確認用の資料がありません"
		incomplete = encode(Implementation{Status: "failed", Incomplete: encode(map[string]any{"reason": wantEvidence + " " + f.Executor.secret})})
	case "missing-summary":
		wantEvidence = "要約がありません"
		incomplete = encode(Implementation{Status: "completed"})
	}
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if failure == "" && req.Review {
			_, data, ok := strings.Cut(req.Prompt, "\n根拠(データ): ")
			var input struct {
				ExecutionPlan executionPlan
			}
			if !ok || json.NewDecoder(strings.NewReader(data)).Decode(&input) != nil || !reflect.DeepEqual(input.ExecutionPlan, plan) {
				t.Fatalf("独立レビューに確認の分担・手段・期待結果が届きません: %+v", input.ExecutionPlan)
			}
			reviewedPlan = true
		}
		if req.Policy.ReadOnly && !req.Review {
			reports++
			if req.Session != "" || req.OnSession != nil || req.Settings.Provider != "codex" {
				t.Fatal("報告に実装会話が渡されました", req.Session)
			}
			if reports == 1 {
				saved, err := f.Executor.Store.Load(1)
				if err != nil || saved.Session != "implementation-session" {
					t.Fatal("実装会話のある経路を通っていません", err)
				}
				if failure == "transient" {
					return providers.Answer{Session: "failed-report-session", Transient: true}, errors.New("capacity")
				}
				if failure == "final-error" {
					return providers.Answer{Usage: providers.Usage{Input: &reportInputTokens, Output: &reportOutputTokens}}, errors.New("report provider failed")
				}
				if incomplete != nil {
					return providers.Answer{JSON: incomplete, Session: "incomplete-report-session"}, nil
				}
				if failure == "usage" {
					tr.refuseUsage = true
				}
			}
			_, data, ok := strings.Cut(req.Prompt, "\n報告対象(データ): ")
			var report struct {
				Head           string
				Title          string
				Body           string
				Requirements   []executionRequirement
				Instructions   []instructionContent
				Implementation Implementation
				Test           TestResult
			}
			if !ok || json.Unmarshal([]byte(data), &report) != nil {
				t.Fatal("報告担当に入力データが届きません")
			}
			if report.Head != f.head || report.Title != wantTitle || report.Body != wantBody ||
				len(report.Requirements) != 1 || report.Requirements[0].ID != "report" ||
				len(report.Instructions) != 1 || report.Instructions[0].Context != "固定検証の結果を根拠にする" ||
				(incomplete != nil && report.Instructions[0].Text != "不足した根拠を追記したので訂正") ||
				report.Implementation.Summary != "機能を実装" || !report.Test.Passed {
				t.Fatalf("報告訂正の対象・指示・根拠が不足しています: %+v", report)
			}
			if failure == "read" {
				f.NoticeReadStatus = 503
			}
			if failure == "response" {
				f.DropNoticeResponse = true
				f.HideNotices = true
			}
			return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: "訂正: 実装と固定検証の結果を照合しました"}), Session: "new-report-session"}, nil
		}
		answer, err := base(ctx, req)
		if !req.Review {
			answer = conversationAnswer(t, req, answer)
		}
		return answer, err
	}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if failure == "" && initialIssueReads.Load() != 2 {
		t.Fatalf("新規報告を含む実行でIssueを%d回取得しました", initialIssueReads.Load())
	}
	if failure != "" && failure != "transient" {
		expectedReports := 1
		if failure == "approval" {
			expectedReports = 0
		}
		if err != nil || state.Status != "held" || f.MergePosts != 1 || reports != expectedReports {
			t.Fatal(state, err, reports)
		}
		saved, _ := f.Executor.Store.Load(1)
		if !saved.ExecutionReportPending {
			t.Fatal("未完了の報告工程が保存されません")
		}
		if failure == "final-error" {
			type usageRow struct {
				InputTokens  int64
				OutputTokens int64
			}
			if saved.UsageSent != len(saved.Usage) || !slices.ContainsFunc(tr.usageRequests, func(batch json.RawMessage) bool {
				var request struct {
					Usage []usageRow
				}
				return json.Unmarshal(batch, &request) == nil &&
					slices.Contains(request.Usage, usageRow{InputTokens: reportInputTokens, OutputTokens: reportOutputTokens})
			}) {
				t.Fatalf("最終エラーの使用量が受付に届きません: %s", tr.usageRequests)
			}
			for _, comment := range f.Comments {
				body, _ := comment["body"].(string)
				if strings.Contains(body, "<!-- issue-desk-execution-report:") {
					t.Fatal("最終エラー後に報告コメントが投稿されました")
				}
			}
			return
		}
		if saved.Session != "implementation-session" {
			t.Fatal("保留時に実装会話が上書きされました", saved.Session)
		}
		if failure == "response" {
			saved.ReviewBaseline = nil
			if err := f.Executor.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
		}
		if incomplete != nil {
			if hasJSON(saved.ExecutionReportResult) || saved.UsageSent != len(saved.Usage) ||
				!strings.Contains(string(state.Result), wantEvidence) || strings.Contains(string(state.Result), f.Executor.secret) ||
				strings.Contains(string(state.Result), strings.Repeat("あ", 1000)) {
				t.Fatal("未完了の報告の保存・使用量・停止表示が不正です", state.Result)
			}
			if failure == "saved-incomplete" {
				saved.ExecutionReportResult = incomplete
				saved.ExecutionReportHead = saved.Head
				if err := f.Executor.Store.Save(saved); err != nil {
					t.Fatal(err)
				}
			}
			d.View["instructions"] = encode([]instructionContent{{Text: "不足した根拠を追記したので訂正", Context: "固定検証の結果を根拠にする"}})
		}
		tr.refuseUsage = false
		f.NoticeReadStatus = 0
		f.DropNoticeResponse = false
		f.HideNotices = false
		if failure == "approval" {
			if saved.ReportCompletionRuns != 0 {
				t.Fatal("上限で起動しなかった報告を計上", saved.ReportCompletionRuns)
			}
			d.View["overLimitApprovals"] = encode([]map[string]any{{"id": "approved", "statement": "今回の対象の追加1回を承認", "implementationRuns": 1}})
		}
		d.Generation++
		d.View["implementationResumes"] = encode(1)
		f.Job.Raw = encode(d)
		var issueReads atomic.Int32
		wantIssueReads := int32(1)
		if failure == "approval" || incomplete != nil {
			wantIssueReads = 2
		}
		f.GiteaOverride = func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method != http.MethodGet || req.URL.Path != "/api/v1/repos/owner/project/issues/7" || issueReads.Add(1) <= wantIssueReads {
				return false
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		state, err = f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
		if issueReads.Load() != wantIssueReads {
			t.Fatalf("報告の再開でIssueを%d回取得しました（期待値: %d）", issueReads.Load(), wantIssueReads)
		}
	}
	if failure == "response" {
		count := 0
		for _, comment := range f.Comments {
			body, _ := comment["body"].(string)
			if strings.Contains(body, "<!-- issue-desk-execution-report:") {
				count++
			}
		}
		if count != 1 {
			t.Fatal("訂正を二重投稿", count)
		}
		if err != nil || state.Status != "held" || f.MergePosts != 1 || len(state.ControllerTest) == 0 || len(state.ControllerReport) != 0 || reports != 1 {
			t.Fatal("別世代の投稿を引き継がず保留する必要があります", state, err, reports)
		}
		return
	}
	expectedReports := 1
	if incomplete != nil || failure == "transient" {
		expectedReports = 2
	}
	if err != nil || state.Status != "merged" || f.MergePosts != 1 || reports != expectedReports || len(state.ControllerTest) == 0 || len(state.ControllerReport) == 0 {
		t.Fatal(state, err, reports)
	}
	if failure == "" && !reviewedPlan {
		t.Fatal("独立レビューが実行されませんでした")
	}
	if failure == "" || failure == "transient" {
		saved, err := f.Executor.Store.Load(1)
		if err != nil || saved.Session != "implementation-session" {
			t.Fatal("成功時に実装会話が上書きされました", err)
		}
	}
}

func TestRequiredCapabilityStopsBeforeImplementation(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	_ = json.Unmarshal(f.Job.Raw, &d)
	d.View["executionPlanSHA"] = encode(strings.Repeat("a", 64))
	d.View["executionPlan"] = encode(executionPlan{Version: 1, Requirements: []executionRequirement{{ID: "windows", Criterion: "Windows実行", Owner: "implementer", Phase: "verification", Method: "OS上で検証", Expected: "成功", EvidenceKind: "fixed_test", Capabilities: []string{"windows"}}}})
	f.Job.Raw = encode(d)
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		t.Fatal("能力不足で実装を開始")
		return providers.Answer{}, nil
	}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatal(state, err)
	}
	var view map[string]any
	_ = json.Unmarshal(state.View, &view)
	if view["executionWait"] != "capability" {
		t.Fatal(view)
	}
}

func TestReportOnlyReconcilesLostResponseAcrossGeneration(t *testing.T) {
	f := newPipeline(t, "approve")
	planJob(f, "report_comment", "reporter", "report")
	calls := 0
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		f.DropNoticeResponse = true
		f.HideNotices = true
		return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: "訂正: 固定版の資料を照合しました"})}, nil
	}
	first, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || first.Status != "held" || calls != 1 {
		t.Fatal(first.Status, err, calls)
	}
	f.DropNoticeResponse = false
	f.HideNotices = false
	var d Descriptor
	if err = json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Generation++
	d.View["implementationResumes"] = encode(1)
	f.Job.Raw = encode(d)
	second, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || second.Status != "held" || calls != 1 || len(second.ControllerReport) != 0 {
		t.Fatal(second.Status, err, calls)
	}
	count := 0
	for _, c := range f.Comments {
		body, _ := c["body"].(string)
		if strings.Contains(body, "<!-- issue-desk-execution-report:") {
			count++
		}
	}
	if count != 1 {
		t.Fatal("報告を二重投稿", count)
	}
}

func TestAntigravityReportPlanStopsBeforeProviderCall(t *testing.T) {
	f := newPipeline(t, "approve")
	planJob(f, "report_comment", "reporter", "report")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Provider = "antigravity"
	f.Job.Raw = encode(d)
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		t.Fatal("読み取り専用制約を保証できない実行先で報告を起動しました")
		return providers.Answer{}, nil
	}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || !strings.Contains(string(state.Result), "Antigravity") ||
		!strings.Contains(string(state.Result), "読み取り専用") {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Provider != "antigravity" || saved.ProviderSwitch != nil || len(saved.Usage) != 0 {
		t.Fatal("実行先または使用量が変わりました", err)
	}
}

func TestExecutionReportFenceLeavesRunAvailable(t *testing.T) {
	f, d, r := executionReportFixture(t)
	e := f.Executor
	e.Config.Assignments.ImplementationRunLimit = 1
	if err := e.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	plan, err := parseExecutionPlan(d)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	const summary = "対象版を照合しました"
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		saved, err := e.Store.Load(r.ID)
		if err != nil || saved == nil || saved.Runs != 1 || len(saved.StartedGenerations) != 1 || saved.StartedGenerations[0] != d.Generation {
			t.Fatal("AI起動前に開始が保存されていません", saved, err)
		}
		return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: summary})}, nil
	}
	lost := errors.New("lease lost")
	checks := 0
	err = e.deliverExecutionReport(t.Context(), d, r, "報告", "", plan, func(context.Context) error {
		checks++
		if checks == 2 {
			return lost
		}
		return nil
	}, false)
	if !errors.Is(err, lost) || calls != 0 {
		t.Fatal(err, calls)
	}
	if r.Runs != 0 || r.ReportCompletionRuns != 0 || len(r.StartedGenerations) != 0 {
		t.Fatal("未起動の報告が計上されました", r.Runs, r.ReportCompletionRuns, r.StartedGenerations)
	}
	saved, err := e.Store.Load(r.ID)
	if err != nil || saved == nil || saved.Runs != 0 || saved.ReportCompletionRuns != 0 || len(saved.StartedGenerations) != 0 {
		t.Fatal("未起動の報告が計上されました", saved, err)
	}
	checks = 0
	err = e.deliverExecutionReport(t.Context(), d, saved, "報告", "", plan, func(context.Context) error {
		checks++
		if checks == 3 {
			return lost
		}
		return nil
	}, false)
	if !errors.Is(err, lost) || calls != 1 || f.NoticePosts != 0 {
		t.Fatal("送信直前の担当喪失", err, calls, f.NoticePosts)
	}
	if err := e.recoverNotifications(t.Context()); err != nil || f.NoticePosts != 0 {
		t.Fatal("一般回復", err, f.NoticePosts)
	}

	saved, err = e.Store.Load(r.ID)
	if err != nil || saved == nil {
		t.Fatal(saved, err)
	}
	fence := func(context.Context) error { return nil }
	f.CurrentLeaseEpoch = saved.Epoch + 1
	err = e.deliverExecutionReport(t.Context(), d, saved, "報告", "", plan, fence, false)
	if err == nil || calls != 1 || f.NoticePosts != 0 {
		t.Fatal("所有者の不一致", err, calls, f.NoticePosts)
	}
	if got := outboxStages(t, e); !slices.Equal(got, []string{"prepared"}) {
		t.Fatal("未送信の報告が保持されていません", got)
	}
	saved.Epoch++
	// Resume again from the journal snapshot taken before recording the receipt.
	beforeReceipt := *saved
	err = e.deliverExecutionReport(t.Context(), d, saved, "報告", "", plan, fence, false)
	if err != nil || calls != 1 || !hasJSON(saved.ExecutionReport) || f.NoticePosts != 1 {
		t.Fatal(err, calls, f.NoticePosts)
	}
	if len(f.Comments) != 1 {
		t.Fatal("生成済みの報告が投稿されていません", f.Comments)
	}
	body, _ := f.Comments[0]["body"].(string)
	if !strings.HasPrefix(body, summary+"\n") {
		t.Fatal("生成済みの報告と投稿内容が一致しません", body)
	}
	err = e.deliverExecutionReport(t.Context(), d, &beforeReceipt, "報告", "", plan, fence, false)
	if err != nil || calls != 1 || !hasJSON(beforeReceipt.ExecutionReport) || f.NoticePosts != 1 {
		t.Fatal("同じ報告の再開", err, calls, f.NoticePosts)
	}
}

func TestExecutionReportRecoveryReconcilesWithoutReposting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"404", http.StatusNotFound},
		{"410", http.StatusGone},
		{"422", http.StatusUnprocessableEntity},
		{"discarded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d, r := executionReportFixture(t)
			e := f.Executor
			plan, err := parseExecutionPlan(d)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			const summary = "対象版を照合しました"
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: summary})}, nil
			}
			ctx := t.Context()
			fence := func(context.Context) error { return nil }
			f.DropNoticeResponse, f.HideNotices = true, true
			err = e.deliverExecutionReport(ctx, d, r, "報告", "", plan, fence, false)
			if err == nil || hasJSON(r.ExecutionReport) || f.NoticePosts != 1 || len(f.Comments) != 1 {
				t.Fatal("投稿の応答消失", err, f.NoticePosts, f.Comments)
			}
			wantStage := "attempted"
			if tc.status == 0 {
				// A report discarded by an earlier Worker may already be visible in Gitea.
				marker := fmt.Sprintf(executionReportMarkerPrefix+"%d:g%d:%s:%s -->", r.ID, r.Generation, r.Head, r.ExecutionPlanSHA)
				wantStage = "discarded"
				n := notification{JobID: r.ID, Epoch: r.Epoch, Host: e.Config.Host,
					Author: e.Config.Assignments.WorkerAccounts[e.Config.Host], Repo: r.Repo, Target: r.Issue,
					Kind: "comment", Marker: marker, Head: r.Head,
					Body: f.Comments[0]["body"].(string), Stage: wantStage}
				if err := e.saveNotification(n); err != nil {
					t.Fatal(err)
				}
			}
			f.NoticeReadStatus = tc.status
			if err := e.recoverNotifications(ctx); err != nil || f.NoticePosts != 1 {
				t.Fatal("一般回復", err, f.NoticePosts)
			}
			f.NoticeReadStatus = 0
			f.DropNoticeResponse = false
			r, err = e.Store.Load(r.ID)
			if err != nil || r == nil {
				t.Fatal(r, err)
			}
			r.Epoch++
			f.CurrentLeaseEpoch = r.Epoch
			err = e.deliverExecutionReport(ctx, d, r, "報告", "", plan, fence, false)
			if err == nil || hasJSON(r.ExecutionReport) || calls != 1 || f.NoticePosts != 1 {
				t.Fatal("別epochでの照合待ち", err, calls, f.NoticePosts)
			}
			if got := outboxStages(t, e); !slices.Equal(got, []string{wantStage}) {
				t.Fatal("照合待ちの報告の状態", got)
			}
			f.HideNotices = false
			if err := e.recoverNotifications(ctx); err != nil || f.NoticePosts != 1 {
				t.Fatal("一般回復での目印照合", err, f.NoticePosts)
			}
			if got := outboxStages(t, e); !slices.Equal(got, []string{"sent"}) {
				t.Fatal("目印の確認結果が保存されていません", got)
			}
			err = e.deliverExecutionReport(ctx, d, r, "報告", "", plan, fence, false)
			if err != nil || !hasJSON(r.ExecutionReport) || calls != 1 || f.NoticePosts != 1 {
				t.Fatal("報告の証跡回収", err, calls, f.NoticePosts)
			}
		})
	}
}

func TestFinishExecutionReportsRequiresReportHead(t *testing.T) {
	for _, moved := range []string{"before-generation", "before-post", "after-post"} {
		t.Run(moved, func(t *testing.T) {
			f, _, r := executionReportFixture(t)
			f.AllowUsage = true
			e := f.Executor
			// An empty commit changes HEAD while keeping a clean tree.
			moveHead := func() {
				f.git(r.Tree, "commit", "--allow-empty", "-m", "Different report target")
			}
			calls := 0
			const firstSummary = "対象版を照合しました"
			const retrySummary = "復元した対象版で報告を作り直しました"
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				if moved == "before-post" {
					if calls > 1 {
						return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: retrySummary})}, nil
					}
					moveHead()
				}
				return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: firstSummary})}, nil
			}
			fence := func(context.Context) error { return nil }
			if moved == "before-generation" {
				moveHead()
			}
			err := e.finishExecutionReports(t.Context(), r, fence)
			if moved == "after-post" {
				if err != nil || calls != 1 || !hasJSON(r.ExecutionReport) || f.NoticePosts != 1 {
					t.Fatal(err, calls, f.NoticePosts)
				}
				saved, err := e.Store.Load(r.ID)
				if err != nil || saved == nil || !hasJSON(saved.ExecutionReport) {
					t.Fatal("投稿済み報告が保存されていません", err)
				}
				moveHead()
				var issueReads atomic.Int32
				f.GiteaOverride = func(w http.ResponseWriter, req *http.Request) bool {
					if req.Method != "GET" || req.URL.Path != "/api/v1/repos/owner/project/issues/7" {
						return false
					}
					issueReads.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				}
				state, err := e.finishMerged(t.Context(), saved, fence, "マージ完了", nil)
				if err != nil || state.Status != "merged" || calls != 1 || f.NoticePosts != 1 || issueReads.Load() != 0 {
					t.Fatal(state, err, calls, f.NoticePosts, issueReads.Load())
				}
				return
			}
			wantCalls := 0
			if moved == "before-post" {
				wantCalls = 1
			}
			if err == nil || calls != wantCalls || f.NoticePosts != 0 || hasJSON(r.ExecutionReport) {
				t.Fatal(err, calls, f.NoticePosts)
			}
			if moved == "before-post" {
				saved, err := e.Store.Load(r.ID)
				if err != nil || saved == nil || hasJSON(saved.ExecutionReportResult) {
					t.Fatal("HEAD変更中に生成した報告が保存されました", err)
				}
				f.git(r.Tree, "checkout", "--detach", r.Head)
				err = e.finishExecutionReports(t.Context(), saved, fence)
				if err != nil || calls != 2 || f.NoticePosts != 1 || len(f.Comments) != 1 {
					t.Fatal(err, calls, f.NoticePosts, f.Comments)
				}
				body, _ := f.Comments[0]["body"].(string)
				if !strings.HasPrefix(body, retrySummary+"\n\n") || strings.Contains(body, firstSummary) {
					t.Fatal(body)
				}
			}
		})
	}
}

func executionReportFixture(t *testing.T) (*pipelineFixture, Descriptor, *journal.Record) {
	t.Helper()
	f := newPipeline(t, "approve")
	planJob(f, "report_comment", "reporter", "report")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(f.root, "source")
	r := &journal.Record{
		ID: d.ID, Epoch: f.Job.LeaseEpoch, Repo: d.Repo, Issue: d.Issue, Provider: d.Provider,
		Generation: d.Generation, Tree: tree, Head: f.git(tree, "rev-parse", "HEAD"),
		ExecutionPlan: d.View["executionPlan"], ExecutionPlanSHA: field[string](d.View, "executionPlanSHA"),
	}
	return f, d, r
}
