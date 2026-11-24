package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/requeststore"
	"slices"
	"strings"
	"time"
)

func (e *Executor) mergeRequestedPull(ctx context.Context, progress *requestProgress) (map[string]any, error) {
	return e.mergeRequestedPullObserved(ctx, progress, nil)
}

func (e *Executor) mergeRequestedPullObserved(ctx context.Context, progress *requestProgress, observe func(TestResult)) (map[string]any, error) {
	m, result, err := e.readMergeRequest(ctx, progress)
	if m == nil {
		return result, err
	}
	m.observe = observe
	granted, err := m.acquireTurn()
	if err != nil {
		return nil, err
	}
	if !granted {
		return nil, e.releaseRequest(ctx, m.r.ID)
	}
	defer m.turn.release()
	for _, step := range []func() (bool, map[string]any, error){m.prepareCandidate, m.testCandidate, m.recheckAfterTest, m.pushCandidate} {
		if stop, result, err := step(); stop {
			return result, err
		}
	}
	return m.merge()
}

// requestedMerge is the state of one PR merge request.
type requestedMerge struct {
	e        *Executor
	ctx      context.Context
	progress *requestProgress
	r        monitorRequest
	p        mergeRequestPayload
	observe  func(TestResult)
	repo     gitea.Repository
	branch   string
	// pullRef is the PR's head branch when the request was read.
	pullRef string
	// route and who address the merge turn; turnCtx ends with the turn.
	route   string
	who     map[string]any
	turn    *mergeTurn
	turnCtx context.Context
	w       *gitwork.Workspace
	// head is the PR head the turn started from; candidate is the tested head
	// after following the base, and testedBase is that base.
	head           string
	candidate      string
	testedBase     string
	gate           *mergeGate
	createApproved bool
}

type mergeRequestPayload struct {
	Number   int64  `json:"number"`
	Head     string `json:"head"`
	Approval string `json:"approval"`
	// ApprovalDelegated は受付が付ける印で、運用代行を許したトークンから承認の発言が届いたことを表す。
	ApprovalDelegated bool `json:"approvalDelegated"`
	// ApprovalRequest は、承認を付けた作成依頼（pr_create）のID。受付が照合して承認を写したときだけ付く。
	ApprovalRequest string `json:"approvalRequest"`
}

// readMergeRequest validates the request against the PR and the repository.
// It returns no merge with the result when the request ends here.
func (e *Executor) readMergeRequest(ctx context.Context, progress *requestProgress) (*requestedMerge, map[string]any, error) {
	r := progress.Request
	var p mergeRequestPayload
	if json.Unmarshal(r.Payload, &p) != nil || p.Number < 1 || !gitwork.ValidSHA(p.Head) {
		return nil, refusal("PRマージ依頼が不正です"), nil
	}
	pull, err := e.Gitea.Pull(ctx, r.Repo, p.Number)
	if err != nil {
		return nil, nil, err
	}
	repo, err := e.Gitea.Repo(ctx, r.Repo)
	if err != nil {
		return nil, nil, err
	}
	branch := progress.Branch
	if branch == "" {
		branch = pull.Head.Ref
	}
	if progress.Base != "" && progress.Base != repo.DefaultBranch {
		return nil, refusal("既定ブランチが依頼時から変わりました"), nil
	}
	sentHead := progress.Head
	if sentHead == "" {
		sentHead = p.Head
	}
	if !gitwork.ValidSHA(sentHead) {
		return nil, refusal("保存済みheadが不正です"), nil
	}
	if progress.Stage == "posted" {
		ancestor := func(commit string) (bool, error) {
			w, err := e.requestMergeWorkspace(ctx, r, repo.DefaultBranch, repo.DefaultBranch)
			if err != nil {
				return false, err
			}
			return mergeCommitDescendsFrom(ctx, w, sentHead, commit)
		}
		result, err := mergeOutcome(pull, p.Number, r.Repo, branch, repo.DefaultBranch, sentHead, "指定headのfast-forwardマージを確認できません", progress.MergeError, ancestor)
		return nil, result, err
	}

	if pull.Number != p.Number || pull.Merged || pull.State != "open" || pull.Draft || pull.Base.Ref != repo.DefaultBranch || pull.Base.Repo.FullName != r.Repo || pull.Head.Repo.FullName != r.Repo || (pull.Head.SHA != p.Head && (progress.Head == "" || pull.Head.SHA != progress.Head)) || !mergeBranchAllowed(pull.Head.Ref, p.Number, p.Approval) {
		return nil, refusal("同じリポジトリの開いたPR・既定ブランチ・依頼時のheadを確認できません"), nil
	}
	found, err := e.hasChangesRequested(ctx, r.Repo, p.Number)
	if err != nil {
		return nil, nil, err
	}
	if found {
		return nil, refusal("変更要求のレビューが残っています"), nil
	}
	createApproved := preapprovedByCreate(p, pull.Body)
	m := &requestedMerge{e: e, ctx: ctx, progress: progress, r: r, p: p, repo: repo, branch: branch, pullRef: pull.Head.Ref, route: mergeTurnRoute(r.Repo), head: pull.Head.SHA, createApproved: createApproved}
	m.who = map[string]any{"requestId": r.ID}
	e.addNativeTestGate(r.Repo, m.who)
	return m, nil, nil
}

// acquireTurn asks for the repository's merge turn and, when granted,
// starts renewing it; turnCtx ends with the turn.
func (m *requestedMerge) acquireTurn() (bool, error) {
	var acquired struct {
		Granted bool `json:"granted"`
		Turn    struct {
			Epoch     int64     `json:"epoch"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"turn"`
	}
	if err := m.e.API.Call(m.ctx, "POST", m.route+"/acquire", m.who, &acquired); err != nil {
		return false, err
	}
	if !acquired.Granted {
		return false, nil
	}
	m.who["turnEpoch"] = acquired.Turn.Epoch
	m.turn = m.e.holdMergeTurn(m.ctx, m.route, m.who, acquired.Turn.ExpiresAt)
	m.turnCtx = m.turn.ctx
	return true, nil
}

// renew renews the merge turn at a step that must not pass a lost turn.
func (m *requestedMerge) renew(ctx context.Context) error {
	return m.turn.renew(ctx)
}

// prepareCandidate opens the PR branch, checks its snapshot and follows the
// default branch into the candidate head.
func (m *requestedMerge) prepareCandidate() (bool, map[string]any, error) {
	e, ctx := m.e, m.turnCtx
	w, err := e.requestMergeWorkspace(ctx, m.r, m.repo.DefaultBranch, m.pullRef)
	if err != nil {
		return true, nil, err
	}
	m.w = w
	head, err := w.Head(ctx)
	if err != nil {
		return true, nil, err
	}
	if head != m.head {
		return true, refusal("取得したPRの先頭が依頼時から変わっています"), nil
	}
	m.gate = &mergeGate{approved: m.p.ApprovalDelegated && (approvalMentions(m.p.Approval, m.p.Number) || m.createApproved)}
	if result, err := e.checkMergeSnapshot(ctx, w, m.r.Repo, m.gate); err != nil || result != nil {
		return true, result, err
	}
	if result, err := refuseFastForwardFollow(ctx, w, head); err != nil || result != nil {
		return true, result, err
	}
	followed, err := w.Follow(ctx)
	if err != nil {
		if errors.Is(err, gitwork.ErrConflict) {
			return true, refusal("既定ブランチとの取り込みで競合しました"), nil
		}
		return true, nil, err
	}
	m.testedBase, err = w.Base(ctx)
	if err != nil {
		return true, nil, err
	}
	m.candidate, err = w.Head(ctx)
	if err != nil {
		return true, nil, err
	}
	if followed || m.candidate != m.p.Head {
		if result, err := validateMergeChain(ctx, w, m.p.Head, m.candidate, m.testedBase); err != nil || result != nil {
			return true, result, err
		}
	}

	if result, err := e.checkMergeSnapshot(ctx, w, m.r.Repo, m.gate); err != nil || result != nil {
		return true, result, err
	}
	return false, nil, nil
}

// testCandidate runs the repository's fixed test on the candidate and refuses
// a failed test or a tree the test left dirty.
func (m *requestedMerge) testCandidate() (bool, map[string]any, error) {
	e, ctx, w, candidate := m.e, m.turnCtx, m.w, m.candidate
	configuration, err := e.Config.Repository(m.r.Repo)
	if err != nil {
		return true, nil, err
	}
	record := &journal.Record{Repo: m.r.Repo, Base: m.testedBase, Head: candidate, Tree: w.Tree, Result: json.RawMessage(`{}`)}
	if err := e.setup(ctx, configuration, record); err != nil {
		if ctx.Err() != nil {
			return true, nil, ctx.Err()
		}
		return true, refusal(fixedTestMessage("固定テストの環境を準備できません: ", e.redact(err.Error()), 2000, true)), nil
	}
	tested, err := e.fixedTest(ctx, configuration, record, func() error {
		actual, err := e.testHead(ctx, record)
		if err != nil {
			return err
		}
		if actual != candidate {
			return errors.New("固定テスト対象が依頼時のheadと一致しません")
		}
		return nil
	})
	if err != nil {
		return true, nil, err
	}
	record.Test = encode(tested)
	if tested.Passed && !reusableTestEvidence(record) {
		var persisted TestResult
		_ = json.Unmarshal(record.Test, &persisted)
		reason := testEvidenceReason(persisted, candidate, m.testedBase)
		if reason == "" {
			reason = "固定テストの完全な合格証拠を確認できません"
		}
		rejectTestEvidence(&tested, reason)
	}
	if m.observe != nil {
		m.observe(tested)
	}
	if tested.evidenceMissing && !w.Clean(ctx) {
		if ctx.Err() != nil {
			return true, nil, ctx.Err()
		}
		result, err := e.dirtyMergeRefusal(ctx, w)
		return true, result, err
	}
	if !tested.Passed {
		if ctx.Err() != nil {
			return true, nil, ctx.Err()
		}
		if tested.Unavailable {
			hold := tested.environmentHold
			prefix := "実行環境の問題で固定テストを実行できません: 保留理由: " + e.redact(hold.Reason) + "、開始時刻: " + hold.Since + "\n"
			return true, refusal(fixedTestMessage(prefix, e.redact(tested.Detail), 2000, tested.evidenceReason)), nil
		}
		return true, refusal(fixedTestMessage("固定テストが不合格です: ", e.redact(tested.Detail), 2000, tested.evidenceReason)), nil
	}
	if !w.Clean(ctx) {
		if ctx.Err() != nil {
			return true, nil, ctx.Err()
		}
		result, err := e.dirtyMergeRefusal(ctx, w)
		return true, result, err
	}
	return false, nil, nil
}

// baseMoved fetches and reports whether the default branch moved from the
// tested base. A moved base releases the request to run again.
func (m *requestedMerge) baseMoved() (bool, error) {
	if err := m.w.Fetch(m.turnCtx); err != nil {
		return true, err
	}
	base, err := m.w.Base(m.turnCtx)
	if err != nil {
		return true, err
	}
	if base != m.testedBase {
		return true, m.e.releaseRequest(m.ctx, m.r.ID)
	}
	return false, nil
}

// recheckAfterTest checks that the base, the PR and its reviews did not change
// during the test, and saves the candidate.
func (m *requestedMerge) recheckAfterTest() (bool, map[string]any, error) {
	e, ctx, r, p := m.e, m.turnCtx, m.r, m.p
	if moved, err := m.baseMoved(); moved {
		return true, nil, err
	}
	pull, err := e.Gitea.Pull(ctx, r.Repo, p.Number)
	if err != nil {
		return true, nil, err
	}
	if pull.Number != p.Number || pull.Merged || !samePull(pull, r.Repo, m.branch, m.repo.DefaultBranch, m.head) || pull.State != "open" || pull.Draft {
		return true, refusal("テスト後にPRのhead・状態・マージ可能性が変わりました"), nil
	}
	found, err := e.hasChangesRequested(ctx, r.Repo, p.Number)
	if err != nil {
		return true, nil, err
	}
	if found {
		return true, refusal("変更要求のレビューが残っています"), nil
	}
	if err := m.renew(ctx); err != nil {
		return true, nil, err
	}
	m.progress.Head = m.candidate
	m.progress.Branch = m.branch
	m.progress.Base = m.repo.DefaultBranch
	if err := e.saveRequest(m.progress); err != nil {
		return true, nil, err
	}
	return false, nil, nil
}

// pushCandidate pushes a followed candidate and waits until Gitea reports the
// PR at the candidate as mergeable.
func (m *requestedMerge) pushCandidate() (bool, map[string]any, error) {
	e, ctx, r, p, w := m.e, m.turnCtx, m.r, m.p, m.w
	if m.candidate != m.head {
		out, err := w.Config.Run(ctx, w.Tree, "push", "origin", "HEAD:refs/heads/"+w.Branch)
		if err != nil || out.Code != 0 {
			if ctx.Err() != nil {
				return true, nil, ctx.Err()
			}
			reason := out.Err + "\n" + out.Out
			if err != nil {
				reason += "\n" + err.Error()
			}
			return true, mergeFailed("PRブランチへのpushに失敗しました", pushDiagnostic(e, reason)), nil
		}
	}
	// Gitea may need time to recompute mergeability after the push.
	for attempt := 0; attempt < 60; attempt++ {
		current, err := e.Gitea.Pull(ctx, r.Repo, p.Number)
		if err != nil {
			return true, nil, err
		}
		if current.Number != p.Number || current.Merged || current.State != "open" || current.Draft || !samePull(current, r.Repo, m.branch, m.repo.DefaultBranch, m.candidate) {
			if current.Head.SHA != m.head && current.Head.SHA != m.candidate {
				return true, refusal("push後にPRのheadが変わりました"), nil
			}
		} else if current.Mergeable {
			return false, nil, nil
		}
		select {
		case <-ctx.Done():
			return true, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return true, mergeFailed("push後のPRのhead・マージ可能性を確認できません", ""), nil
}

// merge rechecks the base, records the approval, passes the native gate and
// merges the candidate. The outcome is decided by reading the PR again.
func (m *requestedMerge) merge() (map[string]any, error) {
	e, ctx, r, p, w, progress, candidate := m.e, m.turnCtx, m.r, m.p, m.w, m.progress, m.candidate
	if moved, err := m.baseMoved(); moved {
		return nil, err
	}
	if err := m.renew(ctx); err != nil {
		return nil, err
	}
	if (len(m.gate.protected) > 0 || m.gate.sql) && !progress.ApprovalRecorded {
		if err := e.Gitea.Comment(ctx, r.Repo, p.Number, mergeApprovalRecord(e, p, m.gate)); err != nil {
			return nil, err
		}
		progress.ApprovalRecorded = true
		if err := e.saveRequest(progress); err != nil {
			return nil, err
		}
	}
	if err := e.nativeGate(ctx, r.Repo, p.Number, w, candidate, m.testedBase, m.renew, r.ID); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return refusal(err.Error()), nil
	}
	if err := m.renew(ctx); err != nil {
		return nil, err
	}
	found, err := e.hasChangesRequested(ctx, r.Repo, p.Number)
	if err != nil {
		return nil, err
	}
	if found {
		return refusal("変更要求のレビューが残っています"), nil
	}
	progress.Stage = "posted"
	progress.Branch = m.branch
	progress.Base = m.repo.DefaultBranch
	if err := e.saveRequest(progress); err != nil {
		return nil, err
	}
	// マージの成否は読み直しで決める。エラーでも応答が失われただけでマージ済みの場合がある。
	if mergeErr := e.Gitea.Merge(ctx, r.Repo, p.Number, candidate); mergeErr != nil {
		progress.MergeError = mergeErrorReason(e, mergeErr)
		if err := e.saveRequest(progress); err != nil {
			return nil, withMergeError(err, progress.MergeError)
		}
	}
	actual, err := e.Gitea.Pull(m.ctx, r.Repo, p.Number)
	if err != nil {
		return nil, withMergeError(err, progress.MergeError)
	}
	ancestor := func(mergeCommit string) (bool, error) {
		return mergeCommitDescendsFrom(m.ctx, w, candidate, mergeCommit)
	}
	return mergeOutcome(actual, p.Number, r.Repo, m.branch, m.repo.DefaultBranch, candidate, "マージ後のPRの結果を確認できません", progress.MergeError, ancestor)
}

func mergeCommitDescendsFrom(ctx context.Context, w *gitwork.Workspace, head, commit string) (bool, error) {
	if err := w.Fetch(ctx); err != nil {
		return false, err
	}
	// A stale object in the mirror alone is not evidence from the fetched refs.
	refs, err := w.Config.Checked(ctx, w.Tree, "for-each-ref", "--contains", commit, "--format=%(refname)", "refs/remotes/origin/")
	if err != nil || refs == "" {
		return false, err
	}
	args := []string{"merge-base", "--is-ancestor", head, commit}
	out, err := w.Config.Run(ctx, w.Tree, args...)
	if err != nil {
		return false, err
	}
	switch out.Code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, gitwork.CommandError(args, out)
	}
}

// mergeOutcome は、マージ要求の後(または再開時)に読み直したPRから結果を決める。
// マージ済みでマージコミットが依頼時headと一致するか、依頼時headがその祖先だと確認できればmerged。
// Gitの異常時はエラーを返して再開に委ね、祖先の証拠がないときは結果未確認とする。
// 実際に未マージと確認できたときだけ、観測した状態(merged・open・mergeable)を添えて失敗にする。
func mergeOutcome(pull gitea.Pull, number int64, repo, branch, base, head, unconfirmed, reason string, ancestor func(string) (bool, error)) (map[string]any, error) {
	if pull.Number != number || !samePull(pull, repo, branch, base, head) {
		return mergeFailed(unconfirmed, reason), nil
	}
	if !pull.Merged {
		message := fmt.Sprintf("%s(観測: merged=false, open=%t, mergeable=%t)", unconfirmed, pull.State == "open", pull.Mergeable)
		return mergeFailed(message, reason), nil
	}
	if pull.MergeCommitSHA == head {
		return map[string]any{"status": "merged", "mergeCommit": pull.MergeCommitSHA}, nil
	}
	if ancestor != nil && gitwork.ValidSHA(pull.MergeCommitSHA) {
		ok, err := ancestor(pull.MergeCommitSHA)
		if err != nil {
			return nil, withMergeError(err, reason)
		}
		if ok {
			return map[string]any{"status": "merged", "mergeCommit": pull.MergeCommitSHA}, nil
		}
	}
	return mergeFailed("マージ結果未確認: 依頼時headとマージコミットの祖先関係を確認できません", reason), nil
}

// validateMergeChain permits bounded successive base merges, but no independent commits.
func validateMergeChain(ctx context.Context, w *gitwork.Workspace, original, candidate, base string) (map[string]any, error) {
	current := candidate
	for depth := 0; current != original; depth++ {
		if depth >= 8 {
			return refusal("取り込み履歴の上限を超えました"), nil
		}
		out, err := w.Config.Run(ctx, w.Tree, "rev-list", "--parents", "-n", "1", current)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(out.Out)
		if out.Code != 0 || len(fields) != 3 || fields[0] != current {
			return refusal("取り込み履歴に別の変更が含まれています"), nil
		}
		ancestor, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", fields[2], base)
		if err != nil {
			return nil, err
		}
		if ancestor.Code != 0 {
			return refusal("取り込み元が既定ブランチの履歴にありません"), nil
		}
		merged, refused, err := mergedTree(ctx, w, fields[1], fields[2])
		if err != nil {
			return nil, err
		}
		if refused != nil {
			return refused, nil
		}
		actual, err := w.Config.Run(ctx, w.Tree, "rev-parse", current+"^{tree}")
		if err != nil {
			return nil, err
		}
		if actual.Code != 0 || merged != strings.TrimSpace(actual.Out) {
			return refusal("取り込み履歴に別の変更が含まれています"), nil
		}
		current = fields[1]
	}
	expected, refused, err := mergedTree(ctx, w, original, base)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return refused, nil
	}
	tree, err := w.Config.Run(ctx, w.Tree, "rev-parse", candidate+"^{tree}")
	if err != nil {
		return nil, err
	}
	if tree.Code != 0 || expected != strings.TrimSpace(tree.Out) {
		return refusal("取り込みコミットに別の変更が含まれています"), nil
	}
	return nil, nil
}

// refuseFastForwardFollow refuses before Follow when the PR head is already part of
// the default branch: following would fast-forward to the base and send commits that
// are not this PR's changes. It does not depend on the shape of the base's history.
func refuseFastForwardFollow(ctx context.Context, w *gitwork.Workspace, head string) (map[string]any, error) {
	if err := w.Fetch(ctx); err != nil {
		return nil, err
	}
	base, err := w.Base(ctx)
	if err != nil {
		return nil, err
	}
	if head == base {
		return nil, nil
	}
	check, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", head, base)
	if err != nil {
		return nil, err
	}
	switch check.Code {
	case 0:
		return refusal("PRの先頭が既定ブランチに含まれていて、取り込むと早送りになるのでマージしません"), nil
	case 1:
		return nil, nil
	}
	return refusal("PRの先頭と既定ブランチの関係を確認できません: " + clip(strings.TrimSpace(check.Err), 300)), nil
}

// mergedTree returns the tree of merging two commits. Exit code 1 is a real conflict;
// anything else means the check itself could not run (merge-tree --write-tree needs
// Git 2.38), and the comparison must not be skipped. Only an interrupted ctx returns an
// error, so that the request is resumed. The command's own time limit with ctx alive is
// a refusal: resuming would hit the same limit again and repeat forever.
func mergedTree(ctx context.Context, w *gitwork.Workspace, first, second string) (string, map[string]any, error) {
	tree, err := w.Config.MergeTree(ctx, w.Tree, first, second)
	switch {
	case err == nil:
		return tree, nil, nil
	case errors.Is(err, gitwork.ErrConflict):
		return "", refusal("取り込みの照合で競合しました"), nil
	case ctx.Err() != nil:
		return "", nil, ctx.Err()
	}
	return "", refusal("取り込み内容を照合できません: " + clip(err.Error(), 300)), nil
}

func pushDiagnostic(e *Executor, text string) string {
	return clip(strings.TrimSpace(e.redact(process.HideURLCredentials(text))), mergeErrorLimit)
}

// checkMergeSnapshot checks HEAD's diff against its merge base with the
// default branch, so changes made only on the base branch are excluded.
func (e *Executor) checkMergeSnapshot(ctx context.Context, w *gitwork.Workspace, repo string, gate *mergeGate) (map[string]any, error) {
	snapshot, err := w.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return e.checkMergeSnapshotData(snapshot, repo, gate)
}

// mergeGate は、権限保護対象・データ削除SQLを通してよいかと、記録に残す対象を持つ。
type mergeGate struct {
	approved  bool
	protected []string
	sql       bool
}

func (e *Executor) checkMergeSnapshotData(snapshot gitwork.Snapshot, repo string, gate *mergeGate) (map[string]any, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return refusal(err.Error()), nil
	}
	if e.secret != "" && strings.Contains(snapshot.Diff, e.secret) {
		return refusal("変更に資格情報が含まれています"), nil
	}
	protected, err := e.permissionFiles(snapshot, repo)
	if err != nil {
		return refusal(err.Error()), nil
	}
	if len(protected) > 0 && !gate.approved {
		return refusal("権限保護対象の変更は、PR番号を挙げた利用者の承認の発言（運用代行トークンの --approval）か、PR作成依頼に付けた事前承認（--approval-request）が必要です"), nil
	}
	holdsSQL := sqlHold(snapshot)
	if holdsSQL && !gate.approved {
		return refusal("データ削除SQLは、PR番号を挙げた利用者の承認の発言（運用代行トークンの --approval）か、PR作成依頼に付けた事前承認（--approval-request）が必要です"), nil
	}
	for _, file := range protected {
		if !slices.Contains(gate.protected, file) {
			gate.protected = append(gate.protected, file)
		}
	}
	gate.sql = gate.sql || holdsSQL
	return nil, nil
}

// mergeApprovalRecord は、利用者の承認でマージする権限保護対象・データ削除SQLの記録をPRへ残す本文を作る。
func mergeApprovalRecord(e *Executor, p mergeRequestPayload, gate *mergeGate) string {
	lines := []string{"マージの承認の記録（チャットの利用者の承認）", "利用者の発言: " + strings.ReplaceAll(e.redact(p.Approval), "<!--", "&lt;!--"), "対象コミット: " + p.Head}
	if p.ApprovalRequest != "" {
		lines = append(lines, "承認の出どころ: PR作成依頼 "+p.ApprovalRequest+"（PR番号が決まる前の承認）")
	}
	if gate.sql {
		lines = append(lines, "データ削除SQLを含みます")
	}
	for _, file := range gate.protected {
		lines = append(lines, "権限ファイル: "+file)
	}
	return strings.Join(lines, "\n")
}

const mergeErrorLimit = 500

// mergeErrorReason は、マージ要求のエラーを、資格情報を伏せて長さを抑えた文にする。
func mergeErrorReason(e *Executor, err error) string {
	text := err.Error()
	var refused *gitea.Error
	if errors.As(err, &refused) && refused.Message != "" {
		text += ": " + refused.Message
	}
	return clip(e.redact(text), mergeErrorLimit)
}

func mergeFailed(message, reason string) map[string]any {
	if reason != "" {
		message += ": " + reason
	}
	return map[string]any{"status": "failed", "message": message}
}

func withMergeError(err error, reason string) error {
	if reason == "" {
		return err
	}
	return fmt.Errorf("%w（マージ要求の結果: %s）", err, reason)
}

var errRequestReleased = requeststore.ErrReleased

func (e *Executor) releaseRequest(ctx context.Context, id string) error {
	return requeststore.Release(ctx, e.API, id)
}

// mergeBranchAllowed は、マージ依頼の対象ブランチを許すか決める。
// IssueのPR(codex/issue-…)は、受付のレビュー・マージの流れが担当するので、通常は拒否する。
// 利用者がそのPR番号を挙げて承認した発言(approval)が付いたときだけ、通す。固定テスト・権限保護・秘密の検査は変わらず通す。
func mergeBranchAllowed(branch string, number int64, approval string) bool {
	if !gitwork.ValidBranchShape(branch) {
		return false
	}
	if !strings.HasPrefix(branch, "codex/issue-") {
		return true
	}
	return approvalMentions(approval, number)
}

// approvalMentions は、承認発言が8文字以上で、PR番号を単独の数として含むかを見る。
func approvalMentions(approval string, number int64) bool {
	text := strings.TrimSpace(approval)
	if len([]rune(text)) < 8 || number < 1 {
		return false
	}
	want := fmt.Sprint(number)
	for _, token := range strings.FieldsFunc(text, func(r rune) bool { return r < '0' || r > '9' }) {
		if token == want {
			return true
		}
	}
	return false
}
