package gitwork

import (
	"context"
	"errors"
	"issue-desk-worker/internal/cause"
	"path"
	"strings"
)

// SameFileContents compares the complete set supplied by the caller. Both
// revisions must contain every path as a regular file with the same mode and
// content; the approved commit must be an ancestor of head. Symbolic links and
// other entry kinds never match. It does not determine which paths require
// approval.
func (w *Workspace) SameFileContents(ctx context.Context, approved, head string, files []string) (bool, error) {
	if !sha.MatchString(approved) || !sha.MatchString(head) || files == nil {
		return false, errors.New("承認対象を確認できません")
	}
	out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", approved, head)
	if err != nil {
		return false, err
	}
	if out.Code == 1 {
		return false, nil
	}
	if out.Code != 0 {
		return false, errors.New("祖先関係を確認できません")
	}
	for _, file := range files {
		if file == "" || path.IsAbs(file) || path.Clean(file) != file || file == ".." || strings.HasPrefix(file, "../") || strings.ContainsRune(file, '\x00') {
			return false, errors.New("承認対象パスが不正です")
		}
		a, aok, err := w.approvalBlob(ctx, approved, file)
		if err != nil {
			return false, err
		}
		b, bok, err := w.approvalBlob(ctx, head, file)
		if err != nil {
			return false, err
		}
		if !aok || !bok || a != b {
			return false, nil
		}
	}
	return true, nil
}

// approvalBlob returns the mode and blob of a regular file as one identity.
func (w *Workspace) approvalBlob(ctx context.Context, commit, file string) (string, bool, error) {
	out, err := w.Config.Run(ctx, w.Tree, "ls-tree", "-z", "--full-tree", commit, "--", ":(literal)"+file)
	if err != nil {
		return "", false, err
	}
	if out.Code != 0 {
		return "", false, errors.New("承認対象を読めません")
	}
	if out.Out == "" {
		return "", false, nil
	}
	entry := strings.SplitN(strings.TrimSuffix(out.Out, "\x00"), "\t", 2)
	if len(entry) != 2 || entry[1] != file {
		return "", false, errors.New("承認対象を確認できません")
	}
	fields := strings.Fields(entry[0])
	if len(fields) != 3 || fields[1] != "blob" || !sha.MatchString(fields[2]) || fields[0] != "100644" && fields[0] != "100755" {
		return "", false, nil
	}
	return fields[0] + " " + fields[2], true, nil
}

// DeletionApprovalEvidence is evidence already bound to an approval by its
// source. Base and Diff must be the values recorded for that approval, never
// the current PR base or a newly computed merge base. Paths is the complete
// set represented by Diff. A nil Paths means the evidence is incomplete.
type DeletionApprovalEvidence struct {
	PR               int64
	Head, Base, Diff string
	Paths            []string
}

// DeletionEvidenceReader reads existing approval-bound evidence. Implementations
// must return an error if they cannot establish that association.
type DeletionEvidenceReader interface {
	ReadDeletionApproval(context.Context, string) (DeletionApprovalEvidence, error)
}

// ReadDeletionApprovalSnapshot rejects missing, inconsistent or unreadable
// evidence. The raw diff is retained without line normalization or deduplication
// so a later SQL comparison can distinguish paths, multiline SQL and counts.
func (w *Workspace) ReadDeletionApprovalSnapshot(ctx context.Context, reader DeletionEvidenceReader, approvedHead string, pr int64) (DeletionApprovalEvidence, error) {
	var zero DeletionApprovalEvidence
	if reader == nil || !sha.MatchString(approvedHead) || pr <= 0 {
		return zero, errors.New("削除承認の証拠を確認できません")
	}
	e, err := reader.ReadDeletionApproval(ctx, approvedHead)
	if err != nil {
		return zero, err
	}
	if e.PR != pr || e.Head != approvedHead || !sha.MatchString(e.Base) || e.Paths == nil || len(e.Paths) == 0 || e.Diff == "" {
		return zero, errors.New("削除承認の証拠が不完全または不一致です")
	}
	seen := make(map[string]bool, len(e.Paths))
	for _, p := range e.Paths {
		if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsRune(p, '\x00') || seen[p] {
			return zero, errors.New("削除承認の対象が不正です")
		}
		seen[p] = true
	}
	// Re-read exactly the recorded endpoints, and require the evidence to cover
	// the entire diff. A current branch or merge-base is never consulted.
	names, err := w.Config.Run(ctx, w.Tree, "diff", "--name-only", "-z", "--no-renames", e.Base, e.Head, "--")
	if err != nil || names.Code != 0 || !strings.HasSuffix(names.Out, "\x00") {
		return zero, cause.New("削除承認の対象差分を読めません", err)
	}
	actual := strings.Split(strings.TrimSuffix(names.Out, "\x00"), "\x00")
	if len(actual) != len(seen) {
		return zero, errors.New("削除承認の対象集合が不一致です")
	}
	for _, p := range actual {
		if !seen[p] {
			return zero, errors.New("削除承認の対象集合が不一致です")
		}
	}
	// Force textual hunks even for files marked binary, including by attributes.
	// An abbreviated "Binary files ... differ" is not sufficient evidence.
	diff, err := w.Config.Run(ctx, w.Tree, "diff", "--text", "--no-ext-diff", "--no-textconv", "--no-renames", e.Base, e.Head, "--")
	if err != nil || diff.Code != 0 || diff.Out != e.Diff {
		return zero, cause.New("削除承認の差分を検証できません", err)
	}
	e.Paths = append([]string(nil), e.Paths...)
	return e, nil
}
