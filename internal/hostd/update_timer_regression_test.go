package hostd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImmediateUpdateCompletesNormalAndLazy(t *testing.T) {
	for _, kind := range []string{"normal", "lazy"} {
		t.Run(kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Target.Kind = kind
			got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
			if err != nil || got.Reason != "success" || f.Starts != 1 || f.Restores != 0 || f.Drain != nil {
				t.Fatalf("result=%+v err=%v starts=%d", got, err, f.Starts)
			}
		})
	}
}

func TestImmediateLockPreventsSecondRuntime(t *testing.T) {
	f := newUpdateFixture(t)
	if err := os.WriteFile(filepath.Join(f.Runtime.Config.Home, "token"), []byte("idh_"+strings.Repeat("a", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := New(f.Runtime.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := New(f.Runtime.Config)
	if second != nil || !errors.Is(err, ErrLocked) || len(f.Events) != 0 || f.Starts != 0 {
		t.Fatalf("second=%v err=%v events=%v", second, err, f.Events)
	}
}

func TestImmediateBusyAndResume(t *testing.T) {
	f := newUpdateFixture(t)
	f.OnSnapshot = func(s *imageSnapshot) {
		if f.Drain != nil {
			one := 1
			s.Heartbeat.Publishing = &one
		}
	}
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(16*time.Second))
	if err != nil || got.Reason != "wait_timeout" || got.WaitReason != "busy" || got.Publishing != 1 || f.Starts != 0 || f.Drain == nil {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	f.OnSnapshot = nil
	got, err = f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if err != nil || got.Reason != "success" || f.Starts != 1 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestImmediateNewWorkReleasesDrain(t *testing.T) {
	f := newUpdateFixture(t)
	f.OnSnapshot = func(s *imageSnapshot) {
		if f.Drain != nil {
			one := 1
			s.Heartbeat.Implementing = &one
		}
	}
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if err != nil || got.Reason != "new_implementation" || f.Drain != nil || f.Starts != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestImmediateSkipReasons(t *testing.T) {
	for _, reason := range []string{"no_delivery", "same_version", "pinned", "failed_tag"} {
		t.Run(reason, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.OnSnapshot = func(s *imageSnapshot) {
				switch reason {
				case "no_delivery":
					s.Desired = nil
				case "same_version":
					s.Desired.Tag = "aaaaaaaaaaaa"
				case "pinned":
					s.Pin = true
				case "failed_tag":
					s.FailedTags = []string{f.Target.Tag}
				}
			}
			got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
			if err != nil || got.Reason != reason || f.Starts != 0 || f.Drain != nil {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

func TestImmediateCancellationPreservesWaitingState(t *testing.T) {
	f := newUpdateFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.Runtime.Sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	got, err := f.Runtime.ImmediateUpdate(ctx, f.Now.Add(time.Minute))
	if !errors.Is(err, context.Canceled) || got != (ImmediateUpdateResult{}) || f.Drain == nil {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	f.Now = f.Now.Add(time.Minute)
	if err := f.Runtime.updateCycle(context.Background()); err != nil || f.Starts != 1 {
		t.Fatalf("resume err=%v starts=%d", err, f.Starts)
	}
}

func TestImmediateFailureAndRecovery(t *testing.T) {
	for _, reason := range []string{"digest_mismatch", "ready_failed"} {
		t.Run(reason, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.WrongRegistry = reason == "digest_mismatch"
			if reason == "ready_failed" {
				f.OnSnapshot = func(s *imageSnapshot) {
					if f.Starts > 0 {
						s.Heartbeat.Ready = false
					}
				}
			}
			got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
			if err != nil || got.Reason != reason || f.Restores != 1 || len(f.Reported) != 1 || f.Reported[0] != "failed" {
				t.Fatalf("result=%+v err=%v restores=%d", got, err, f.Restores)
			}
		})
	}
}

func TestImmediateReadyWaitInterruptedAndResumed(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancel"}[cancelWait], func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Runtime.Config.ReadySeconds = 60
			f.OnSnapshot = func(s *imageSnapshot) {
				if f.Starts > 0 {
					s.Heartbeat.Ready = false
					two := 2
					s.Heartbeat.Publishing = &two
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelWait {
				f.Runtime.Sleep = func(_ context.Context, d time.Duration) error {
					if f.Starts > 0 {
						cancel()
						return ctx.Err()
					}
					f.Now = f.Now.Add(d)
					return nil
				}
			}
			got, err := f.Runtime.ImmediateUpdate(ctx, f.Now.Add(12*time.Second))
			if cancelWait && (!errors.Is(err, context.Canceled) || got != (ImmediateUpdateResult{})) || !cancelWait && (err != nil || got.Reason != "wait_timeout" || got.WaitReason != "heartbeat_wait" || !got.CountsKnown || got.Publishing != 2) {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			var state *UpdateState
			if err := f.Runtime.load("state.json", &state); err != nil || state == nil || state.Phase != "readying" || f.Restores != 0 || len(f.Reported) != 0 || f.Starts != 1 {
				t.Fatalf("state=%+v err=%v restores=%d reported=%v starts=%d", state, err, f.Restores, f.Reported, f.Starts)
			}
			f.OnSnapshot = nil
			f.Runtime.Sleep = func(_ context.Context, d time.Duration) error { f.Now = f.Now.Add(d); return nil }
			if err := f.Runtime.updateCycle(context.Background()); err != nil || f.Starts != 1 || f.Restores != 0 || len(f.Reported) != 1 || f.Reported[0] != "success" || f.Drain != nil {
				t.Fatalf("resume err=%v starts=%d restores=%d reported=%v drain=%v", err, f.Starts, f.Restores, f.Reported, f.Drain)
			}
		})
	}
}

func TestImmediateReadyDeadlineSurvivesRepeatedTimeouts(t *testing.T) {
	f := newUpdateFixture(t)
	f.Runtime.Config.ReadySeconds = 20
	f.OnSnapshot = func(s *imageSnapshot) {
		if f.Starts > 0 {
			s.Heartbeat.Ready = false
		}
	}
	for i := 0; i < 5 && f.Restores == 0; i++ {
		_, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(6*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.Restores != 1 || len(f.Reported) != 1 || f.Reported[0] != "failed" {
		t.Fatalf("deadline restarted: restores=%d reported=%v", f.Restores, f.Reported)
	}
}

func TestReadyResumeAfterDeadlineChecksLatestHeartbeat(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "not_ready"}[ready], func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Runtime.Config.ReadySeconds = 60
			f.OnSnapshot = func(s *imageSnapshot) {
				if f.Starts > 0 {
					s.Heartbeat.Ready = false
				}
			}
			got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(12*time.Second))
			if err != nil || got.Reason != "wait_timeout" {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			var state *UpdateState
			if err := f.Runtime.load("state.json", &state); err != nil || state == nil || state.Phase != "readying" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			f.Now = state.ReadyDeadline.Add(time.Minute)
			f.OnSnapshot = func(s *imageSnapshot) { s.Heartbeat.Ready = ready }
			before := len(f.Events)
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantRestores := "failed", 1
			if ready {
				wantStatus, wantRestores = "success", 0
			}
			if len(f.Events) == before || f.Starts != 1 || f.Restores != wantRestores || len(f.Reported) != 1 || f.Reported[0] != wantStatus {
				t.Fatalf("events=%v starts=%d restores=%d reported=%v", f.Events[before:], f.Starts, f.Restores, f.Reported)
			}
		})
	}
}

func TestImmediateReadyReportErrorAndRetry(t *testing.T) {
	f := newUpdateFixture(t)
	f.FailReport = true
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if err == nil || got != (ImmediateUpdateResult{}) {
		t.Fatalf("expected report error and empty result: result=%+v err=%v", got, err)
	}
	got, err = f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if err != nil || got.Reason != "report_retried" || got.ReportStatus != "success" || f.Starts != 1 || len(f.Reported) != 1 || f.Reported[0] != "success" {
		t.Fatalf("result=%+v err=%v starts=%d reported=%v", got, err, f.Starts, f.Reported)
	}
}

func TestImmediateInterruptedUpdateAndReportRetry(t *testing.T) {
	f := newUpdateFixture(t)
	if err := f.Runtime.save("state.json", &UpdateState{Phase: "updating", Target: f.Target}); err != nil {
		t.Fatal(err)
	}
	f.FailReport = true
	if _, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute)); err == nil {
		t.Fatal("missing report failure")
	}
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if err != nil || got.Reason != "report_retried" || got.ReportStatus != "failed" || f.Starts != 0 || f.Restores != 1 || len(f.Reported) != 1 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestNormalCycleDrainDeadlineAndIdleOrder(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "idle"}[idle], func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Runtime.Config.WaitMinutes = 1
			if err := f.Runtime.Cycle(context.Background()); err != nil || f.Starts != 0 || f.Drain == nil {
				t.Fatalf("first cycle err=%v", err)
			}
			f.Now = f.Now.Add(2 * time.Minute)
			if !idle {
				f.OnSnapshot = func(s *imageSnapshot) { one := 1; s.Heartbeat.Publishing = &one }
			}
			if err := f.Runtime.Cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Drain != nil || (f.Starts == 1) != idle || len(f.Cycles) != 2 {
				t.Fatalf("idle=%v starts=%d drain=%v cycles=%d", idle, f.Starts, f.Drain, len(f.Cycles))
			}
		})
	}
}

func TestLazyWaitResumesWithoutRestartingClock(t *testing.T) {
	f := newUpdateFixture(t)
	f.Target.Kind = "lazy"
	f.OnSnapshot = func(s *imageSnapshot) { one := 1; s.Heartbeat.Publishing = &one }
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(10*time.Second))
	if err != nil || got.WaitReason != "lazy_wait" || f.Drain != nil {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	f.Now = f.Now.Add(12 * time.Hour)
	if err := f.Runtime.updateCycle(context.Background()); err != nil || f.Drain == nil {
		t.Fatalf("lazy clock restarted: %v", err)
	}
}

func TestImmediateDrainTimeoutReleases(t *testing.T) {
	f := newUpdateFixture(t)
	f.Runtime.Config.WaitMinutes = 1
	f.OnSnapshot = func(s *imageSnapshot) {
		if f.Drain != nil {
			one := 1
			s.Heartbeat.Publishing = &one
		}
	}
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(2*time.Minute))
	if err != nil || got.Reason != "drain_timeout" || f.Drain != nil || f.Starts != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

// Corrupt stage state makes reaching each real stage observable without adding
// test-only hooks to production. A drained worker must defer all three stages.
func TestNormalCycleOtherStagesAndDrainIsolation(t *testing.T) {
	for _, file := range []string{"apps-state.json", "reader-state.json", "live-state.json"} {
		t.Run(file, func(t *testing.T) {
			f := newUpdateFixture(t)
			if err := os.WriteFile(filepath.Join(f.Runtime.Config.Home, file), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.Runtime.Cycle(context.Background()); err != nil {
				t.Fatalf("draining entered %s: %v", file, err)
			}
			f.Now = f.Now.Add(3 * time.Hour)
			f.OnSnapshot = func(s *imageSnapshot) { one := 1; s.Heartbeat.Publishing = &one }
			if err := f.Runtime.Cycle(context.Background()); err == nil || f.Drain != nil {
				t.Fatalf("deadline did not release and reach %s: %v", file, err)
			}
		})
	}
}

func TestImmediateProgressReportsBusyBeforeDeadline(t *testing.T) {
	f := newUpdateFixture(t)
	f.OnSnapshot = func(s *imageSnapshot) {
		if f.Drain != nil {
			one := 1
			s.Heartbeat.Publishing = &one
		}
	}
	var observations []ImmediateUpdateResult
	got, err := f.Runtime.ImmediateUpdateWithProgress(context.Background(), f.Now.Add(12*time.Second), func(r ImmediateUpdateResult) { observations = append(observations, r) })
	if err != nil || got.Reason != "wait_timeout" || len(observations) < 2 || observations[0].Reason != "heartbeat_wait" || observations[1].Reason != "busy" || observations[1].Publishing != 1 || f.Starts != 0 || f.Drain == nil {
		t.Fatalf("result=%+v observations=%+v err=%v", got, observations, err)
	}
}

func TestImmediateWaitErrorReturnsEmptyResult(t *testing.T) {
	f := newUpdateFixture(t)
	sleepErr := errors.New("sleep failed")
	f.Runtime.Sleep = func(context.Context, time.Duration) error { return sleepErr }
	got, err := f.Runtime.ImmediateUpdate(context.Background(), f.Now.Add(time.Minute))
	if !errors.Is(err, sleepErr) || got != (ImmediateUpdateResult{}) {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestNormalCycleInvocationCounts(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		cycles int
	}{{"normal", 2}, {"lazy", 3}} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Target.Kind = tc.kind
			f.Runtime.Sleep = func(context.Context, time.Duration) error {
				t.Error("normal cycle waited for heartbeat")
				return errors.New("unexpected wait")
			}
			for i := 1; i <= tc.cycles; i++ {
				if err := f.Runtime.Cycle(context.Background()); err != nil {
					t.Fatal(err)
				}
				if (f.Starts == 1) != (i == tc.cycles) {
					t.Fatalf("cycle=%d starts=%d", i, f.Starts)
				}
				f.Now = f.Now.Add(time.Minute)
			}
			if len(f.Reported) != 1 || f.Reported[0] != "success" || f.Drain != nil {
				t.Fatalf("reported=%v drain=%v", f.Reported, f.Drain)
			}
		})
	}
}

func TestImmediateWaitCounts(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		missing    bool
		reason     string
	}{
		{"lazy", "lazy", false, "lazy_wait"},
		{"lazy missing", "lazy", true, "lazy_wait"},
		{"heartbeat", "", false, "heartbeat_wait"},
		{"heartbeat missing", "", true, "heartbeat_wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t)
			f.Target.Kind = tc.kind
			f.OnSnapshot = func(s *imageSnapshot) {
				one, two := 1, 2
				s.Heartbeat.Implementing = &one
				s.Heartbeat.Publishing = &two
				if tc.missing {
					s.Heartbeat.Publishing = nil
				}
			}
			var observed ImmediateUpdateResult
			got, err := f.Runtime.ImmediateUpdateWithProgress(context.Background(), f.Now.Add(time.Second), func(r ImmediateUpdateResult) { observed = r })
			if err != nil || observed.Reason != tc.reason || observed.CountsKnown == tc.missing || got.WaitReason != tc.reason || got.CountsKnown != observed.CountsKnown {
				t.Fatalf("result=%+v observed=%+v err=%v", got, observed, err)
			}
			if !tc.missing && (observed.Implementing != 1 || observed.Publishing != 2 || got.Publishing != 2) {
				t.Fatalf("counts: result=%+v observed=%+v", got, observed)
			}
		})
	}
}

// アプリ配置の失敗で、読む側の同期と稼働確認を飛ばさない。
// 3つの段の保存状態を壊し、どの段まで進んだかを失敗の数で見る（TestNormalCycleOtherStagesAndDrainIsolation と同じ方法）。
// 1回目の周期は更新の drain に入るだけで段に進まず、期限を過ぎた2回目で段に進む。
func TestCycleRunsReadersAndLiveChecksAfterAppsFail(t *testing.T) {
	f := newUpdateFixture(t)
	for _, file := range []string{"apps-state.json", "reader-state.json", "live-state.json"} {
		if err := os.WriteFile(filepath.Join(f.Runtime.Config.Home, file), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Runtime.Cycle(context.Background()); err != nil {
		t.Fatalf("draining entered stages: %v", err)
	}
	f.Now = f.Now.Add(3 * time.Hour)
	f.OnSnapshot = func(s *imageSnapshot) { one := 1; s.Heartbeat.Publishing = &one }
	err := f.Runtime.Cycle(context.Background())
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 3 {
		t.Fatalf("3つの段の失敗がそろっていません: %v", err)
	}
}

// 更新の段の失敗で、アプリ配置・読む側の同期・稼働確認を飛ばさない。
// 更新は入れ替えを始める前（環境の読取）で失敗させ、保存状態を壊した3つの段まで進んだかを失敗の数で見る。
func TestCycleRunsStagesAfterUpdateFails(t *testing.T) {
	f := newUpdateFixture(t)
	for _, file := range []string{"apps-state.json", "reader-state.json", "live-state.json"} {
		if err := os.WriteFile(filepath.Join(f.Runtime.Config.Home, file), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(f.Runtime.Config.ClientDir, ".env")); err != nil {
		t.Fatal(err)
	}
	err := f.Runtime.Cycle(context.Background())
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 4 || !strings.Contains(err.Error(), "環境ファイル") {
		t.Fatalf("更新と3つの段の失敗がそろっていません: %v", err)
	}
}

// 更新が失敗しても、保存状態が入れ替えの途中（drain中の待ち）なら3つの段を飛ばし、更新の失敗だけを返す。
func TestCycleSkipsStagesAfterUpdateFailsDuringSwap(t *testing.T) {
	f := newUpdateFixture(t)
	for _, file := range []string{"apps-state.json", "reader-state.json", "live-state.json"} {
		if err := os.WriteFile(filepath.Join(f.Runtime.Config.Home, file), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Runtime.save("state.json", &UpdateState{Phase: "waiting", DrainSet: true, Target: f.Target, Started: f.Now, Until: f.Now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Runtime.Config.ClientDir, ".env")); err != nil {
		t.Fatal(err)
	}
	err := f.Runtime.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "環境ファイル") {
		t.Fatalf("更新の失敗を返しません: %v", err)
	}
	if _, joined := err.(interface{ Unwrap() []error }); joined {
		t.Fatalf("入れ替えの途中で他の段に進みました: %v", err)
	}
}
