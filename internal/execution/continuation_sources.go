package execution

import (
	"context"
	"errors"
	"strings"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// A retry instruction and its applied marker authorize only this generation's
// branch. A marker alone must never hide the ordinary continuation sources.
func explicitContinuationRetry(d Descriptor, r *journal.Record) bool {
	return retryBaseApplied(d, r) && retryBaseNeeded(d, &journal.Record{})
}

func continuationPRRequired(d Descriptor, r *journal.Record) bool {
	return r.PR > 0 || d.PR > 0 && !abandonedRetryDescriptor(d, r)
}

func (e *Executor) captureContinuationBranch(d Descriptor, r *journal.Record) error {
	if r.ContinuationBranch != nil {
		return nil
	}
	saved, err := e.Store.Load(d.ID)
	if err != nil {
		return err
	}
	branch := ""
	// A current-generation record created before implementation is not proof
	// that its generated branch contains a predecessor's work. Retain a branch
	// only from an earlier generation or an explicitly stopped journal.
	if saved != nil && (saved.Generation < d.Generation || saved.Stage == "stopped") {
		branch = saved.Branch
	}
	r.ContinuationBranch = &branch
	return nil
}

// Resolve local provenance before generation/count mutations, including when a
// new job has no head of its own. Branch-name digits are not identity evidence.
func (e *Executor) openRequiredContinuationWorkspace(ctx context.Context, d Descriptor, r *journal.Record, base string, fence func(context.Context) error) (*gitwork.Workspace, *intake.State, error) {
	restoreTargetDirtyStage(r)
	var sources []journal.Record
	if !explicitContinuationRetry(d, r) {
		records, err := e.Store.ListIssue(d.Repo, d.Issue)
		if err != nil {
			_ = e.captureContinuationBranch(d, r)
			seedContinuationEvidence(d, r)
			state, holdErr := e.holdContinuationHead(ctx, d, r, fence, "source_records_unavailable", continuationDiagnostic{source: "journal"})
			return nil, state, holdErr
		}
		sources = activeContinuationSources(d, r, records)
	}
	if !continuationHeadRequired(d, r) && len(sources) == 0 && !continuationPRRequired(d, r) {
		return nil, nil, nil
	}
	if err := e.captureContinuationBranch(d, r); err != nil {
		seedContinuationEvidence(d, r)
		state, holdErr := e.holdContinuationHead(ctx, d, r, fence, "source_records_unavailable", continuationDiagnostic{source: "journal"})
		return nil, state, holdErr
	}
	return e.openContinuationSources(ctx, d, r, base, fence, sources)
}

// Every referenced source must resolve. An optional missing remote branch is
// acceptable only if its recorded commit or PR still identifies the work.
func (e *Executor) continuationSourceHeads(ctx context.Context, d Descriptor, w *gitwork.Workspace, source journal.Record, diag *continuationDiagnostic, origins map[string]string) ([]string, string) {
	// A held job without provenance must not become evidence merely because the
	// next resume receives yet another job ID. A recorded head/PR can repair it.
	if source.ContinuationBranch != nil && *source.ContinuationBranch == "" && source.Head == "" && source.PR == 0 {
		return nil, "source_missing"
	}
	var heads []string
	sourceLabel := diag.source
	if source.Head != "" {
		if !gitwork.ValidSHA(source.Head) {
			return nil, "invalid_sha"
		}
		heads = append(heads, source.Head)
		origins[source.Head] = diag.source
	}
	if source.PR > 0 {
		diag.head = ""
		pull, err := e.Gitea.Pull(ctx, d.Repo, source.PR)
		if err != nil {
			return nil, "source_pr_unavailable"
		}
		if pull.Number != source.PR || pull.Head.Repo.FullName != d.Repo || pull.Base.Repo.FullName != d.Repo || pull.Base.Ref != w.Default ||
			!gitwork.ValidBranchShape(pull.Head.Ref) || source.Branch != "" && pull.Head.Ref != source.Branch {
			return nil, "source_pr_mismatch"
		}
		if !gitwork.ValidSHA(pull.Head.SHA) {
			return nil, "invalid_sha"
		}
		heads = append(heads, pull.Head.SHA)
		origins[pull.Head.SHA] = diag.source
	}
	if source.Branch != "" {
		if !gitwork.ValidBranchShape(source.Branch) || !strings.HasPrefix(source.Branch, "codex/") {
			return nil, "source_branch_invalid"
		}
		refs := []string{"refs/remotes/origin/" + source.Branch}
		// A different job's exact recorded branch can also retain unpublished
		// commits in this PC's mirror. Do not use a new current local branch.
		if source.ID != 0 && source.ID != d.ID {
			refs = append(refs, "refs/heads/"+source.Branch)
		}
		for _, ref := range refs {
			label := sourceLabel
			if strings.HasPrefix(ref, "refs/remotes/") {
				label = "remote"
			}
			*diag = continuationDiagnostic{source: label}
			out, err := w.Config.Run(ctx, w.Tree, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
			if err != nil || out.Code != 0 && out.Code != 1 {
				return nil, "source_branch_unavailable"
			}
			if out.Code == 0 {
				head := strings.TrimSpace(out.Out)
				if !gitwork.ValidSHA(head) {
					return nil, "invalid_sha"
				}
				heads = append(heads, head)
				origins[head] = label
			}
		}
	}
	if len(heads) == 0 {
		return nil, "source_missing"
	}
	return heads, ""
}

// Sources must form one ancestry chain. Neither journal timestamps nor numeric
// job/branch ordering prove which of two divergent artifacts supersedes the other.
func continuationSourceTip(ctx context.Context, w *gitwork.Workspace, heads []string, origins map[string]string, diag *continuationDiagnostic) (string, string) {
	tip := ""
	for _, head := range heads {
		*diag = continuationDiagnostic{origins[head], head}
		out, err := w.Config.Run(ctx, w.Tree, "cat-file", "-t", head)
		if err != nil || out.Code != 0 {
			return "", "commit_unavailable"
		}
		if strings.TrimSpace(out.Out) != "commit" {
			return "", "not_commit"
		}
		if tip == "" || tip == head {
			tip = head
			continue
		}
		forward, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", tip, head)
		if err != nil || forward.Code != 0 && forward.Code != 1 {
			return "", "comparison_unavailable"
		}
		if forward.Code == 0 {
			tip = head
			continue
		}
		backward, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", head, tip)
		if err != nil || backward.Code != 0 && backward.Code != 1 {
			return "", "comparison_unavailable"
		}
		if backward.Code != 0 {
			*diag = continuationDiagnostic{source: "unavailable"}
			return "", "source_ambiguous"
		}
	}
	return tip, ""
}

func (e *Executor) checkContinuationSources(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, sources []journal.Record, fence func(context.Context) error, diag *continuationDiagnostic) (string, bool, error) {
	var heads []string
	origins := map[string]string{}
	for _, source := range sources {
		*diag = continuationDiagnostic{"journal", source.Head}
		resolved, cause := e.continuationSourceHeads(ctx, d, w, source, diag, origins)
		if cause != "" {
			return cause, false, nil
		}
		heads = append(heads, resolved...)
	}
	// Intake's PR or a different recorded branch is independent of the current
	// branch. Do not substitute the current branch's mere existence for provenance.
	if !abandonedRetryDescriptor(d, r) {
		if d.PR > 0 || d.Branch != "" && d.Branch != r.Branch {
			*diag = continuationDiagnostic{"descriptor", d.HeadSHA}
			resolved, cause := e.continuationSourceHeads(ctx, d, w, journal.Record{Branch: d.Branch, Head: d.HeadSHA, PR: d.PR}, diag, origins)
			if cause != "" {
				return cause, false, nil
			}
			heads = append(heads, resolved...)
		}
	}
	if r.PR > 0 && (r.PR != d.PR || abandonedRetryDescriptor(d, r)) {
		*diag = continuationDiagnostic{"journal", r.Head}
		resolved, cause := e.continuationSourceHeads(ctx, d, w, journal.Record{Branch: r.Branch, Head: r.Head, PR: r.PR}, diag, origins)
		if cause != "" {
			return cause, false, nil
		}
		heads = append(heads, resolved...)
	}
	if len(heads) == 0 {
		if d.HeadSHA != "" || r.Head != "" || explicitContinuationRetry(d, r) {
			return "", false, nil
		}
		// Only a pre-existing journal can make this current branch a prior source.
		if r.ContinuationBranch == nil || *r.ContinuationBranch == "" {
			return "source_missing", false, nil
		}
		*diag = continuationDiagnostic{source: "journal"}
		resolved, cause := e.continuationSourceHeads(ctx, d, w, journal.Record{Branch: *r.ContinuationBranch}, diag, origins)
		if cause == "source_missing" {
			cause = "commit_unavailable"
		}
		if cause != "" {
			return cause, false, nil
		}
		heads = append(heads, resolved...)
	}
	*diag = continuationDiagnostic{source: "unavailable"}
	tip, cause := continuationSourceTip(ctx, w, heads, origins, diag)
	if cause != "" {
		return cause, false, nil
	}
	*diag = continuationDiagnostic{origins[tip], tip}
	if cause = continuationHeadCause(ctx, w, tip); cause != "not_included" {
		return cause, false, nil
	}
	// Follow only a proven source, without resetting or merging divergent work.
	// All delivered/local requirements must also be included in that source.
	for _, item := range []continuationDiagnostic{{"descriptor", d.HeadSHA}, {"journal", r.Head}} {
		head := item.head
		if head == "" {
			continue
		}
		if !gitwork.ValidSHA(head) {
			*diag = item
			return "invalid_sha", false, nil
		}
		out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", head, tip)
		if err != nil || out.Code != 0 {
			*diag = item
			return "not_included", false, nil
		}
	}
	if err := w.CheckTargetClean(ctx); err != nil {
		if errors.Is(err, gitwork.ErrWorkspaceUncommitted) {
			return "target_dirty", false, nil
		}
		return "status_unavailable", false, nil
	}
	out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", "HEAD", tip)
	if err != nil || out.Code != 0 && out.Code != 1 {
		return "comparison_unavailable", false, nil
	}
	if out.Code != 0 {
		return "not_included", false, nil
	}
	if err := fence(ctx); err != nil {
		return "", false, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	// Persist invalidation before changing HEAD, so a crash cannot reuse approval.
	keepPreviousResults(r)
	clearFollowedResults(r)
	if r.Stage == "review" || r.Stage == "publish" || r.Stage == "merge" {
		r.Stage = "test"
	}
	if err := e.Store.Save(r); err != nil {
		return "", false, err
	}
	out, err = w.Config.Run(ctx, w.Tree, "merge", "--ff-only", tip)
	if err != nil || out.Code != 0 {
		return "source_follow_unavailable", false, nil
	}
	return continuationHeadCause(ctx, w, tip), true, nil
}
