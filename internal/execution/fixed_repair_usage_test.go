package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/providers"
)

// The intake's implementation aggregates and Claude session-cost correction
// include 実装 but do not consistently include 固定テストの修正. Keep the
// existing step until the intake contract can include the latter everywhere.
func TestFixedRepairUsageKeepsImplementationStep(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	completionAPI(t, f)
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			calls++
			if calls == 2 && (!strings.Contains(req.Prompt, `"diagnostic":{"version":1`) || !strings.Contains(req.Prompt, `"reason":"test_failure"`) || !strings.Contains(req.Prompt, `"environment":`)) {
				t.Fatal("実際の固定テスト失敗の診断を修復呼出しへ渡していません")
			}
			if calls == 1 {
				if err := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); err != nil {
					t.Fatal(err)
				}
			}
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || calls != 2 {
		t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, raw := range saved.Usage {
		var row struct {
			Step string `json:"step"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, row.Step)
	}
	if len(steps) != 3 || steps[0] != "実装" || steps[1] != "実装" || steps[2] != "独立レビュー" {
		t.Fatalf("steps=%v", steps)
	}
}
