package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderHoldPersistsOnlyWorkAndKeepsRunMarker(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: root}}}
	if err := e.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(90 * time.Minute)
	limited := &providers.UsageLimit{Provider: "claude", Until: until, Source: "cli"}
	if err := e.recordProviderHold("claude", true, limited); err != nil {
		t.Fatal(err)
	}
	if !e.CanAcquire() {
		t.Fatal("レビューだけで新実装を止めました")
	}
	if err := e.recordProviderHold("claude", false, limited); err != nil {
		t.Fatal(err)
	}
	if e.CanAcquire() {
		t.Fatal("実装quotaを見逃しました")
	}
	raw := e.providerHoldMarker("claude", 3)
	var marker map[string]any
	json.Unmarshal(raw, &marker)
	if marker["autoRetry"] != false || marker["generation"] != float64(3) || marker["provider"] != "claude" {
		t.Fatal(marker)
	}
	again := &Executor{Config: e.Config}
	if err := again.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	if again.CanAcquire() {
		t.Fatal("再起動でquotaが消えました")
	}
}

func TestIntakeHoldReadsKeyOutsideLockAndRechecksHold(t *testing.T) {
	now := time.Now()
	e := &Executor{holds: hostHolds{Usage: map[string]providerHold{"claude": {Until: now.Add(time.Hour).UnixMilli()}}}}
	reading, resume := make(chan struct{}), make(chan struct{})
	result := make(chan map[string]any, 1)
	go func() {
		result <- e.intakeHoldWithKeyReader(now, func() string {
			close(reading)
			<-resume
			return "key"
		})
	}()
	<-reading
	locked := make(chan struct{})
	go func() {
		e.holdsMu.Lock()
		e.holds.APIKeyHold = &apiKeyHold{Until: now.Add(time.Hour).UnixMilli()}
		e.holdsMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("key read held holdsMu")
	}
	close(resume)
	if <-result == nil {
		t.Fatal("new API key hold was ignored after key read")
	}
}

func TestHeartbeatPublishingCountsOnlyActiveStages(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store, controls: jobControls{byID: map[int64]*control{}}}
	for i, stage := range []string{"implement", "test", "review", "publish", "merge", "deploy", "done"} {
		id := int64(i + 1)
		if err := store.Save(&journal.Record{ID: id, Stage: stage}); err != nil {
			t.Fatal(err)
		}
		if stage != "done" {
			e.controls.byID[id] = &control{}
		}
	}
	s := e.HeartbeatStatus()
	if s.Publishing != 5 {
		t.Fatalf("公開件数=%d", s.Publishing)
	}
	delete(e.controls.byID, 2)
	if e.HeartbeatStatus().Publishing != 4 {
		t.Fatal("終了済みの工程を数えました")
	}
}
func TestEnvironmentHoldUsesActualPermissionAndRecoversAfterIsolation(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	codex := filepath.Join(root, "codex")
	if err = os.WriteFile(codex, []byte("#!/bin/sh\necho ISOLATION_OK\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, Executable: "/unused", Config: Configuration{Fleet: Fleet{RuntimeDir: root, Codex: codex, WorkerTokenFile: filepath.Join(root, "private/token")}}}
	if err = e.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	if err = e.recordEnvironmentHold(errors.New("AI says permission denied")); err != nil || !e.CanAcquire() {
		t.Fatal("任意文字列でglobal保留を作りました", err)
	}
	if err = e.recordEnvironmentHold(&os.PathError{Op: "mkdir", Path: filepath.Join(root, "workspaces"), Err: os.ErrPermission}); err != nil || e.CanAcquire() {
		t.Fatal("実権限失敗を見逃しました", err)
	}
	if err = e.VerifyIsolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.CanAcquire() {
		t.Fatal("隔離再検査成功後も永久保留です")
	}
	again := &Executor{Config: e.Config}
	if err = again.loadHostHolds(); err != nil || !again.CanAcquire() {
		t.Fatal("解除を永続化しませんでした", err)
	}
}

// No host-holds.json and no old database: start empty, write the file, read it back.
func TestHostHoldsStartEmptyAndSurviveRestart(t *testing.T) {
	cfg := Configuration{}
	cfg.RuntimeDir = t.TempDir()
	e := &Executor{Config: cfg}
	if err := e.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.holdFile()); err != nil {
		t.Fatal(err)
	}
	if len(e.holds.Usage) != 0 || e.holds.Environment != nil {
		t.Fatalf("holds: %+v", e.holds)
	}
	again := &Executor{Config: cfg}
	if err := again.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAPIKeyHoldAcquisitionAndPersistence(t *testing.T) {
	root := t.TempDir()
	cfg := Configuration{Fleet: Fleet{RuntimeDir: root, WorkerTokenFile: filepath.Join(root, "private", "token")}}
	if err := os.MkdirAll(filepath.Dir(cfg.WorkerTokenFile), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: cfg}
	if err := e.loadHostHolds(); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	limited := &providers.UsageLimit{Provider: "claude", Until: until, Source: "cli"}
	if err := e.recordProviderHold("claude", false, limited); err != nil {
		t.Fatal(err)
	}
	if e.CanAcquire() {
		t.Fatal("missing key bypassed hold")
	}
	keyPath := filepath.Join(filepath.Dir(cfg.WorkerTokenFile), "claude-api-key")
	if err := os.WriteFile(keyPath, []byte("sk-ant-"+"abcdefghijklmnopqrst"), 0600); err != nil {
		t.Fatal(err)
	}
	if !e.CanAcquire() || e.intakeHold(time.Now()) != nil {
		t.Fatal("valid key did not bypass hold")
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if e.CanAcquire() || e.intakeHold(time.Now()) == nil {
		t.Fatal("removed key did not restore hold")
	}
	if err := os.WriteFile(keyPath, []byte("sk-ant-"+"abcdefghijklmnopqrst"), 0600); err != nil {
		t.Fatal(err)
	}
	original := e.holds.Usage["claude"]
	if err := e.recordAPIKeyHold(limited); err != nil {
		t.Fatal(err)
	}
	if e.holds.Usage["claude"] != original || e.holds.APIKeyHold.Until != original.Until || e.CanAcquire() {
		t.Fatal("key hold did not preserve subscription hold")
	}
	again := &Executor{Config: cfg}
	if err := again.loadHostHolds(); err != nil || again.CanAcquire() {
		t.Fatal("key hold not restored", err)
	}
	if again.intakeHold(until.Add(time.Second)) != nil {
		t.Fatal("expired subscription still blocks")
	}
	again.holds.APIKeyHold = nil
	if err := again.recordProviderHold("codex", false, &providers.UsageLimit{Until: until, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if again.CanAcquire() {
		t.Fatal("codex hold bypassed")
	}
	again.holds.Usage["codex"] = providerHold{}
	if err := again.recordProviderHold("antigravity", false, &providers.UsageLimit{Until: until, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if again.CanAcquire() {
		t.Fatal("antigravity hold bypassed")
	}
	again.holds.Usage["antigravity"] = providerHold{}
	again.holds.Environment = &environmentHold{Since: time.Now().UTC().Format(time.RFC3339Nano), Reason: "test"}
	if again.CanAcquire() {
		t.Fatal("environment hold bypassed")
	}
}

func TestAPIKeyHoldValidationAndCap(t *testing.T) {
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: t.TempDir()}}}
	now := time.Now()
	e.holds.Usage = map[string]providerHold{"claude": {Until: now.Add(8 * 24 * time.Hour).UnixMilli()}}
	if err := e.recordAPIKeyHold(&providers.UsageLimit{}); err != nil {
		t.Fatal(err)
	}
	if e.holds.APIKeyHold.Until > time.Now().Add(7*24*time.Hour).UnixMilli() {
		t.Fatal("hold exceeds seven days")
	}
	for _, h := range []*apiKeyHold{{Until: 1, Since: "bad", Reason: "limit"}, {Until: 1, Since: now.Format(time.RFC3339Nano)}, {Until: -1, Since: now.Format(time.RFC3339Nano), Reason: "limit"}} {
		e.holds.Usage = nil
		e.holds.APIKeyHold = h
		if e.validateHoldsLocked() == nil {
			t.Fatal("invalid hold accepted")
		}
	}
}

func TestHostHoldsRedactSecretsOnReload(t *testing.T) {
	for _, prefix := range []string{"", "x", "9", "_"} {
		t.Run("prefix_"+prefix, func(t *testing.T) {
			cfg := Configuration{Fleet: Fleet{RuntimeDir: t.TempDir()}}
			e := &Executor{Config: cfg}
			secret := prefix + "sk-ant-" + strings.Repeat("a", 20)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			e.holds = hostHolds{Version: 1, Usage: map[string]providerHold{"claude": {Until: 1, Since: now, Summary: secret, Source: secret}}, Environment: &environmentHold{Since: now, Reason: secret}, APIKeyHold: &apiKeyHold{Until: 1, Since: now, Reason: secret}}
			if err := e.saveHoldsLocked(); err != nil {
				t.Fatal(err)
			}
			again := &Executor{Config: cfg}
			if err := again.loadHostHolds(); err != nil {
				t.Fatal(err)
			}
			h := again.holds.Usage["claude"]
			for _, got := range []string{h.Summary, h.Source, again.holds.Environment.Reason, again.holds.APIKeyHold.Reason} {
				if got != prefix+"[REDACTED]" {
					t.Fatalf("hold field = %q", got)
				}
			}
		})
	}
}
