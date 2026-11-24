package execution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
	workerruntime "issue-desk-worker/internal/runtime"
)

type settingsSchedulerAPI struct {
	mu       sync.Mutex
	snapshot intake.ConfigSnapshot
	err      error
	statuses []intake.HostStatus
}

func (a *settingsSchedulerAPI) Config(context.Context) (intake.ConfigSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshot, a.err
}
func (a *settingsSchedulerAPI) Heartbeat(_ context.Context, s intake.HostStatus) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.statuses = append(a.statuses, s)
	return nil
}
func (*settingsSchedulerAPI) Next(context.Context, int) (intake.Assignment, error) {
	return intake.Assignment{}, nil
}
func (*settingsSchedulerAPI) Renew(context.Context, int64, int64) (intake.Assignment, error) {
	return intake.Assignment{}, nil
}
func (*settingsSchedulerAPI) State(context.Context, int64, int64, intake.State) error { return nil }
func (a *settingsSchedulerAPI) set(s intake.ConfigSnapshot, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.snapshot = s
	a.err = err
}
func (a *settingsSchedulerAPI) last() intake.HostStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statuses[len(a.statuses)-1]
}

type blockedHeartbeatAPI struct {
	*settingsSchedulerAPI
	calls         atomic.Int32
	started       chan struct{}
	release       chan struct{}
	fetched       chan struct{}
	seen          chan string
	polled        chan struct{}
	refreshedPoll chan struct{}
	once          sync.Once
}

func (a *blockedHeartbeatAPI) Config(ctx context.Context) (intake.ConfigSnapshot, error) {
	s, err := a.settingsSchedulerAPI.Config(ctx)
	if s.SettingsVersion == "s2" {
		a.once.Do(func() { close(a.fetched) })
	}
	return s, err
}

func (a *blockedHeartbeatAPI) Next(context.Context, int) (intake.Assignment, error) {
	select {
	case <-a.fetched:
		select {
		case a.refreshedPoll <- struct{}{}:
		default:
		}
	default:
	}
	select {
	case a.polled <- struct{}{}:
	default:
	}
	return intake.Assignment{}, nil
}

func (a *blockedHeartbeatAPI) Heartbeat(ctx context.Context, s intake.HostStatus) error {
	if a.calls.Add(1) == 2 {
		close(a.started)
		select {
		case <-a.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := a.settingsSchedulerAPI.Heartbeat(ctx, s)
	a.seen <- s.Checks["settingsVersion"].(string)
	return err
}

func TestSettingsRefreshWaitsForInFlightHeartbeat(t *testing.T) {
	cfg := Configuration{Host: "worker", Version: "c", Fleet: Fleet{Git: "git", Codex: "codex", GiteaURL: "https://example.invalid", RuntimeDir: "/tmp/worker", WorkerTokenFile: "worker", Model: "model"}}
	old := intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "c", SettingsVersion: "s1", Config: encode(cfg.Fleet), Settings: encode(map[string]any{})}
	e := &Executor{Config: cfg, ready: true}
	if err := e.InitializeSettings(old); err != nil {
		t.Fatal(err)
	}
	api := &blockedHeartbeatAPI{settingsSchedulerAPI: &settingsSchedulerAPI{snapshot: old}, started: make(chan struct{}), release: make(chan struct{}), fetched: make(chan struct{}), seen: make(chan string, 10), polled: make(chan struct{}, 10), refreshedPoll: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var now atomic.Int64
	var clockMu sync.Mutex
	now.Store(time.Now().UnixNano())
	wake := make(chan struct{}, 1)
	runner := workerruntime.Runner{API: api, Executor: e, Capacity: 1, ConfigurationVersion: "c", PollInterval: time.Hour, HeartbeatInterval: 10 * time.Millisecond, Wake: wake, Now: func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return time.Unix(0, now.Load())
	}}
	go func() { done <- runner.Run(ctx) }()
	<-api.started
	<-api.polled
	newer := old
	newer.SettingsVersion = "s2"
	api.set(newer, nil)
	clockMu.Lock()
	now.Add(int64(61 * time.Second))
	wake <- struct{}{}
	clockMu.Unlock()
	<-api.fetched
	<-api.refreshedPoll // The scheduling loop continues after the refresh while the heartbeat is blocked.
	if e.SettingsVersion() != "s1" {
		t.Fatal("published settings before earlier heartbeat completed")
	}
	close(api.release)
	for version := range api.seen {
		if version == "s2" {
			break
		}
	}
	cancel()
	<-done
}

func TestSchedulerSharesValidatedSettingsWithHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Configuration{Host: "worker", Version: "c", Fleet: Fleet{Git: "git", Codex: "codex", GiteaURL: "https://example.invalid", RuntimeDir: "/tmp/worker", WorkerTokenFile: "worker", Model: "model"}}
		snap := intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "c", SettingsVersion: "s1", Config: encode(cfg.Fleet), Settings: encode(map[string]any{})}
		e := &Executor{Config: cfg, ready: true}
		if err := e.InitializeSettings(snap); err != nil {
			t.Fatal(err)
		}
		api := &settingsSchedulerAPI{snapshot: snap}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startChecks := map[string]any{"ok": true, "at": "startup", "items": []any{"original"}, "settingsVersion": "s1"}
		runner := workerruntime.Runner{API: api, Executor: e, Capacity: 1, ConfigurationVersion: "c", StartupChecks: startChecks, PollInterval: time.Hour, HeartbeatInterval: time.Second, Now: time.Now}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		check := func(version string) {
			t.Helper()
			s := api.last()
			if s.Checks["settingsVersion"] != version || s.Checks["ok"] != true || s.Checks["at"] != "startup" || s.Checks["items"].([]any)[0] != "original" {
				t.Fatalf("checks: %#v", s.Checks)
			}
		}
		check("s1")
		next := snap
		next.SettingsVersion = "s2"
		next.Settings = encode(map[string]any{"providerDisable.codex": map[string]any{"until": "2099-01-01T00:00:00Z"}})
		api.set(next, nil)
		time.Sleep(62 * time.Second)
		synctest.Wait()
		check("s2")
		current, err := e.operationalConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		provider, _, _, err := current.SettingsFor("codex", "", false)
		if err != nil || provider != "claude" {
			t.Fatalf("settings not applied: %s %v", provider, err)
		}
		bad := next
		bad.SettingsVersion = "s3"
		bad.Settings = []byte(`[]`)
		api.set(bad, nil)
		time.Sleep(62 * time.Second)
		synctest.Wait()
		check("s2")
		api.set(bad, errors.New("unavailable"))
		time.Sleep(62 * time.Second)
		synctest.Wait()
		check("s2")
		changed := next
		changed.SettingsVersion = "s4"
		changed.ConfigurationVersion = "c2"
		api.set(changed, nil)
		time.Sleep(62 * time.Second)
		synctest.Wait()
		err = <-done
		if err == nil || !strings.Contains(err.Error(), "再起動") {
			t.Fatalf("expected restart: %v", err)
		}
		check("s2")
		if startChecks["settingsVersion"] != "s1" {
			t.Fatal("startup checks mutated")
		}
	})
}

func TestAICallsUsePublishedSettingsSnapshot(t *testing.T) {
	e, r, d, tr := completionFixture(t)
	e.Config.Version = "c"
	snap := intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "c", SettingsVersion: "s1", Config: encode(e.Config.Fleet), Settings: encode(map[string]any{})}
	if err := e.InitializeSettings(snap); err != nil {
		t.Fatal(err)
	}
	tr.failConfig = true
	r.Session = "implementation-session"
	r.Provider = "codex"
	if _, err := e.reportCompletionRequest(context.Background(), d, r, errors.New("missing")); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	release := make(chan struct{})
	callDone := make(chan error, 1)
	e.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		started <- req.Settings.Provider
		<-release
		return providers.Answer{}, nil
	}
	inflight := *r
	go func() {
		_, err := e.callAIOnce(context.Background(), d, &inflight, "test", false, nil)
		callDone <- err
	}()
	if selected := <-started; selected != "codex" {
		t.Fatalf("initial call: %s", selected)
	}
	next := snap
	next.SettingsVersion = "s2"
	next.Settings = encode(map[string]any{"providerDisable.codex": map[string]any{"until": "2099-01-01T00:00:00Z"}})
	if err := e.ApplySettingsSnapshot(next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reportCompletionRequest(context.Background(), d, r, errors.New("missing")); err == nil {
		t.Fatal("report completion used stale settings")
	}
	close(release)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	if _, err := e.callAIOnce(context.Background(), d, r, "test", false, nil); err != nil {
		t.Fatal(err)
	}
	if selected := <-started; selected != "claude" {
		t.Fatalf("updated call: %s", selected)
	}
}

func TestUninitializedExecutorRetainsStartupVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		snap := intake.ConfigSnapshot{ConfigurationVersion: "c", SettingsVersion: "new"}
		api := &settingsSchedulerAPI{snapshot: snap}
		e := &Executor{ready: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var errorsSeen atomic.Int32
		r := workerruntime.Runner{API: api, Executor: e, Capacity: 1, ConfigurationVersion: "c", StartupChecks: map[string]any{"ok": true, "settingsVersion": "startup"}, PollInterval: time.Hour, HeartbeatInterval: time.Second, OnMaintenanceError: func(error) { errorsSeen.Add(1) }}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		synctest.Wait()
		if got := api.last().Checks["settingsVersion"]; got != "startup" {
			t.Fatalf("startup version: %v", got)
		}
		time.Sleep(62 * time.Second)
		synctest.Wait()
		if got := api.last().Checks["settingsVersion"]; got != "new" {
			t.Fatalf("refreshed version: %v", got)
		}
		if errorsSeen.Load() != 0 {
			t.Fatal("unexpected maintenance error")
		}
		cancel()
		<-done
	})
}

func TestUninitializedExecutorOmitsMissingStartupVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &settingsSchedulerAPI{snapshot: intake.ConfigSnapshot{ConfigurationVersion: "c", SettingsVersion: "new"}}
		ctx, cancel := context.WithCancel(context.Background())
		r := workerruntime.Runner{API: api, Executor: &Executor{ready: true}, Capacity: 1, StartupChecks: map[string]any{"ok": true}, PollInterval: time.Hour, HeartbeatInterval: time.Hour}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		synctest.Wait()
		if _, ok := api.last().Checks["settingsVersion"]; ok {
			t.Fatal("missing startup version was sent")
		}
		cancel()
		<-done
	})
}
