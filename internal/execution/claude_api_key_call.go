package execution

import (
	"context"
	"time"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func (e *Executor) claudeAPIKeyForCall(provider string) string {
	return e.claudeAPIKeyForCallWithReader(provider, e.claudeAPIKey)
}

// Do not hold the shared hold lock during file IO. Both holds and the clock
// are checked again after the read, including changes made by another job.
func (e *Executor) claudeAPIKeyForCallWithReader(provider string, readKey func() string) string {
	if provider != "claude" {
		return ""
	}
	available := func() bool {
		e.holdsMu.Lock()
		defer e.holdsMu.Unlock()
		now := time.Now().UnixMilli()
		return e.holds.Usage["claude"].Until > now && (e.holds.APIKeyHold == nil || e.holds.APIKeyHold.Until <= now)
	}
	if !available() {
		return ""
	}
	key := readKey()
	if !available() {
		return ""
	}
	return key
}

func (e *Executor) recordCallHold(request providers.Request, callErr error) error {
	if request.APIKey != "" {
		return e.recordAPIKeyHold(callErr)
	}
	return e.recordProviderHold(request.Settings.Provider, request.Review, callErr)
}

// Continue after a subscription limit under the same usage decision rules.
// A new report conversation has no implementation session to resume.
func (e *Executor) retryClaudeWithAPIKey(ctx context.Context, d Descriptor, r *journal.Record, request providers.Request, step string, original providers.Answer, originalErr error) (providers.Answer, error) {
	if err := ctx.Err(); err != nil {
		return original, err
	}
	if (!d.reportOnly && r.Session == "") || e.claudeAPIKeyForCall("claude") == "" {
		return original, originalErr
	}
	// Apply the existing continuation usage decision, without consuming a run
	// or manufacturing an admission. The subscription call is already saved.
	if err := e.flushUsage(ctx, r); err != nil {
		return original, err
	}
	if err := e.checkImplementationUsage(ctx, d, r, true, true); err != nil {
		return original, err
	}
	if !d.reportOnly {
		request.Session = r.Session
	}
	request.APIKey = e.claudeAPIKeyForCall("claude")
	if request.APIKey == "" {
		return original, originalErr
	}
	if err := ctx.Err(); err != nil {
		return original, err
	}
	answer, callErr := e.Call(ctx, request)
	if err := e.recordCallHold(request, callErr); err != nil {
		return providers.Answer{}, err
	}
	if err := e.recordEnvironmentHold(callErr); err != nil {
		return providers.Answer{}, err
	}
	if !d.reportOnly && answer.Session != "" {
		r.Session = answer.Session
	}
	originalHold := r.ProviderHold
	if callErr == nil {
		r.ProviderHold = nil
	}
	if err := e.recordUsage(r, d, request.Settings, step, answer, callUsageSize(ctx, request.Prompt, false)); err != nil {
		r.ProviderHold = originalHold
		return providers.Answer{}, err
	}
	return answer, callErr
}
