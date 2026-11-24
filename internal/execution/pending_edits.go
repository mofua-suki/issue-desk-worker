package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/cause"
	"strings"

	"issue-desk-worker/internal/journal"
)

type editRequests struct {
	DeleteFiles  []string `json:"deleteFiles"`
	Dependencies []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dev     bool   `json:"dev"`
	} `json:"addDependencies"`
}

func editRequestHash(raw json.RawMessage) (editRequests, string, error) {
	var requests editRequests
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return requests, "", cause.New("編集要求を読み取れません", err)
	}
	if err := json.Unmarshal(raw, &requests); err != nil {
		return requests, "", err
	}
	data, err := json.Marshal(requests)
	if err != nil {
		return requests, "", err
	}
	sum := sha256.Sum256(data)
	return requests, hex.EncodeToString(sum[:]), nil
}

func (e *Executor) noteAppliedRequests(r *journal.Record) error {
	// Temporary records used for manual requests have no journal ID, and
	// a JSON null report is an existing no-edit input for ordinary tests.
	var requests editRequests
	if err := json.Unmarshal(r.Result, &requests); err != nil {
		return err
	}
	if len(requests.DeleteFiles) == 0 && len(requests.Dependencies) == 0 {
		if r.AppliedEdits == nil {
			return nil
		}
		r.AppliedEdits = nil
		return e.Store.Save(r)
	}
	_, hash, err := editRequestHash(r.Result)
	if err != nil {
		return err
	}
	r.AppliedEdits = &journal.AppliedEdits{RequestHash: hash}
	if r.EditRequestHash == hash {
		r.AppliedEdits.CreatedAt = r.EditRequestedAt
		r.AppliedEdits.WorkerID = r.EditWorkerID
	}
	return e.Store.Save(r)
}

func pendingEditDetail(r *journal.Record) string {
	requests, hash, err := editRequestHash(r.Result)
	var details []string
	at, worker := "不明", "不明"
	if r.EditRequestHash == hash && !r.EditRequestedAt.IsZero() {
		at = r.EditRequestedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if r.EditRequestHash == hash && r.EditWorkerID != "" {
		worker = r.EditWorkerID
	}
	if r.AppliedEdits != nil && r.AppliedEdits.RequestHash == hash {
		if !r.AppliedEdits.CreatedAt.IsZero() {
			at = r.AppliedEdits.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		if r.AppliedEdits.WorkerID != "" {
			worker = r.AppliedEdits.WorkerID
		}
	}
	if err != nil {
		details = append(details, "種類=編集要求 対象=不明 状態=不確か")
	} else if (len(requests.DeleteFiles) > 0 || len(requests.Dependencies) > 0) && (r.AppliedEdits == nil || r.AppliedEdits.Head == "" || r.AppliedEdits.Head != r.Head || r.AppliedEdits.RequestHash != hash) {
		for _, name := range requests.DeleteFiles {
			details = append(details, fmt.Sprintf("種類=deleteFiles 対象=%q 状態=不確か", name))
		}
		for _, dep := range requests.Dependencies {
			details = append(details, fmt.Sprintf("種類=addDependencies 対象=%q 状態=不確か", dep.Name))
		}
	}
	if c := r.ReportCompletion; c != nil && c.Attempted && (!c.Validated || c.EditsStarted && !c.EditsApplied) {
		original, _, originalErr := editRequestHash(c.Original)
		count := 0
		if originalErr == nil {
			for _, name := range original.DeleteFiles {
				details = append(details, fmt.Sprintf("種類=報告補完 対象=%q 状態=不確か", name))
				count++
			}
			for _, dep := range original.Dependencies {
				details = append(details, fmt.Sprintf("種類=報告補完 対象=%q 状態=不確か", dep.Name))
				count++
			}
		}
		if count == 0 {
			details = append(details, "種類=報告補完 対象=不明 状態=不確か")
		}
	}
	if len(details) == 0 {
		return ""
	}
	for i := range details {
		details[i] += fmt.Sprintf(" 作成時刻=%s workerId=%s", at, worker)
	}
	return "未処理または不確かな編集要求があります\n" + strings.Join(details, "\n")
}
