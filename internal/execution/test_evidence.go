package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/cause"
	"strings"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/testcause"
)

type testFenceError struct{ error }

// testHead accepts only a clean, committed tree. A dirty tree cannot be
// attributed to its parent even if rev-parse succeeds.
func (e *Executor) testHead(ctx context.Context, r *journal.Record) (string, error) {
	head, err := e.gitConfig().Local(ctx, r.Tree, 10*time.Second, "rev-parse", "HEAD")
	if err != nil || head.Code != 0 || !gitwork.ValidSHA(strings.TrimSpace(head.Out)) {
		return "", cause.New("対象headの40桁SHAを取得できません", err)
	}
	status, err := e.gitConfig().Local(ctx, r.Tree, 10*time.Second, "status", "--porcelain")
	if err != nil || status.Code != 0 || strings.TrimSpace(status.Out) != "" {
		return "", cause.New("作業ツリーが未確定か状態を取得できません", err)
	}
	return strings.TrimSpace(head.Out), nil
}

func (e *Executor) testTargetPreparer(ctx context.Context, w *gitwork.Workspace, r *journal.Record, title string, fence func(context.Context) error) func() error {
	return func() error {
		if err := fence(ctx); err != nil {
			return testFenceError{err}
		}
		if err := e.validatePending(ctx, w); err != nil {
			return err
		}
		if err := w.Stage(ctx); err != nil {
			return err
		}
		if err := fence(ctx); err != nil {
			return testFenceError{err}
		}
		head, err := w.Commit(ctx, title)
		if err != nil {
			return err
		}
		r.Head = head
		return nil
	}
}

// Revalidate the persisted evidence, not just the producer's Passed flag.
func testEvidenceReason(test TestResult, head string, base ...string) string {
	if !gitwork.ValidSHA(head) {
		return "対象headのSHAが不正です"
	}
	target, _ := testcause.Classify(test.Observation, nil)["target"].(testcause.Object)
	if test.Observation != nil && (target == nil || target["complete"] != true) {
		labels := map[string]string{"conditions": "実行条件", "completion": "完了件数", "target": "対象SHA", "base": "基点SHA", "output": "出力の完全性", "failures": "失敗出力", "source": "実行種別", "format": "保存形式の検証"}
		var names []string
		switch missing := test.Observation["passEvidenceMissing"].(type) {
		case []string:
			for _, code := range missing {
				if label := labels[code]; label != "" {
					names = append(names, label)
				}
			}
		case []any:
			for _, value := range missing {
				if code, ok := value.(string); ok {
					if label := labels[code]; label != "" {
						names = append(names, label)
					}
				}
			}
		}
		if len(names) > 0 {
			return "固定テストの合格証拠が不足しています: " + strings.Join(names, "、")
		}
	}
	if target == nil || target["source"] != "fixed-test" || target["sha"] != head || target["passed"] != true {
		return "対象headと一致する固定テスト観測がありません"
	}
	if target["complete"] != true {
		return "固定テストの完全な合格証拠を確認できません"
	}
	if len(base) > 0 && target["baseSha"] != base[0] {
		return "固定テストの基点SHAが一致しません"
	}
	return ""
}

func rejectTestEvidence(result *TestResult, reason string) {
	result.Passed, result.evidenceMissing, result.evidenceReason = false, true, true
	result.Detail = reason + "\n" + result.Detail
	if result.Observation != nil {
		result.Observation["passed"] = false
		result.Observation["complete"] = false
		result.Observation["passEvidence"] = nil
	}
}

func (e *Executor) reviewTestEvidence(ctx context.Context, r *journal.Record, head string) string {
	var test TestResult
	if json.Unmarshal(r.Test, &test) != nil || !test.Passed {
		return "レビュー対象の固定テスト合格証拠がありません"
	}
	actual, err := e.testHead(ctx, r)
	if err != nil || actual != head {
		return "レビュー前に固定テスト対象を照合できません"
	}
	return testEvidenceReason(test, head, r.Base)
}

func reusableTestEvidence(r *journal.Record) bool {
	var test TestResult
	if json.Unmarshal(r.Test, &test) != nil || !test.Passed {
		return false
	}
	target, _ := testcause.Classify(test.Observation, nil)["target"].(testcause.Object)
	return target != nil && target["complete"] == true
}
