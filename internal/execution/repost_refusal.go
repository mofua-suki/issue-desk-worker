package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

var (
	errRepostMarkerConflict = errors.New("repost_marker_author")
	errRepostResultMismatch = errors.New("repost_result_mismatch")
)

// Caller holds notificationsMu. Stop only this repost, preserving the body and
// attempt history in its outbox rather than making it eligible for another POST.
func (e *Executor) stopRepostNotice(n notification, reason string) error {
	n.Stage = "refused"
	n.RefusalReason = reason
	if err := e.saveNotification(n); err != nil {
		return err
	}
	return errRepostRefused
}

func (e *Executor) repostNoticeRefusal(n notification) (string, error) {
	raw, err := os.ReadFile(e.notificationFile(n))
	if err != nil {
		return "", err
	}
	var saved notification
	if json.Unmarshal(raw, &saved) != nil || saved.Stage != "refused" {
		return "", errors.New("再投稿の送信停止記録が不正です")
	}
	if saved.RefusalReason == "" {
		return "post_4xx", nil // Compatibility with existing refused outboxes.
	}
	return saved.RefusalReason, nil
}

func (e *Executor) refuseMismatchedRepost(ctx context.Context, cmd command, saved *repostResult) error {
	if saved.Notice != nil {
		e.notificationsMu.Lock()
		raw, err := os.ReadFile(e.notificationFile(*saved.Notice))
		if err == nil {
			var n notification
			if json.Unmarshal(raw, &n) == nil && n.Kind == "repost" {
				err = e.stopRepostNotice(n, "saved_repost_mismatch")
			}
		}
		e.notificationsMu.Unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errRepostRefused) {
			return err
		}
	}
	r := &repostResult{Command: cmd, Status: "failed", Result: repostOutcome{Result: "refused", Reason: "saved_repost_mismatch"}}
	if err := e.saveRepostResult(r); err != nil {
		return err
	}
	return e.ackRepostResult(ctx, r)
}
