package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

type reviewNote struct{ kind, text, findingKind string }

func (e *Executor) sendReviewNotes(ctx context.Context, r *journal.Record, fence func(context.Context) error, merged bool) error {
	var review Review
	if err := json.Unmarshal(r.Review, &review); err != nil {
		return nil
	}
	var notes []reviewNote
	if !merged {
		for i, text := range review.Findings {
			kind := ""
			if i < len(review.Kinds) {
				kind = review.Kinds[i]
			}
			notes = append(notes, reviewNote{"finding", text, kind})
		}
	} else {
		for _, text := range review.Followups {
			notes = append(notes, reviewNote{"followup", text, ""})
		}
		for _, text := range review.Improvements {
			notes = append(notes, reviewNote{"improvement", text, ""})
		}
	}
	type pendingNote struct {
		key  string
		note reviewNote
	}
	var pending []pendingNote
	seen := map[string]bool{}
	for _, key := range r.NotesAttempted {
		seen[key] = true
	}
	for _, n := range notes {
		n.text = clip(e.redact(n.text), 2000)
		n.findingKind = clip(e.redact(n.findingKind), 200)
		if strings.TrimSpace(n.text) == "" {
			continue
		}
		keyData, _ := json.Marshal([]any{r.Repo, r.Issue, r.Head, n.kind, n.text})
		hash := sha256.Sum256(keyData)
		key := hex.EncodeToString(hash[:])
		if !seen[key] {
			pending = append(pending, pendingNote{key, n})
			seen[key] = true
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if err := fence(ctx); err != nil {
		return err
	}
	for _, item := range pending {
		r.NotesAttempted = append(r.NotesAttempted, item.key)
		if err := e.Store.Save(r); err != nil {
			r.NotesAttempted = r.NotesAttempted[:len(r.NotesAttempted)-1]
			fmt.Fprintf(os.Stderr, "仕事%dの注意点の記録に失敗: %v\n", r.ID, err)
			return nil
		}
		data := map[string]any{"text": item.note.text, "issue": r.Issue, "head": r.Head}
		if item.note.findingKind != "" {
			data["findingKind"] = item.note.findingKind
		}
		if err := e.API.Note(ctx, r.ID, r.Epoch, item.note.kind, data); err != nil {
			var apiErr *intake.APIError
			if errors.As(err, &apiErr) && apiErr.LeaseLost() {
				return err
			}
			fmt.Fprintf(os.Stderr, "仕事%dの注意点の送信に失敗: %v\n", r.ID, err)
		}
	}
	return nil
}

func (e *Executor) finishMerged(ctx context.Context, r *journal.Record, fence func(context.Context) error, summary string, view map[string]any) (intake.State, error) {
	if err := e.finishExecutionReports(ctx, r, fence); err != nil {
		return e.holdMergedExecution(r, view, err)
	}
	return e.finishConfirmedMerge(ctx, r, fence, summary, view)
}

func (e *Executor) finishConfirmedMerge(ctx context.Context, r *journal.Record, fence func(context.Context) error, summary string, view map[string]any) (intake.State, error) {
	r.ExecutionReportPending = false
	state, err := e.finish(r, "merged", "", summary, view)
	if err != nil {
		return state, err
	}
	if err := e.sendReviewNotes(ctx, r, fence, true); err != nil {
		fmt.Fprintf(os.Stderr, "仕事%dのマージ後の注意点送信に失敗: %v\n", r.ID, err)
	}
	return state, nil
}

func (e *Executor) holdMergedExecution(r *journal.Record, view map[string]any, err error) (intake.State, error) {
	if view == nil {
		view = map[string]any{}
	}
	view["executionReportError"] = e.redact(err.Error())
	view["executionWait"] = "reporter"
	view["codeMerged"] = true
	r.ExecutionReportPending = true
	return e.finish(r, "held", "review_held", "コードはマージ済み。報告工程が未完了です: "+e.redact(err.Error()), view)
}
