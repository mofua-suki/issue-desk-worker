package hostd

import (
	"context"
	"errors"
	"time"
)

type apiStatusError struct{ Status int }

func (e *apiStatusError) Error() string { return "hostd APIが要求を拒否しました" }

// HostCommand is scoped to Config.Host by the authenticated desired-image URL.
// Host, when supplied by an API extension, must agree with that scope.
type HostCommand struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Tag       string    `json:"tag"`
	ExpiresAt time.Time `json:"expiresAt"`
	Host      string    `json:"host,omitempty"`
}

func sameHostCommand(a, b HostCommand) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Tag == b.Tag && a.Host == b.Host && a.ExpiresAt.Equal(b.ExpiresAt)
}

func (r *Runtime) recordInstallOutcome(command HostCommand, reason string) {
	if command.Kind == "install" && reason != "done" {
		r.failure = &cycleFailure{"install", "other", reason}
	}
}

type CommandResult struct {
	ID     string    `json:"id"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type commandReceipt struct {
	Command HostCommand    `json:"command"`
	Result  *CommandResult `json:"result,omitempty"`
	Pending bool           `json:"pending"`
}

// Retain delivered IDs as well as pending results. A stale desired-image response
// must not execute a command again, even after another command has completed.
type commandReceipts map[string]*commandReceipt

func (r *Runtime) retryCommandResults(ctx context.Context) error {
	receipts, err := r.receipts()
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		if receipt == nil {
			return errors.New("指示の保存結果が不正です")
		}
		if err := r.sendCommandResult(ctx, receipts, receipt); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) receipts() (commandReceipts, error) {
	out := commandReceipts{}
	if err := r.load("command-results.json", &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = commandReceipts{}
	}
	return out, nil
}

func (r *Runtime) sendCommandResult(ctx context.Context, receipts commandReceipts, receipt *commandReceipt) error {
	if !receipt.Pending {
		return nil
	}
	if receipt.Result == nil {
		return errors.New("未送信の指示結果がありません")
	}
	err := r.api(ctx, "POST", "hostd-health", map[string]any{"commandResult": receipt.Result}, nil)
	var status *apiStatusError
	if err != nil && !(errors.As(err, &status) && status.Status == 400) {
		return err
	}
	// 400 includes replaced commands and already consumed IDs. Do not change
	// the timestamp or reason and try a different result for the same ID.
	receipt.Pending = false
	if err != nil {
		receipt.Result = nil
	}
	return r.save("command-results.json", receipts)
}

func (r *Runtime) commandOutcome(ctx context.Context, s *UpdateState, reason string) error {
	s.Phase = "command-reporting"
	s.CommandResult = &CommandResult{ID: s.Command.ID, OK: reason == "done", Reason: reason, At: r.Now().UTC()}
	if err := r.save("state.json", s); err != nil {
		return err
	}
	return r.finishCommand(ctx, s)
}

func (r *Runtime) finishCommand(ctx context.Context, s *UpdateState) error {
	if s.CommandResult == nil {
		return errors.New("指示の保存結果がありません")
	}
	r.recordInstallOutcome(*s.Command, s.CommandResult.Reason)
	receipts, err := r.receipts()
	if err != nil {
		return err
	}
	receipt := receipts[s.Command.ID]
	if receipt == nil {
		receipt = &commandReceipt{Command: *s.Command, Result: s.CommandResult, Pending: true}
		receipts[s.Command.ID] = receipt
		if err := r.save("command-results.json", receipts); err != nil {
			return err
		}
	}
	// Report the result before releasing acquisition, but still release it when
	// the result endpoint is temporarily down.
	reportErr := r.sendCommandResult(ctx, receipts, receipt)
	// DrainAttempt also covers a PUT whose successful response was lost.
	if s.DrainSet || s.DrainAttempt {
		if err := r.api(ctx, "DELETE", "drain", nil, nil); err != nil {
			return err
		}
		s.DrainSet, s.DrainAttempt = false, false
		if err := r.save("state.json", s); err != nil {
			return err
		}
	}
	if reportErr != nil {
		return reportErr
	}
	return r.save("state.json", nil)
}
