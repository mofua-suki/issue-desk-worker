package execution

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/journal"
	"strings"
)

// 使用量の重みと上限、欠測の扱いはServerの保存済み行と照合して判定する。
func (e *Executor) checkImplementationUsage(ctx context.Context, d Descriptor, r *journal.Record, automatic, recall bool) error {
	expected := field[struct {
		Generation int    `json:"generation"`
		Admitted   bool   `json:"usageAdmission"`
		Used       string `json:"usageAdmissionUsed"`
	}](d.View, "reimplementExpected")
	for _, pending := range field[[]struct {
		ID string `json:"id"`
	}](d.View, "pending") {
		for _, prefix := range []string{"review:", "provider-switch:", "environment-retry:", "permission-retry:", "usage-limit-retry:", "conflict-from-base:"} {
			if strings.HasPrefix(pending.ID, prefix) {
				automatic = true
			}
		}
	}
	admitted := expected.Admitted && expected.Generation == d.Generation
	if !automatic && !admitted {
		return nil
	}
	rows := r.Usage
	if rows == nil {
		rows = []json.RawMessage{}
	}
	data := map[string]any{"rows": rows, "skipped": 0}
	if len(encode(data)) > 100000 {
		data = map[string]any{"rows": []any{}, "skipped": 1}
	}
	// The admission belongs to the run about to start. Only an unstarted generation
	// (not automatic) or a recall uses it, so Runs+1 and Runs name the same run.
	run := r.Runs + 1
	if recall {
		run = r.Runs
	}
	same := r.AdmissionID != "" && r.AdmissionGeneration == d.Generation && r.AdmissionRun == run
	if admitted && ((!automatic) || (recall && same && !r.AdmissionRecall)) {
		if !same {
			id := expected.Used
			if id == "" {
				b := make([]byte, 16)
				if _, err := rand.Read(b); err != nil {
					return err
				}
				b[6] = (b[6] & 15) | 64
				b[8] = (b[8] & 63) | 128
				id = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
			}
			r.AdmissionID = id
			r.AdmissionGeneration = d.Generation
			r.AdmissionRun = run
			r.AdmissionRecall = false
			if err := e.Store.Save(r); err != nil {
				return err
			}
		}
		data["admissionId"] = r.AdmissionID
		if recall {
			data["admissionRecall"] = true
		}
	}
	var result struct {
		Job    json.RawMessage `json:"job"`
		Reason json.RawMessage `json:"usageLimitReason"`
	}
	if err := e.API.Call(ctx, "POST", fmt.Sprintf("/api/v1/jobs/%d/events", d.ID), map[string]any{"epoch": r.Epoch, "kind": "implementation_usage_check", "data": data}, &result); err != nil {
		return cause.New("実装の使用量をServerで確認できませんでした", err)
	}
	if len(result.Job) == 0 || string(result.Job) == "null" {
		return errors.New("実装使用量の確認応答が不正です")
	}
	var checked Descriptor
	if json.Unmarshal(result.Job, &checked) != nil || checked.ID != d.ID {
		return errors.New("実装使用量の確認対象が一致しません")
	}
	if len(result.Reason) == 0 {
		return errors.New("実装使用量の確認応答に判定がありません")
	}
	if string(result.Reason) != "null" {
		var reason string
		if json.Unmarshal(result.Reason, &reason) != nil || strings.TrimSpace(reason) == "" {
			return errors.New("実装使用量の確認応答が不正です")
		}
		return errors.New("実装の使用量を理由に停止しました: " + reason)
	}
	// The server may have learned of additional saved usage since the job was
	// acquired. Preserve the largest known total for subsequent local reserves.
	serverUsage := field[float64](checked.View, "implementationWeightedUsage")
	checkpoint := field[struct {
		WeightedUsage float64 `json:"weightedUsage"`
	}](checked.View, "workerExecution")
	if latest := max(serverUsage, checkpoint.WeightedUsage); latest > r.WeightedUsage {
		r.WeightedUsage = latest
		if err := e.Store.Save(r); err != nil {
			return err
		}
	}
	if recall && data["admissionRecall"] == true {
		r.AdmissionRecall = true
		return e.Store.Save(r)
	}
	return nil
}
