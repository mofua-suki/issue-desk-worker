package runtime

import (
	"context"
	"errors"
	"issue-desk-worker/internal/hostload"
	"issue-desk-worker/internal/intake"
	"testing"
	"time"
)

type hostChecksAPI struct {
	fakeAPI
	statuses  []intake.HostStatus
	nextCalls int
	stop      context.CancelFunc
}

func (a *hostChecksAPI) Config(context.Context) (intake.ConfigSnapshot, error) {
	return intake.ConfigSnapshot{SettingsVersion: "settings-8"}, nil
}
func (a *hostChecksAPI) Heartbeat(_ context.Context, s intake.HostStatus) error {
	a.statuses = append(a.statuses, s)
	return nil
}
func (a *hostChecksAPI) Next(context.Context, int) (intake.Assignment, error) {
	a.nextCalls++
	a.stop()
	return intake.Assignment{}, nil
}
func TestHeartbeatCarriesCanonicalChecksAndFailureStopsDispatch(t *testing.T) {
	for _, failed := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		a := &hostChecksAPI{stop: cancel}
		r := Runner{API: a, Executor: fakeExecutor{}, Capacity: 1, PollInterval: time.Hour}
		if failed {
			r.StartupChecks = map[string]any{"ok": false, "items": []any{map[string]any{"ok": false}}}
		}
		err := r.Run(ctx)
		cancel()
		if failed {
			if err == nil || len(a.statuses) != 0 || a.nextCalls != 0 {
				t.Fatalf("失敗時に取得/成功送信しました: %v %+v", err, a)
			}
			continue
		}
		if len(a.statuses) != 1 || a.nextCalls != 1 {
			t.Fatalf("送信回数: %+v", a)
		}
		s := a.statuses[0]
		if !s.Ready || s.Checks["ok"] != true || s.Checks["settingsVersion"] != "settings-8" || s.Checks["at"] == "" {
			t.Fatalf("%+v", s)
		}
		if rows, ok := s.Checks["items"].([]any); !ok || len(rows) != 1 {
			t.Fatal(s.Checks)
		}
	}
}

func TestHeartbeatStatusCarriesMetricsAndLivePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &hostChecksAPI{stop: cancel}
	load := map[string]float64{"cpuPercent": 0, "logicalCpuCount": 2, "freeMemoryMB": 0, "freeDiskMB": 4, "loadAverage1m": 1.5}
	cli := []map[string]any{{"cli": "codex", "current": "0.160.1", "expected": "0.160.1", "at": int64(1)}}
	r := Runner{API: a, Executor: fakeExecutor{}, Capacity: 3, Status: func() intake.HostStatus {
		return intake.HostStatus{Load: load, CLIVersions: cli, Publishing: 2, Capabilities: map[string]any{"platform": "linux", "providers": []string{"codex"}}, IntakeHold: map[string]any{"reasons": []any{map[string]any{"kind": "usage_limit"}}}}
	}}
	_ = r.Run(ctx)
	if len(a.statuses) != 1 {
		t.Fatal(a.statuses)
	}
	s := a.statuses[0]
	if s.Publishing != 2 || s.Implementing != 0 || !s.Busy || s.Load["cpuPercent"] != 0 || s.Load["freeMemoryMB"] != 0 || s.CLIVersions[0]["current"] != "0.160.1" || s.IntakeHold == nil {
		t.Fatalf("heartbeat13項目: %+v", s)
	}
}
func TestHostHoldStopsNextButAllowsReadinessHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	a := &hostChecksAPI{stop: cancel}
	r := Runner{API: a, Executor: fakeExecutor{}, Capacity: 1, CanAcquire: func() bool { return false }, PollInterval: time.Millisecond}
	_ = r.Run(ctx)
	if len(a.statuses) != 1 || a.nextCalls != 0 {
		t.Fatalf("host保留中に取得しました: %+v", a)
	}
}

func TestHeartbeatForwardsMeasuredGPULoad(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		fail      bool
		delay     bool
		wantGPU   bool
		gpu       float64
		wantVRAM  bool
		vram      float64
	}{
		{"measured zero", "0, 0", false, false, true, 0, true, 0},
		{"measured load", "42, N/A", false, false, true, 42, false, 0},
		{"free vram", "42, 1536", false, false, true, 42, true, 1536},
		{"unavailable", "", true, false, false, 0, false, 0},
		{"timeout", "", false, true, false, 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &hostChecksAPI{stop: cancel}
			release := make(chan struct{})
			defer close(release)
			meter := hostload.New(t.TempDir(), hostload.Source{GPU: func(context.Context) ([]byte, error) {
				if tc.delay {
					<-release
				}
				if tc.fail {
					return nil, errors.New("unavailable")
				}
				return []byte(tc.raw), nil
			}})
			r := Runner{API: a, Executor: fakeExecutor{}, Capacity: 1, Status: func() intake.HostStatus { return intake.HostStatus{Load: meter.Measure(time.Now())} }}
			if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(a.statuses) != 1 {
				t.Fatal(a.statuses)
			}
			load := a.statuses[0].Load
			got, ok := load["gpuPercent"]
			if ok != tc.wantGPU || (ok && got != tc.gpu) {
				t.Fatal(load)
			}
			vram, ok := load["freeVramMB"]
			if ok != tc.wantVRAM || (ok && vram != tc.vram) {
				t.Fatal(load)
			}
			if _, ok := load["logicalCpuCount"]; !ok {
				t.Fatal(load)
			}
		})
	}
}
