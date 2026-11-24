package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/atomicfile"
	"os"
	"path/filepath"
	"strings"
)

type repostOutcome struct {
	Result     string `json:"result"`
	Reason     string `json:"reason,omitempty"`
	ProposalID string `json:"proposalId,omitempty"`
	CommentID  int64  `json:"commentId,omitempty"`
}
type repostResult struct {
	Command     command       `json:"command"`
	Notice      *notification `json:"notice,omitempty"`
	Status      string        `json:"status"`
	Result      repostOutcome `json:"result"`
	AckStatus   string        `json:"ackStatus,omitempty"`
	AckRejected int           `json:"ackRejected,omitempty"`
}

func (e *Executor) repostResultFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(e.Config.RuntimeDir, "control", "reposts", hex.EncodeToString(sum[:])+".json")
}
func (e *Executor) saveRepostResult(r *repostResult) error {
	file := e.repostResultFile(r.Command.ID)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	return atomicfile.Write(file, ".repost-", 0600, true, encode(r), nil)
}
func (e *Executor) loadRepostResult(cmd command) (*repostResult, error) {
	raw, err := os.ReadFile(e.repostResultFile(cmd.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r repostResult
	if len(raw) > 1<<20 || json.Unmarshal(raw, &r) != nil {
		return nil, errors.New("保存済み再投稿指示と要求が一致しません")
	}
	if string(encode(r.Command)) != string(encode(cmd)) {
		return &r, errRepostResultMismatch
	}
	return &r, nil
}
func (e *Executor) finishRepostResult(ctx context.Context, r *repostResult) error {
	if r.Notice != nil && r.Status == "done" {
		_, err := e.syncNotification(ctx, *r.Notice)
		if errors.Is(err, errRepostRefused) {
			reason, err := e.repostNoticeRefusal(*r.Notice)
			if err != nil {
				return err
			}
			r.Status = "failed"
			r.Result = repostOutcome{Result: "refused", Reason: reason}
			if err := e.saveRepostResult(r); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	// Even a delivered command may be listed again after an ack response was lost.
	return e.ackRepostResult(ctx, r)
}
func (e *Executor) recoverRepostResults(ctx context.Context) error {
	dir := filepath.Join(e.Config.RuntimeDir, "control", "reposts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil // A repost-only directory must not prevent Worker startup.
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue // Isolate this record; never recreate or send an unreadable one.
		}
		var r repostResult
		if len(raw) > 1<<20 || json.Unmarshal(raw, &r) != nil || e.repostResultFile(r.Command.ID) != filepath.Join(dir, entry.Name()) {
			continue
		}
		// A queued result is not a delivery receipt. Recovery can discover a later
		// definite refusal and report failed (a server 409 means it already finalized).
		refused := false
		if r.Notice != nil {
			raw, err := os.ReadFile(e.notificationFile(*r.Notice))
			if err != nil {
				continue
			}
			var n notification
			if json.Unmarshal(raw, &n) != nil {
				continue
			}
			refused = n.Stage == "refused"
		}
		if r.AckStatus == r.Status && !(refused && r.Status == "done") {
			continue
		}
		// Transient API errors and individual damaged records remain pending.
		// Neither may block other notices, commands, requests, or startup.
		_ = e.finishRepostResult(ctx, &r)
	}
	return nil
}
