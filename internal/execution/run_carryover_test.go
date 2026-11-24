package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

const carriedFinding = "指摘本文の目印: テストの不足"

// holdingReview は一度だけ指摘付きで保留にするレビューへ差し替え、実装のプロンプトを集める。
func holdingReview(f *pipelineFixture) *[]string {
	base := f.Executor.Call
	prompts := &[]string{}
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		*prompts = append(*prompts, req.Prompt)
		answer, err := base(ctx, req)
		if req.Review {
			answer.JSON = encode(map[string]any{"decision": "hold", "summary": "要約だけ", "breaking": true, "tooLarge": false, "findings": []string{carriedFinding}, "findingKinds": []string{"acceptance"}, "findingSeverities": []string{"minor"}, "findingSpecs": []any{}, "followups": []string{}, "followupSpecs": []any{}, "improvements": []string{}, "postMergeChecks": []string{}, "userJudgments": []string{}, "resolvableUncertainties": []string{}, "repeatedNotes": []string{}})
		}
		return answer, err
	}
	return prompts
}

func nextGeneration(t *testing.T, f *pipelineFixture, state intake.State) {
	t.Helper()
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(state.View, &d.View); err != nil {
		t.Fatal(err)
	}
	d.Generation++
	d.Result = state.Result
	d.HeadSHA, d.Branch = state.HeadSHA, state.Branch
	if state.PR != nil {
		d.PR = *state.PR
	}
	f.Job.Raw, f.CurrentView = encode(d), d.View
}

// 共通番号4565の4回目: 世代が上がると前回のレビューが消え、指摘の中身が分からず保留になった。
func TestNextGenerationPromptsCarryPreviousReview(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "same-pc"
		if moved {
			name = "moved-pc"
		}
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prompts := holdingReview(f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.HoldReason != "review_held" {
				t.Fatal(state, err)
			}
			nextGeneration(t, f, state)
			if moved {
				if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
					t.Fatal(err)
				}
			}
			*prompts = nil
			if _, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if len(*prompts) < 2 {
				t.Fatalf("実装とレビューが呼ばれていません: %d", len(*prompts))
			}
			for i, prompt := range (*prompts)[:2] {
				if !strings.Contains(prompt, carriedFinding) {
					t.Fatalf("呼び出し%dへ前回の指摘が渡っていません", i)
				}
			}
			if !strings.Contains((*prompts)[1], `"previousImplementation":{`) {
				t.Fatal("レビューへ前回の実装の報告が渡っていません")
			}
		})
	}
}

// 実装が質問で止まりレビューが無い回の結果にも前回の指摘を残し、次の別PCへ届ける。
func TestHeldRunWithoutReviewKeepsPreviousReviewInResult(t *testing.T) {
	f := newPipeline(t, "approve")
	holdingReview(f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.HoldReason != "review_held" {
		t.Fatal(state, err)
	}
	nextGeneration(t, f, state)
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		answer.JSON = reportWith("status", "question")
		return answer, err
	}
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.HoldReason != "question" {
		t.Fatal(state, err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result["previousReview"]), carriedFinding) {
		t.Fatalf("結果に前回のレビューがありません: %s", state.Result)
	}
	if _, ok := result["review"]; ok {
		t.Fatal("今回のレビューと前回のレビューを混ぜてはいけません")
	}
}

func TestSeedPreviousResultsIgnoresOmittedCopies(t *testing.T) {
	if usableCarried(json.RawMessage(`{"omitted":true,"reason":"x"}`)) || usableCarried(json.RawMessage(`null`)) || !usableCarried(json.RawMessage(`{"decision":"hold"}`)) {
		t.Fatal("省略された写しを前回のレビューとして渡してはいけません")
	}
}

// interruptingCall は最初の times 回の実装を、答えを返さずに中断させる。
func interruptingCall(f *pipelineFixture, cancel context.CancelFunc, times *int) {
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review && *times > 0 {
			*times--
			cancel()
			return providers.Answer{Outcome: "aborted"}, context.Canceled
		}
		return base(ctx, req)
	}
}

func interruptOnce(t *testing.T, f *pipelineFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	times := 1
	base := f.Executor.Call
	interruptingCall(f, cancel, &times)
	defer func() { f.Executor.Call = base }()
	if _, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("中断のはずです: %v", err)
	}
}

// 2026-11-03 の中断9件: Workerの再起動などで答えが返らなかった開始が実装回数を消費していた。
func TestInterruptedRunDoesNotConsumeRunBudget(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	interruptOnce(t, f)
	if saved, err := f.Executor.Store.Load(1); err != nil || saved.Runs != 1 || saved.OpenRun != 1 {
		t.Fatal("開始は記録されるはずです", saved, err)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatal("中断した開始で上限に達してはいけません", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != 1 || saved.InterruptedRuns != 1 || saved.OpenRun != 0 {
		t.Fatal(saved, err)
	}
}

// 別のPCへ移ったときは journal が無いので、受付の view の印から判定する。
func TestInterruptedRunOnAnotherPCUsesViewMarker(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.HeadSHA = publishContinuationFixture(t, f)
	d.View["workerExecution"] = json.RawMessage(`{"stage":"implement","implementationRuns":1,"openRun":1,"openRunGeneration":0}`)
	f.Job.Raw, f.CurrentView = encode(d), d.View
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatal(state, err)
	}
}

// 印の無い旧形式、別の世代の印、答えが返った回は中断として取り戻さないが、同じ世代なので
// 上限では止めない。開始済みの世代のやり直しとして使用量の確認を通す。
func TestStartedGenerationWithoutInterruptionMarkerChecksUsageInsteadOfLimit(t *testing.T) {
	for name, view := range map[string]string{
		"old-checkpoint":   `{"stage":"implement","implementationRuns":1}`,
		"other-generation": `{"stage":"implement","implementationRuns":1,"openRun":1,"openRunGeneration":7}`,
		"other-run":        `{"stage":"implement","implementationRuns":1,"openRun":9,"openRunGeneration":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			tr := completionAPI(t, f)
			f.Executor.Config.Assignments.ImplementationRunLimit = 1
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			d.HeadSHA = publishContinuationFixture(t, f)
			d.View["workerExecution"] = json.RawMessage(view)
			f.Job.Raw, f.CurrentView = encode(d), d.View
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || len(tr.checks) != 1 {
				t.Fatal("使用量を確かめて続くはずです", state, err, tr.checks)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved.Runs != 1 || saved.InterruptedRuns != 0 {
				t.Fatal(saved, err)
			}
		})
	}
}

// 取り戻しの上限を使い切った後の、答えが返らなかった開始は上限へ数える。
func TestUnansweredStartBeyondAllowanceStopsAtRunLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.HeadSHA = publishContinuationFixture(t, f)
	d.View["workerExecution"] = json.RawMessage(`{"stage":"implement","implementationRuns":1,"openRun":1,"openRunGeneration":0,"interruptedRuns":2}`)
	f.Job.Raw, f.CurrentView = encode(d), d.View
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) || len(f.Calls) != 0 {
		t.Fatal("上限で止まるはずです", state, err, f.Calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.UnansweredRuns != 1 || saved.Runs != 2 {
		t.Fatal(saved, err)
	}
}

// 中断を続ける仕事は取り戻しの上限を超えると従来どおり上限で止まる。
func TestRepeatedInterruptionsStillReachRunLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	for i := 0; i <= maxInterruptedRuns; i++ {
		interruptOnce(t, f)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) {
		t.Fatal(state, err)
	}
}

// 答えが返った回は中断ではないので数える。
func TestAnsweredRunStillCounts(t *testing.T) {
	f := newPipeline(t, "approve")
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		answer.JSON = reportWith("status", "question")
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.HoldReason != "question" {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != 1 || saved.OpenRun != 0 || saved.InterruptedRuns != 0 {
		t.Fatal(saved, err)
	}
	if strings.Contains(string(state.View), "openRun") {
		t.Fatalf("答えが返った回に印を残してはいけません: %s", state.View)
	}
}

func TestLegacyReviewCarriedAcrossPC(t *testing.T) {
	legacy := json.RawMessage(`{"decision":"hold","uncertainties":["旧判断"]}`)
	r := &journal.Record{}
	seedPreviousResults(r, Descriptor{Descriptor: jobspec.Descriptor{Result: encode(map[string]any{"review": legacy})}})
	var review Review
	if err := json.Unmarshal(r.PreviousReview, &review); err != nil || review.Approved() || len(review.Uncertainties) != 1 || review.Uncertainties[0] != "旧判断" {
		t.Fatal(string(r.PreviousReview), err)
	}
}
