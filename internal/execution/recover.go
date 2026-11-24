package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/intake"
	"os"
)

func (e *Executor) Recover(ctx context.Context) error {
	if err := sweepStaleTemps(e.Config.RuntimeDir); err != nil {
		fmt.Fprintf(os.Stderr, "前回の一時領域を消せませんでした: %v\n", err)
	}
	// Unsent notices are retried by every request cycle, so one that cannot
	// be sent yet must not keep this PC from starting.
	if err := e.recoverNotifications(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(os.Stderr, "未送信の通知を送れません（依頼の周期で再試行します）: %v\n", err)
	}
	records, err := e.Store.List()
	if err != nil {
		return err
	}
	for i := range records {
		r := &records[i]
		if err := e.flushUsage(ctx, r); err != nil {
			if !errors.Is(err, errUsageRefused) {
				return err
			}
			// A rejected row must not keep this PC from taking any other work.
			fmt.Fprintf(os.Stderr, "仕事%dの使用量を受付が拒否しました: %v\n", r.ID, err)
		}
		final := hasJSON(r.FinalState)
		epoch := r.Epoch
		if final && r.FinalReportEpoch > 0 {
			epoch = r.FinalReportEpoch
		}
		if epoch < 1 || r.FinalReported {
			continue
		}
		var state intake.State
		if final {
			if json.Unmarshal(r.FinalState, &state) != nil {
				// One unreadable record must not keep this PC from starting.
				fmt.Fprintf(os.Stderr, "仕事%dの未送信の最終結果が不正なため、送らずに残します\n", r.ID)
				continue
			}
			if err := boundFinalResult(epoch, &state); err != nil {
				fmt.Fprintf(os.Stderr, "仕事%dの未送信の最終結果を送らずに残します: %v\n", r.ID, err)
				continue
			}
		} else {
			state = intake.State{Status: "queued", Branch: r.Branch, HeadSHA: r.Head}
			e.addDiffFingerprint(ctx, r, &state)
		}
		err := e.API.State(ctx, r.ID, epoch, state)
		if err == nil && final {
			r.FinalReported = true
			if err := e.Store.Save(r); err != nil {
				return err
			}
		}
		if err != nil {
			var refusal *intake.APIError
			if !errors.As(err, &refusal) || !recoverableRefusal(refusal) {
				return err
			}
			// The record stays unreported and is sent again at the next start.
			if !refusal.LeaseLost() {
				fmt.Fprintf(os.Stderr, "仕事%dの未送信の報告を受付が拒否しました: %v\n", r.ID, err)
			}
		}
	}
	return nil
}

// recoverableRefusal reports whether one job's refused report may be left behind.
// Authentication failures and server errors affect every job, so they still stop the start.
func recoverableRefusal(refusal *intake.APIError) bool {
	return refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != 401 && refusal.Status != 403
}

func (e *Executor) Reported(id, epoch int64) error {
	r, err := e.Store.Load(id)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	r.FinalReported = true
	if r.FinalReportEpoch > 0 {
		var state intake.State
		if json.Unmarshal(r.FinalState, &state) == nil && state.Status == "merged" {
			// The report follows the new lease; the notice keeps the original epoch.
			return e.Store.Save(r)
		}
	}
	r.Epoch = epoch
	return e.Store.Save(r)
}
