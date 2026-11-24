package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"time"
)

type providerHold struct {
	LimitKind string `json:"limitKind,omitempty"`
	Until     int64  `json:"until"`
	Source    string `json:"source"`
	Since     string `json:"since"`
	Summary   string `json:"summary"`
}
type environmentHold struct {
	Since  string `json:"since"`
	Reason string `json:"reason"`
}
type hostHolds struct {
	Version     int                     `json:"version"`
	Usage       map[string]providerHold `json:"usageLimits"`
	Environment *environmentHold        `json:"environmentHold"`
	APIKeyHold  *apiKeyHold             `json:"claudeApiKeyHold,omitempty"`
}

type apiKeyHold struct {
	Until  int64  `json:"until"`
	Since  string `json:"since"`
	Reason string `json:"reason"`
}

func (e *Executor) holdFile() string {
	return filepath.Join(e.Config.RuntimeDir, "control", "host-holds.json")
}
func (e *Executor) saveHoldsLocked() error {
	if e.holds.Version == 0 {
		e.holds.Version = 1
	}
	dir := filepath.Dir(e.holdFile())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return atomicfile.Write(e.holdFile(), ".host-holds-", 0600, true, encode(e.holds), nil)
}

// A missing file means no live holds: usage limits are detected again and the environment is checked at startup.
func (e *Executor) loadHostHolds() error {
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	e.holds = hostHolds{Version: 1, Usage: map[string]providerHold{}}
	raw, err := os.ReadFile(e.holdFile())
	if err == nil {
		if len(raw) > 65536 || json.Unmarshal(raw, &e.holds) != nil || e.holds.Version != 1 {
			return errors.New("ホスト保留journalを読めません")
		}
		return e.validateHoldsLocked()
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("ホスト保留journalを読めません")
	}
	return e.saveHoldsLocked()
}

func (e *Executor) validateHoldsLocked() error {
	if e.holds.Usage == nil {
		e.holds.Usage = map[string]providerHold{}
	}
	for p, h := range e.holds.Usage {
		if p != "codex" && p != "claude" && p != "antigravity" || h.Until < 0 || h.Until > 8640000000000000 {
			return errors.New("利用上限の記録が不正です")
		}
		if h.Until > 0 {
			if _, err := time.Parse(time.RFC3339Nano, h.Since); err != nil {
				return cause.New("利用上限の開始時刻が不正です", err)
			}
		}
		h.Summary = clip(e.redact(h.Summary), 200)
		h.Source = clip(e.redact(h.Source), 50)
		e.holds.Usage[p] = h
	}
	if h := e.holds.Environment; h != nil {
		if _, err := time.Parse(time.RFC3339Nano, h.Since); err != nil {
			return cause.New("環境保留の開始時刻が不正です", err)
		}
		h.Reason = clip(e.redact(h.Reason), 200)
	}
	if h := e.holds.APIKeyHold; h != nil {
		if h.Until <= 0 || h.Until > 8640000000000000 || h.Reason == "" {
			return errors.New("APIキー保留の記録が不正です")
		}
		if _, err := time.Parse(time.RFC3339Nano, h.Since); err != nil {
			return cause.New("APIキー保留の開始時刻が不正です", err)
		}
		h.Reason = clip(e.redact(h.Reason), 200)
	}
	return nil
}
func (e *Executor) clearEnvironmentHold() error {
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	if e.holds.Environment == nil {
		return nil
	}
	e.holds.Environment = nil
	return e.saveHoldsLocked()
}

// accessDeniedError reports whether err is evidence that the host refused access: a test stopped by
// an access outside its work tree, or a file operation refused by permission.
func accessDeniedError(err error) bool {
	var pathErr *os.PathError
	var testErr testEnvironmentError
	return errors.As(err, &testErr) || errors.As(err, &pathErr) && errors.Is(err, os.ErrPermission)
}
func (e *Executor) recordEnvironmentHold(err error) error {
	if !accessDeniedError(err) {
		return nil
	}
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	if e.holds.Environment != nil {
		return nil
	}
	e.holds.Environment = &environmentHold{Since: time.Now().UTC().Format(time.RFC3339Nano), Reason: clip(e.redact(err.Error()), 200)}
	return e.saveHoldsLocked()
}
func (e *Executor) recordProviderHold(rProvider string, review bool, err error) error {
	var limited *providers.UsageLimit
	if review || !errors.As(err, &limited) {
		return nil
	}
	if rProvider != "codex" && rProvider != "claude" && rProvider != "antigravity" {
		return errors.New("利用上限の実行先が不正です")
	}
	now := time.Now()
	until := limited.Until
	source := limited.Source
	if !until.After(now) || until.After(now.Add(7*24*time.Hour)) {
		until = now.Add(30 * time.Minute)
		source = "default"
	}
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	if e.holds.Usage == nil {
		e.holds.Usage = map[string]providerHold{}
	}
	old := e.holds.Usage[rProvider]
	since := now.UTC().Format(time.RFC3339Nano)
	if old.Until > now.UnixMilli() {
		since = old.Since
		if old.Until > until.UnixMilli() {
			until = time.UnixMilli(old.Until)
			source = old.Source
		}
	}
	e.holds.Usage[rProvider] = providerHold{Until: until.UnixMilli(), Source: source, Since: since, Summary: "実行先の利用上限に達しました", LimitKind: limited.LimitKind}
	return e.saveHoldsLocked()
}

// recordAPIKeyHold never changes the subscription hold.
func (e *Executor) recordAPIKeyHold(err error) error {
	var limited *providers.UsageLimit
	if !errors.As(err, &limited) {
		return nil
	}
	now := time.Now()
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	until := e.holds.Usage["claude"].Until
	if until <= now.UnixMilli() {
		return nil
	}
	if max := now.Add(7 * 24 * time.Hour).UnixMilli(); until > max {
		until = max
	}
	e.holds.APIKeyHold = &apiKeyHold{Until: until, Since: now.UTC().Format(time.RFC3339Nano), Reason: "APIキーの利用上限に達しました"}
	return e.saveHoldsLocked()
}
func (e *Executor) intakeHold(at time.Time) map[string]any {
	return e.intakeHoldWithKeyReader(at, e.claudeAPIKey)
}

func (e *Executor) intakeHoldWithKeyReader(at time.Time, readKey func() string) map[string]any {
	e.holdsMu.Lock()
	h := e.holds.Usage["claude"]
	needKey := h.Until > at.UnixMilli() && (e.holds.APIKeyHold == nil || e.holds.APIKeyHold.Until <= at.UnixMilli())
	e.holdsMu.Unlock()
	keyAvailable := needKey && readKey() != ""
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	var reasons []any
	if h := e.holds.Environment; h != nil {
		reasons = append(reasons, map[string]any{"kind": "environment_hold", "since": h.Since, "detail": h.Reason})
	}
	for _, p := range []string{"codex", "claude", "antigravity"} {
		h, ok := e.holds.Usage[p]
		if !ok || h.Until <= at.UnixMilli() {
			continue
		}
		if p == "claude" && (e.holds.APIKeyHold == nil || e.holds.APIKeyHold.Until <= at.UnixMilli()) && keyAvailable {
			continue
		}
		reasons = append(reasons, map[string]any{"kind": "usage_limit", "provider": p, "since": h.Since, "until": time.UnixMilli(h.Until).UTC().Format(time.RFC3339Nano), "source": h.Source, "detail": h.Summary, "limitKind": holdKind(h)})
	}
	if len(reasons) == 0 {
		return nil
	}
	return map[string]any{"reasons": reasons}
}
func (e *Executor) CanAcquire() bool { return e.intakeHold(time.Now()) == nil }
func (e *Executor) RefreshHostEnvironment(ctx context.Context) error {
	e.holdsMu.Lock()
	held := e.holds.Environment != nil
	e.holdsMu.Unlock()
	if !held {
		return nil
	}
	return e.VerifyIsolation(ctx)
}

func (e *Executor) providerHoldMarker(provider string, generation int) json.RawMessage {
	e.holdsMu.Lock()
	defer e.holdsMu.Unlock()
	h, ok := e.holds.Usage[provider]
	if !ok {
		return nil
	}
	return encode(map[string]any{"provider": provider, "resetAt": time.UnixMilli(h.Until).UTC().Format(time.RFC3339Nano), "source": h.Source, "at": time.Now().UTC().Format(time.RFC3339Nano), "generation": generation, "autoRetry": false})
}

func holdKind(h providerHold) string {
	if h.LimitKind == "seven_day" || h.LimitKind == "five_hour" {
		return h.LimitKind
	}
	return providers.QuotaKind(h.Summary)
}
