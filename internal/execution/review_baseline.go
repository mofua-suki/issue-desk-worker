package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

var reviewHunkHeader = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+[0-9]+(?:,[0-9]+)? @@`)

func digestReview(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

func digestReviewJSON(raw json.RawMessage) string {
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return ""
	}
	return digestReview(compact.String())
}

// Preserve hunk boundaries, section labels, context, filenames, modes and changed bytes.
// Binary and unusual patches fall back to full review.
// Line numbers are deliberately ignored: identical changes with identical context
// in different positions have the same identity and can skip another review.
func reviewDiffHash(diff string) (string, bool) {
	var b strings.Builder
	inFile, inHunk := false, false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inFile, inHunk = true, false
			b.WriteString(line)
		case !inFile:
			return "", false
		case strings.HasPrefix(line, "index "): // blob IDs change with context
		case strings.HasPrefix(line, "@@ "):
			header := reviewHunkHeader.FindString(line)
			if header == "" {
				return "", false
			}
			inHunk = true
			b.WriteString("@@ @@")
			b.WriteString(strings.TrimPrefix(line, header))
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"), strings.HasPrefix(line, "@@@ "):
			return "", false
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, "\\")):
			b.WriteString(line)
		case inHunk && strings.HasPrefix(line, " "):
			b.WriteString(line)
		case !inHunk && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") || strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ")):
			b.WriteString(line)
		default:
			return "", false
		}
	}
	return digestReview(b.String()), true
}

func reviewBaselinePath(root string, id int64) string {
	return filepath.Join(root, "workspaces", ".review", fmt.Sprint(id), "baseline.patch")
}

func loadReviewBaseline(root string, r *journal.Record) ([]byte, bool) {
	b := r.ReviewBaseline
	if b == nil || b.Hash == "" || !gitwork.ValidSHA(b.Head) || (b.Decision != "approved" && b.Decision != "held") {
		return nil, false
	}
	var review Review
	if json.Unmarshal(b.Review, &review) != nil || (review.Decision != "approve" && review.Decision != "hold") || digestReviewJSON(b.Review) != b.ReviewHash || b.ReviewHash == "" || (b.Decision == "approved") != review.Approved() {
		return nil, false
	}
	data, err := os.ReadFile(reviewBaselinePath(root, r.ID))
	if err != nil || digestReview(string(data)) != b.PatchHash {
		return nil, false
	}
	hash, ok := reviewDiffHash(string(data))
	return data, ok && hash == b.Hash
}

func reviewRequestHash(d Descriptor, title, body string) string {
	// Preserve the request identity of existing jobs without a plan.
	input := []any{title, body, instructionData(d)}
	if plan, _ := parseExecutionPlan(d); plan != nil {
		input = append(input, field[string](d.View, "executionPlanSHA"), plan)
	}
	return digestReview(string(encode(input)))
}

func reviewBaselineMatches(r *journal.Record, d Descriptor, requestHash string) bool {
	return r.ReviewBaseline != nil && r.ReviewBaseline.Generation == d.Generation && r.ReviewBaseline.RequestHash != "" && r.ReviewBaseline.RequestHash == requestHash
}

func (e *Executor) saveReviewBaseline(r *journal.Record, s gitwork.Snapshot, review Review, raw json.RawMessage, kind, requestHash string) error {
	r.CarriedHeldReview = nil
	hash, ok := reviewDiffHash(s.Diff)
	if ok {
		path := reviewBaselinePath(e.Config.RuntimeDir, r.ID)
		if err := atomicfile.Write(path, ".baseline-*", 0600, true, []byte(s.Diff), nil); err != nil {
			return err
		}
	}
	r.ReviewBaseline = nil
	if ok || hasJSON(r.ExecutionPlan) {
		decision := "held"
		if review.Approved() {
			decision = "approved"
		}
		r.ReviewBaseline = &journal.ReviewBaseline{RequestHash: requestHash, Hash: hash, ReviewHash: digestReviewJSON(raw), Decision: decision, Head: s.Head, Generation: r.Generation, Review: append(json.RawMessage(nil), raw...)}
		if ok {
			r.ReviewBaseline.PatchHash = digestReview(s.Diff)
		}
	}
	r.ReviewSkipped = false
	r.AddReviewRound(journal.ReviewRound{Kind: kind, Base: s.Base, Head: s.Head})
	return e.Store.Save(r)
}

// prepareReview provides both the full PR patch and, when reliable, the patch
// between review snapshots. A failure to identify the change uses the full patch.
func (e *Executor) prepareReview(ctx context.Context, r *journal.Record, w *gitwork.Workspace, s gitwork.Snapshot, full string, d Descriptor, requestHash string) (kind, promptExtra string, files []string) {
	files = []string{full}
	kind = "full"
	if recovery := r.ReviewUsageRecovery; recovery != nil && (recovery.Head != s.Head || recovery.Base != s.Base || recovery.PatchHash != digestReview(s.Diff)) {
		return
	}
	_, valid := loadReviewBaseline(e.Config.RuntimeDir, r)
	hash, ok := reviewDiffHash(s.Diff)
	if !valid || !ok || !reviewBaselineMatches(r, d, requestHash) {
		return
	}
	b := r.ReviewBaseline
	if b.Decision == "approved" && b.Hash == hash {
		return "skipped", "", files
	}
	if b.Decision == "held" && b.Hash == hash {
		return "held", "", files
	}
	delta := full + ".delta"
	out, err := w.Config.Run(ctx, w.Tree, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--", reviewBaselinePath(e.Config.RuntimeDir, r.ID), full)
	if err != nil || (out.Code != 0 && out.Code != 1) {
		return
	}
	if err := os.WriteFile(delta, []byte(out.Out), 0600); err != nil {
		return
	}
	files = append(files, delta)
	promptExtra = "\n新たな差分を主なレビュー対象とし、前回の指摘があればその解消も確認してください。新たな差分（前回と今回のPR差分の比較）: " + delta + "\n全差分（参照用）: " + full
	return "delta", promptExtra, files
}
