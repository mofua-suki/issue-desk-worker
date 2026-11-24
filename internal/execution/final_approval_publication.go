package execution

import (
	"context"
	"encoding/json"
	"strings"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

const finalApprovalWarning = "headを変えると最終承認が未了に戻ります"

func (e *Executor) holdFinalPublication(r *journal.Record, reason string) (intake.State, error) {
	return e.finish(r, "held", "review_held", reason+"。"+finalApprovalWarning, map[string]any{"finalApprovalHold": true, "releasePending": true})
}

func finalPublicationHold(reason string) error {
	return &releaseHold{"review_held", reason + "。" + finalApprovalWarning, map[string]any{"finalApprovalHold": true, "releasePending": true}}
}

// Verify the complete target before any operation that can move the branch.
func (e *Executor) openFinalPublication(ctx context.Context, d Descriptor, p finalApprovalPublication, r *journal.Record, base string, fence func(context.Context) error) (*gitwork.Workspace, *intake.State, error) {
	hold := func(reason string) (*gitwork.Workspace, *intake.State, error) {
		if err := fence(ctx); err != nil {
			return nil, nil, err
		}
		s, err := e.holdFinalPublication(r, reason)
		return nil, &s, err
	}
	if r.Branch != d.Branch || r.PR != d.PR {
		return hold("保存された公開対象が配布対象と一致しません")
	}
	w, err := gitwork.Open(ctx, e.gitConfig(), d.Repo, base, d.Branch, d.ID)
	if err != nil {
		return hold("公開済み作業ツリーを取得できません")
	}
	r.Tree = w.Tree
	if r.Head != "" && r.Head != d.HeadSHA && !finalPublicationAncestor(ctx, w, r.Head, d.HeadSHA) {
		return hold("保存された公開対象が配布対象と一致しません")
	}
	if cause := e.checkFinalApprovalPublication(ctx, d, p, w); cause != "" {
		return hold("公開済み成果物を照合できません: " + cause)
	}
	actual, err := w.Head(ctx)
	if err != nil {
		return hold("作業ツリーのheadを確認できません")
	}
	if actual != p.HeadSHA {
		if w.CheckTargetClean(ctx) != nil || !finalPublicationAncestor(ctx, w, actual, p.HeadSHA) {
			return hold("作業ツリーを承認対象headへ安全に追従できません")
		}
		if err := fence(ctx); err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if w.CheckTargetClean(ctx) != nil {
			return hold("作業ツリーに未確定変更または競合があります")
		}
		// Pin the destination to the approved commit, never a moving branch or
		// a new merge of the default branch. Keep the journal head unchanged so
		// execute still selects reports before updating their target head.
		out, err := w.Config.Run(ctx, w.Tree, "merge", "--ff-only", p.HeadSHA)
		if err != nil || out.Code != 0 {
			return hold("作業ツリーを承認対象headへ追従できません")
		}
		// Re-run Open's forbidden-file checks for the followed tree as well.
		w, err = gitwork.Open(ctx, w.Config, d.Repo, base, d.Branch, d.ID)
		if err != nil {
			return hold("追従後の作業ツリーを取得できません")
		}
		if cause := e.checkFinalApprovalPublication(ctx, d, p, w); cause != "" {
			return hold("公開済み成果物を照合できません: " + cause)
		}
	}
	if reason := finalPublicationTreeCause(ctx, w, p.HeadSHA); reason != "" {
		return hold(reason)
	}
	// Resolve every existing continuation source without following any branch.
	// Sources are records of this Issue only, so read just those.
	records, err := e.Store.ListIssue(d.Repo, d.Issue)
	if err != nil {
		return hold("継続元の保存記録を確認できません")
	}
	heads := []string{p.HeadSHA}
	origins := map[string]string{p.HeadSHA: "descriptor"}
	diag := continuationDiagnostic{}
	for _, source := range activeContinuationSources(d, r, records) {
		diag = continuationDiagnostic{source: "journal", head: source.Head}
		found, cause := e.continuationSourceHeads(ctx, d, w, source, &diag, origins)
		if cause != "" {
			return hold("継続元を照合できません: " + cause)
		}
		heads = append(heads, found...)
	}
	tip, cause := continuationSourceTip(ctx, w, heads, origins, &diag)
	if cause != "" {
		return hold("継続元を照合できません: " + cause)
	}
	if cause := continuationHeadCause(ctx, w, tip); cause != "" {
		return hold("公開対象が継続元を含みません: " + cause)
	}
	return w, nil, nil
}

// Ancestry is usable only for full SHAs identifying actual commit objects.
func finalPublicationAncestor(ctx context.Context, w *gitwork.Workspace, saved, approved string) bool {
	for _, head := range []string{saved, approved} {
		if !gitwork.ValidSHA(head) {
			return false
		}
		out, err := w.Config.Run(ctx, w.Tree, "cat-file", "-t", head)
		if err != nil || out.Code != 0 || strings.TrimSpace(out.Out) != "commit" {
			return false
		}
	}
	out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", saved, approved)
	return err == nil && out.Code == 0
}

func finalPublicationTreeCause(ctx context.Context, w *gitwork.Workspace, head string) string {
	actual, err := w.Head(ctx)
	if err != nil || actual != head {
		return "作業ツリーのheadが承認対象と一致しません"
	}
	if !w.Clean(ctx) {
		return "作業ツリーに未確定変更または競合があります"
	}
	base, err := w.Base(ctx)
	if err != nil {
		return "既定ブランチの基点を確認できません"
	}
	out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", base, head)
	if err != nil || out.Code != 0 {
		return "既定ブランチへの追従が必要または確認不能であり、同じheadでマージできません"
	}
	return ""
}

func (e *Executor) checkPublishedTarget(ctx context.Context, d Descriptor, w *gitwork.Workspace) error {
	p, ok := selectFinalApprovalPublication(d)
	if !ok {
		return finalPublicationHold("公開続行情報を確認できません")
	}
	if err := w.Fetch(ctx); err != nil {
		return finalPublicationHold("公開済み枝を取得できません: " + clip(e.redact(err.Error()), 300))
	}
	if cause := e.checkFinalApprovalPublication(ctx, d, p, w); cause != "" {
		return finalPublicationHold("公開済み成果物を照合できません: " + cause)
	}
	if reason := finalPublicationTreeCause(ctx, w, p.HeadSHA); reason != "" {
		return finalPublicationHold(reason)
	}
	return nil
}

// A transported report is usable only when intake identifies the same target.
func validFinalPublicationReport(raw json.RawMessage) bool {
	var report Implementation
	if !usableCarried(raw) || providers.ValidateReport(raw, true) != nil || json.Unmarshal(raw, &report) != nil || report.Status != "completed" || len(report.Questions) != 0 {
		return false
	}
	_, _, err := transferReport(raw)
	return err == nil
}

func seedFinalPublicationReport(d Descriptor, r *journal.Record) {
	// A prior-head review baseline must not skip the new target's review even
	// when a base-only merge leaves the visible PR diff unchanged.
	if r.ReviewBaseline != nil && r.ReviewBaseline.Head != d.HeadSHA {
		r.ReviewBaseline = nil
	}
	if hasJSON(r.Result) {
		if r.Head == d.HeadSHA && r.Generation == d.Generation && validFinalPublicationReport(r.Result) {
			return
		}
		r.PreviousResult, r.Result = r.Result, nil
	}
	r.Result = intakeImplementationReport(d)
}

func intakeImplementationReport(d Descriptor) json.RawMessage {
	var result struct {
		Head   string          `json:"implementationContent"`
		Report json.RawMessage `json:"implementationResult"`
	}
	if json.Unmarshal(d.Result, &result) != nil || result.Head != d.HeadSHA || !validFinalPublicationReport(result.Report) {
		return nil
	}
	return result.Report
}

func (e *Executor) seedFinalPublicationTest(ctx context.Context, d Descriptor, r *journal.Record) {
	var delivered struct {
		Test json.RawMessage `json:"fixedTest"`
	}
	if json.Unmarshal(d.Result, &delivered) != nil || !usableCarried(delivered.Test) {
		return
	}
	var carried TestResult
	if json.Unmarshal(delivered.Test, &carried) == nil && carried.AppliedEdits != nil {
		_, hash, err := editRequestHash(r.Result)
		if err == nil && carried.AppliedEdits.Head == r.Head && carried.AppliedEdits.RequestHash == hash {
			r.AppliedEdits = carried.AppliedEdits
		}
	}
	previous := r.Test
	previousUsable := reusableTestEvidence(r) && e.reviewTestEvidence(ctx, r, r.Head) == ""
	r.Test = delivered.Test
	if !reusableTestEvidence(r) || e.reviewTestEvidence(ctx, r, r.Head) != "" {
		r.Test = previous
		return
	}
	if previousUsable {
		r.Test = previous
	}
}

func finalPublicationReviewUsable(e *Executor, d Descriptor, r *journal.Record, requestHash, diff string) bool {
	_, ok := loadReviewBaseline(e.Config.RuntimeDir, r)
	if !ok || !reviewBaselineMatches(r, d, requestHash) || r.ReviewBaseline.Head != r.Head || r.ReviewBaseline.Decision != "approved" || digestReviewJSON(r.Review) != r.ReviewBaseline.ReviewHash {
		return false
	}
	hash, comparable := reviewDiffHash(diff)
	if !comparable || hash != r.ReviewBaseline.Hash {
		return false
	}
	var review Review
	return json.Unmarshal(r.Review, &review) == nil && review.Approved()
}

func finalPublicationReason(reason string) string {
	if strings.Contains(reason, finalApprovalWarning) {
		return reason
	}
	return reason + "。" + finalApprovalWarning
}
