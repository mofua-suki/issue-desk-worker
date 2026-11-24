package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

type transientExhausted struct{ cause error }

func (e *transientExhausted) Error() string {
	return fmt.Sprintf("%s\n一時的な失敗が続きました（呼び出し回数: 3回）", e.cause)
}
func (e *transientExhausted) Unwrap() error { return e.cause }

// Each callback must persist its usage and clear Transient on post-call failures.
func (e *Executor) retryTransient(ctx context.Context, d Descriptor, r *journal.Record, call func() (providers.Answer, error)) (providers.Answer, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return providers.Answer{}, err
		}
		answer, err := call()
		var quota *providers.UsageLimit
		if err == nil || !answer.Transient || errors.As(err, &quota) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return answer, err
		}
		if ctx.Err() != nil {
			return answer, ctx.Err()
		}
		if attempt == 3 {
			return answer, &transientExhausted{cause: err}
		}
		if err := e.flushUsage(ctx, r); err != nil {
			return providers.Answer{}, err
		}
		wait := e.transientWait
		if wait == nil {
			wait = waitTransient
		}
		if err := wait(ctx, time.Duration(attempt)*5*time.Second); err != nil {
			return answer, err
		}
		if err := ctx.Err(); err != nil {
			return answer, err
		}
		if err := e.checkImplementationUsage(ctx, d, r, true, true); err != nil {
			return providers.Answer{}, fmt.Errorf("再実行前の使用量確認で停止しました: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return answer, err
		}
	}
}

func waitTransient(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
