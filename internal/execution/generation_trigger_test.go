package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// savedUsageRows returns the saved usage rows of one step, in order.
func savedUsageRows(t *testing.T, f *pipelineFixture, step string) []map[string]any {
	t.Helper()
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved == nil {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	var rows []map[string]any
	for _, raw := range saved.Usage {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row["step"] == step {
			rows = append(rows, row)
		}
	}
	return rows
}

// 差し戻しからの再実装は行の generation を進めないので、その世代のきっかけ（最初の世代は null）を
// 変えない。独立レビューの行には2項目を付けない。
func TestReviewReworkKeepsGenerationTrigger(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	reviews, implementations := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if err != nil {
			return answer, err
		}
		if !req.Review {
			implementations++
			if implementations > 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("revised\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return answer, nil
		}
		reviews++
		if reviews == 1 {
			var v map[string]any
			_ = json.Unmarshal(answer.JSON, &v)
			v["decision"] = "hold"
			v["findings"] = []string{"must fix", "also"}
			v["findingKinds"] = []string{"acceptance", "style"}
			v["findingSeverities"] = []string{"must", "must"}
			answer.JSON = encode(v)
		}
		return answer, nil
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	rows := savedUsageRows(t, f, "実装")
	if len(rows) != 2 {
		t.Fatalf("implementation rows=%v", rows)
	}
	if v, ok := rows[0]["generationTrigger"]; !ok || v != nil || rows[0]["reviewFindingKinds"] != nil {
		t.Fatalf("first generation row=%v", rows[0])
	}
	if v, ok := rows[1]["generationTrigger"]; !ok || v != nil || rows[1]["reviewFindingKinds"] != nil {
		t.Fatalf("rework row=%v", rows[1])
	}
	for _, row := range savedUsageRows(t, f, "独立レビュー") {
		if _, ok := row["generationTrigger"]; ok {
			t.Fatalf("review row=%v", row)
		}
		if _, ok := row["reviewFindingKinds"]; ok {
			t.Fatalf("review row=%v", row)
		}
	}
}

// 受付が進めた世代の実装の行は、前の世代の最後の報告と受付の指示からきっかけを持つ。
func TestNewGenerationUsageRowsCarryGenerationTrigger(t *testing.T) {
	for _, tc := range []struct {
		name         string
		previous     *intake.State
		review       json.RawMessage
		instructions []map[string]any
		result       json.RawMessage
		resumed      bool
		retest       map[string]any
		want         any
		kinds        any
	}{
		// 受付が再開と判定した世代は行の generation が進まないので、その generation で送ったきっかけ（最初の世代は null）を保つ。
		{name: "resumed generation", previous: &intake.State{Status: "held", HoldReason: "question"}, resumed: true, want: nil},
		{name: "other pc resumed generation", result: encode(map[string]any{"status": "failed"}), resumed: true, want: nil},
		{name: "review return", previous: &intake.State{Status: "held", HoldReason: "review_held"}, review: encode(map[string]any{"findingKinds": []string{"acceptance", "style"}}), instructions: []map[string]any{{"id": "review:1:1", "text": "指摘"}}, want: "review-return", kinds: []any{"acceptance"}},
		{name: "other pc review return", result: encode(map[string]any{"status": "held", "review": map[string]any{"findingKinds": []string{"security"}}}), instructions: []map[string]any{{"id": "review:1:1", "text": "指摘"}}, want: "review-return", kinds: []any{"security"}},
		{name: "review hold", previous: &intake.State{Status: "held", HoldReason: "review_held"}, want: "hold-instruction"},
		{name: "question", previous: &intake.State{Status: "held", HoldReason: "question"}, want: "answer"},
		{name: "failure", previous: &intake.State{Status: "failed"}, want: "retry"},
		// 受付の取り直し（テストの失敗・時間切れからの再開）は、前の世代が失敗でも retest にする。
		{name: "retest", previous: &intake.State{Status: "failed"}, retest: map[string]any{"generation": 1, "by": "token:test"}, want: "retest"},
		// 受付は前の世代の取り直しの印を view に残すので、この世代の印でなければ使わない。
		{name: "stale retest mark", previous: &intake.State{Status: "held", HoldReason: "review_held"}, review: encode(map[string]any{"findingKinds": []string{"data"}}), instructions: []map[string]any{{"id": "review:1:1", "text": "指摘"}}, retest: map[string]any{"generation": 0, "by": "token:test"}, want: "review-return", kinds: []any{"data"}},
		{name: "automatic retry", previous: &intake.State{Status: "held", HoldReason: "review_held"}, instructions: []map[string]any{{"id": "environment-retry:1", "text": "再試行"}}, want: "retry"},
		{name: "follow-up after completion", previous: &intake.State{Status: "merged"}, instructions: []map[string]any{{"id": "command:1", "text": "追加"}}, want: "unknown"},
		{name: "other pc failure", result: encode(map[string]any{"status": "failed"}), want: "retry"},
		{name: "other pc unknown", want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			// The previous generation published its work, as a real continuation does.
			w := continuationWorkspace(t, f)
			base := f.git(w.Tree, "rev-parse", "HEAD")
			head := continuationCommit(t, f, w.Tree, "prior.txt", "previous work\n")
			if err := w.Push(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "reset", "--hard", base)
			if tc.previous == nil {
				// The previous generation ran on another PC: this one has no journal for it.
				runtime := t.TempDir()
				store, err := journal.Open(runtime)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
			}
			d := continuationDescriptor(t, f, "")
			d.Generation = 1
			if tc.instructions != nil {
				d.View["instructions"] = encode(tc.instructions)
			}
			d.Result = tc.result
			if tc.retest != nil {
				d.View["retryRetest"] = encode(tc.retest)
			}
			f.Job.Raw = encode(d)
			if tc.resumed {
				d.View["implementationResumes"] = encode(1)
				f.Job.Raw = encode(d)
			}
			if tc.previous != nil {
				before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "held", FinalState: encode(*tc.previous), Review: tc.review}
				if tc.resumed {
					// The first generation already sent its rows as row generation 1.
					before.Runs, before.UsageTrigger = 1, &journal.GenerationTrigger{Generation: 1}
				}
				if err := f.Executor.Store.Save(before); err != nil {
					t.Fatal(err)
				}
			} else {
				d.HeadSHA = head
				if tc.resumed {
					// The first generation ran on another PC and handed its checkpoint over.
					d.View["workerExecution"] = encode(map[string]any{"generation": 0, "implementationRuns": 1, "startedGenerations": []int{0}, "usageTrigger": map[string]any{"generation": 1, "trigger": ""}})
				}
				f.Job.Raw = encode(d)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			rows := savedUsageRows(t, f, "実装")
			if len(rows) == 0 {
				t.Fatalf("no implementation rows: state=%s %s %s", state.Status, state.HoldReason, state.Result)
			}
			if rows[0]["generation"] != float64(map[bool]int{true: 1, false: 2}[tc.resumed]) || rows[0]["generationTrigger"] != tc.want {
				t.Fatalf("row=%v", rows[0])
			}
			if v, ok := rows[0]["reviewFindingKinds"]; !ok || !reflect.DeepEqual(v, tc.kinds) {
				t.Fatalf("row=%v", rows[0])
			}
		})
	}
}
