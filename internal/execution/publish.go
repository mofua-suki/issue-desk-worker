package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func (e *Executor) publish(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, title string) error {
	if d.finalApprovalFixedTest {
		if err := e.checkPublishedTarget(ctx, d, w); err != nil {
			return err
		}
		if err := e.Store.Save(r); err != nil {
			return err
		}
		_, _ = e.reviewNotice(ctx, r)
		return nil
	}
	if err := w.Push(ctx); err != nil {
		var push *gitwork.PushError
		if errors.As(err, &push) && push.Error() == gitwork.PushBranchReason {
			if fetchErr := w.Fetch(ctx); fetchErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			if relationErr := checkTargetBranch(ctx, w, true); relationErr != nil {
				return relationErr
			}
		}
		return err
	}
	if r.PR == 0 {
		// Read open pulls first so a lost creation response cannot create a duplicate pull on restart.
		found, err := e.findOpenPull(ctx, d.Repo, r.Branch, w.Default, r.Head, r.PRCreationAttempted)
		if err != nil {
			return err
		}
		r.PR = found
		if r.PR == 0 {
			if r.PRCreationAttempted {
				return errPRCreationUncertain
			}
			r.PRCreationAttempted = true
			if err := e.Store.Save(r); err != nil {
				return err
			}
			var result Implementation
			_ = json.Unmarshal(r.Result, &result)
			// The summary is written by the implementing AI, so hide credentials it may quote.
			pull, err := e.Gitea.CreatePull(ctx, d.Repo, r.Branch, w.Default, title, e.redact(result.Summary)+fmt.Sprintf("\n\nCloses #%d\n\n固定テストと独立レビューの証跡は受付に記録しています。", d.Issue))
			if err != nil {
				if rejectedPost(err) {
					r.PRCreationAttempted = false
					if saveErr := e.Store.Save(r); saveErr != nil {
						return saveErr
					}
				}
				return err
			}
			r.PR = pull.Number
			if err := e.Store.Save(r); err != nil {
				return err
			}
		}
	}
	actual, err := e.Gitea.Pull(ctx, d.Repo, r.PR)
	if err != nil {
		return err
	}
	if !samePull(actual, d.Repo, r.Branch, w.Default, r.Head) || actual.Number != r.PR || (actual.Merged && actual.MergeCommitSHA != r.Head) || (!actual.Merged && actual.State != "open") {
		return errPullMismatch
	}
	if err := e.Store.Save(r); err != nil {
		return err
	}
	_, _ = e.reviewNotice(ctx, r)
	return nil
}
func (e *Executor) publishHeld(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, fence func(context.Context) error, reason, summary string) (intake.State, error) {
	if err := fence(ctx); err != nil {
		return intake.State{}, err
	}
	if err := e.publish(ctx, d, r, w, d.Title); err != nil {
		if errors.Is(err, errTargetBranchDiverged) {
			return e.holdTargetBranch(ctx, r, fence, err)
		}
		return intake.State{}, err
	}
	if merged, err := e.matchingMergedPull(ctx, d, r, w.Default); err != nil {
		return intake.State{}, err
	} else if merged {
		return e.finishMerged(ctx, r, fence, "公開済みPRの宛先とマージ済みコミットを確認しました", nil)
	}
	return e.finish(r, "held", reason, summary, map[string]any{"releasePending": true})
}
func (e *Executor) merge(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, fence func(context.Context) error) (bool, bool, error) {
	var held *mergeTurn
	defer func() {
		if held != nil {
			held.release()
		}
	}()
	return e.mergeWithTurn(ctx, d, r, w, fence, &held, gitwork.Snapshot{})
}

func (e *Executor) mergeWithTurn(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, fence func(context.Context) error, held **mergeTurn, previous gitwork.Snapshot) (bool, bool, error) {
	if merged, err := e.matchingMergedPull(ctx, d, r, w.Default); err != nil || merged {
		if err != nil && d.finalApprovalFixedTest && ctx.Err() == nil {
			return false, false, finalPublicationHold("マージ前に公開済みPRの状態を確認できません")
		}
		return merged, false, err
	}
	snapshot, err := w.SnapshotReusing(ctx, previous)
	if err != nil {
		return false, false, fmt.Errorf("マージ前の差分を確認できません: %w", err)
	}
	if err := e.releaseGate(ctx, d, r, w, snapshot); err != nil {
		return false, false, err
	}
	if *held == nil {
		turn, err := e.acquireMergeTurn(ctx, d, r, fence)
		if err != nil {
			return false, false, err
		}
		*held = turn
	}
	turn := *held
	ctx = turn.ctx
	renew := turn.renew
	var deadline time.Time
	for attempts := 1; ; attempts++ {
		if attempts > 1 {
			if err := fence(ctx); err != nil {
				return false, false, err
			}
			if err := renew(ctx); err != nil {
				return false, false, err
			}
		}
		if attempts > 1 {
			snapshot = gitwork.Snapshot{}
		}
		merged, changed, err := e.prepareMerge(ctx, d, r, w, fence, renew, snapshot)
		if err != nil || merged || changed {
			return merged, changed, err
		}
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if err := e.nativeGate(ctx, d.Repo, r.PR, w, r.Head, r.Base, func(c context.Context) error {
			if err := fence(c); err != nil {
				return err
			}
			return renew(c)
		}, fmt.Sprintf("%d/%d", d.ID, d.Generation)); err != nil {
			return false, false, err
		}
		if err := fence(ctx); err != nil {
			return false, false, err
		}
		if err := renew(ctx); err != nil {
			return false, false, err
		}
		if err := e.markMergeStarted(d.ID); err != nil {
			return false, false, err
		}
		mergeErr := e.Gitea.Merge(ctx, d.Repo, r.PR, r.Head)
		actual, err := e.Gitea.Pull(ctx, d.Repo, r.PR)
		if err != nil {
			if d.finalApprovalFixedTest && ctx.Err() == nil {
				return false, false, finalPublicationHold("マージ後のPRを取得できず結果を確認できません")
			}
			return false, false, err
		}
		if actual.Number == r.PR && actual.Merged && actual.MergeCommitSHA == r.Head && samePull(actual, d.Repo, r.Branch, w.Default, r.Head) {
			return true, false, nil
		}
		if mergeErr != nil && actual.Base.SHA != r.Base {
			if d.finalApprovalFixedTest {
				return false, false, finalPublicationHold("マージ拒否後に既定ブランチが更新されました")
			}
			var refusal *gitea.Error
			confirmed := errors.As(mergeErr, &refusal) && refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != 408 && refusal.Status != 429 && actual.Number == r.PR && samePull(actual, d.Repo, r.Branch, w.Default, r.Head) && !actual.Merged && actual.State == "open"
			changed, err := w.Follow(ctx)
			if err == nil && changed && confirmed {
				if err := fence(ctx); err != nil {
					return false, false, err
				}
				if err := ctx.Err(); err != nil {
					return false, false, err
				}
				if active := e.controls.get(d.ID); active != nil {
					active.mu.Lock()
					active.mergeStarted = false
					active.mu.Unlock()
				}
			}
			return false, changed, err
		}
		if mergeErr != nil {
			var refusal *gitea.Error
			if errors.As(mergeErr, &refusal) && refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != 408 && refusal.Status != 429 && actual.Number == r.PR && samePull(actual, d.Repo, r.Branch, w.Default, r.Head) && !actual.Merged && actual.State == "open" && actual.Base.SHA == r.Base {
				if err := ctx.Err(); err != nil {
					return false, false, err
				}
				if err := fence(ctx); err != nil {
					return false, false, err
				}
				if err := ctx.Err(); err != nil {
					return false, false, err
				}
				if refusal.Status == 405 && strings.Contains(strings.ToLower(refusal.Message), "please try again later") {
					if deadline.IsZero() {
						deadline = time.Now().Add(mergeReadLimit)
					}
					remaining := time.Until(deadline)
					if attempts < mergeReadRetries && remaining > 0 {
						wait := e.mergeRetryWait
						if wait == nil {
							wait = waitTransient
						}
						if err := wait(ctx, min(mergeReadInterval, remaining)); err != nil {
							return false, false, err
						}
						if time.Now().Before(deadline) {
							continue
						}
					}
				}
				message := "マージが拒否されました: " + mergeErrorReason(e, mergeErr)
				return false, false, &releaseHold{"review_held", message, map[string]any{"releasePending": true, "mergeRefusalMessage": message}}
			}
			return false, false, mergeErr
		}
		return false, false, errMergeUnverified
	}
}
func indexSlash(s string) int {
	for i, c := range s {
		if c == '/' {
			return i
		}
	}
	return 0
}

type permissionApprovalRecord struct {
	ID          string   `json:"id"`
	Head        string   `json:"head"`
	PR          int64    `json:"pr"`
	Files       []string `json:"files"`
	Resume      bool     `json:"resumePublication"`
	IssuePosted bool     `json:"issuePosted"`
	PRPosted    bool     `json:"prPosted"`
	Statement   string   `json:"approvalStatement"`
	From        string   `json:"approvedFrom"`
}

// readPermissionApproval checks everything except the commit the approval was given for.
func readPermissionApproval(d Descriptor, pr int64, files []string) (permissionApprovalRecord, bool) {
	var a permissionApprovalRecord
	if json.Unmarshal(d.View["permissionApproval"], &a) != nil {
		return a, false
	}
	if a.ID == "" || a.Head == "" || a.PR != pr || !a.Resume || !a.IssuePosted || !a.PRPosted || a.Statement == "" || (a.From != "screen" && a.From != "token") {
		return a, false
	}
	left, right := append([]string{}, files...), append([]string{}, a.Files...)
	slices.Sort(left)
	slices.Sort(right)
	return a, slices.Equal(left, right)
}

// permissionApprovedContent also accepts an approval given to an earlier commit
// of this branch when that commit is an ancestor of head and every protected
// file still exists with byte-identical content. Anything that cannot be
// confirmed keeps the hold.
func permissionApprovedContent(ctx context.Context, w *gitwork.Workspace, d Descriptor, head string, pr int64, files []string) bool {
	a, ok := readPermissionApproval(d, pr, files)
	if !ok {
		return false
	}
	if a.Head == head {
		return true
	}
	if w == nil {
		return false
	}
	same, err := w.SameFileContents(ctx, a.Head, head, files)
	return err == nil && same
}
