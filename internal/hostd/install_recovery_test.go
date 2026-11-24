package hostd

import (
	"context"
	"errors"
	"issue-desk-worker/internal/process"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallRequiresFreshCompleteHeartbeatAndOwnDrain(t *testing.T) {
	for _, mode := range []string{"stale", "future", "missing-counts", "missing-mark", "foreign-mark", "old-mark"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallFixture(t)
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.Now = f.Now.Add(time.Minute)
			f.OnSnapshot = func(s *imageSnapshot) {
				switch mode {
				case "stale":
					s.Heartbeat.At = f.Drain.At
				case "future":
					s.Heartbeat.At = f.Now.Add(time.Second)
				case "missing-counts":
					s.Heartbeat.Publishing = nil
				case "missing-mark":
					s.Heartbeat.Drain.Hostd = nil
				case "foreign-mark":
					copy := *f.Drain
					copy.By = "hostd:another"
					s.Heartbeat.Drain.Hostd = &copy
				case "old-mark":
					copy := *f.Drain
					copy.At = copy.At.Add(-time.Hour)
					s.Heartbeat.Drain.Hostd = &copy
				}
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Stops+f.Starts != 0 {
				t.Fatal("stopped without fresh drain confirmation")
			}
			if strings.HasSuffix(mode, "mark") {
				requireCommandReason(t, f, "failed")
			}
		})
	}
}

func TestInstallExpiresOrIsReplacedWhileWaitingWithoutStopping(t *testing.T) {
	for _, mode := range []string{"expires", "replaced", "changed", "removed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallFixture(t)
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			original := *f.Command
			f.Now = f.Now.Add(time.Minute)
			reason := "no_target"
			switch mode {
			case "expires":
				f.Now = original.ExpiresAt
				reason = "expired"
			case "replaced":
				copy := *f.Command
				copy.ID = "another"
				f.Command = &copy
			case "changed":
				copy := *f.Command
				copy.Tag = strings.Repeat("c", 12)
				f.Command = &copy
			case "removed":
				f.Command = nil
			case "timeout":
				f.Now = f.Now.Add(3 * time.Hour)
				reason = "expired"
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.Command = &original
			requireCommandReason(t, f, reason)
			if f.Stops+f.Starts != 0 || f.Drain != nil {
				t.Fatal("expired/replaced command affected worker")
			}
		})
	}
}

func TestInstallVerifiesPulledDigestBeforeDraining(t *testing.T) {
	f := newInstallFixture(t)
	f.OnSnapshot = func(s *imageSnapshot) {
		copy := *s.Desired
		copy.Digest = "sha256:" + strings.Repeat("f", 64)
		s.Desired = &copy
	}
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCommandReason(t, f, "verify_failed")
	if f.Stops+f.Starts != 0 || f.Drain != nil {
		t.Fatal("mismatching digest stopped worker")
	}
}

func TestInstallPreemptsWaitingUpdateAndKeepsDrainUntilWorkerStops(t *testing.T) {
	f := newInstallFixture(t)
	command := f.Command
	f.Command = nil
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Normal lazy update has not drained. Model the persisted waiting/drained
	// state produced by the ordinary update path with lazy disabled.
	f.OnSnapshot = nil
	f.Target.Kind = ""
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Drain == nil {
		t.Fatal("normal update not drained")
	}
	f.Command = command
	f.Now = f.Now.Add(time.Minute)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Drain == nil || f.Starts != 0 {
		t.Fatal("waiting update not safely replaced")
	}
	f.Now = f.Now.Add(time.Minute)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCommandReason(t, f, "done")
	for _, event := range f.Events {
		if event == "stop" {
			break
		}
		if event == "DELETE /api/v1/hosts/test/drain" {
			t.Fatal("inherited drain released before stop")
		}
	}
}

func TestInstallFinishesOrdinaryRecoveryOrReportingFirst(t *testing.T) {
	for _, phase := range []string{"updating", "recovering", "reporting", "releasing"} {
		t.Run(phase, func(t *testing.T) {
			f := newInstallFixture(t)
			if err := f.Runtime.save("state.json", &UpdateState{Phase: phase, Target: f.Target, Status: "success"}); err != nil {
				t.Fatal(err)
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.Pulls+f.Stops+f.Starts != 0 || len(f.CommandResults) != 0 {
				t.Fatal("install bypassed ordinary transaction")
			}
		})
	}
}

func TestInstallInterruptedStopOrSwitchRecoversWithoutRepeating(t *testing.T) {
	for _, phase := range []string{"command-stopping", "updating", "recovering"} {
		t.Run(phase, func(t *testing.T) {
			f := newInstallFixture(t)
			s := &UpdateState{Command: f.Command, CommandHost: f.Runtime.Config.Host, Phase: phase, Target: f.Target, OldTag: strings.Repeat("a", 12), DrainSet: true}
			if phase != "command-stopping" {
				if err := os.WriteFile(filepath.Join(f.Runtime.Config.ClientDir, ".linux-update.pending"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Runtime.save("state.json", s); err != nil {
				t.Fatal(err)
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			requireCommandReason(t, f, "switch_failed")
			if f.Starts+f.Pulls+f.Stops != 0 {
				t.Fatal("interrupted install repeated")
			}
			if phase == "command-stopping" {
				if f.Restarts != 1 {
					t.Fatal("stopped old worker not restarted")
				}
			} else if f.Restores != 1 {
				t.Fatal("pending switch not restored")
			}
		})
	}
}

func TestInstallRetainsRecoveryUntilOldWorkerCanStart(t *testing.T) {
	f := newInstallFixture(t)
	f.FailStop = true
	base := f.Runtime.Run
	f.Runtime.Run = func(ctx context.Context, name string, args []string, o process.Options) (process.Result, error) {
		if strings.Contains(strings.Join(args, " "), "up -d --no-deps --pull never") {
			return process.Result{}, errors.New("synthetic start failure")
		}
		return base(ctx, name, args, o)
	}
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	if f.Runtime.updateCycle(context.Background()) == nil {
		t.Fatal("restart failure lost")
	}
	var state *UpdateState
	if err := f.Runtime.load("state.json", &state); err != nil || state == nil || state.Phase != "recovering" || f.Drain == nil || len(f.CommandResults) != 0 {
		t.Fatal("recovery obligation lost", err)
	}
	f.Runtime.Run = base
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCommandReason(t, f, "failed")
	if f.Starts != 0 || f.Stops != 1 || f.Drain != nil {
		t.Fatal("unsafe recovery retry")
	}
}

func TestInstallCannotPullBeforeCommandStateIsSaved(t *testing.T) {
	f := newInstallFixture(t)
	if err := os.Mkdir(filepath.Join(f.Runtime.Config.Home, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Runtime.acceptCommand(context.Background(), environment{Tag: strings.Repeat("a", 12)}, imageSnapshot{Command: f.Command}, nil); err == nil {
		t.Fatal("save failure ignored")
	}
	if f.Pulls+f.Stops+f.Starts != 0 {
		t.Fatal("effect before durable command")
	}
}

func TestInstallNormalizesPythonTagAndAcceptsNonDesiredRelease(t *testing.T) {
	for _, flavor := range []string{"", "-python"} {
		t.Run("flavor="+flavor, func(t *testing.T) {
			f := newInstallFixture(t)
			f.Target.Flavor = flavor
			if flavor != "" {
				f.Target.Tag += "-client-python"
				f.Command.Tag += "-python"
			}
			if err := os.WriteFile(filepath.Join(f.Runtime.Config.ClientDir, ".env"), []byte("FLAVOR="+flavor+"\nISSUE_DESK_CLIENT_TAG="+strings.Repeat("a", 12)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.OnSnapshot = func(s *imageSnapshot) {
				copy := *s.Desired
				copy.Tag = strings.Repeat("c", 12)
				copy.Revision = strings.Repeat("c", 40)
				s.Desired = &copy
				s.Pin = map[string]any{"tag": copy.Tag}
			}
			installCycles(t, f)
			requireCommandReason(t, f, "done")
			env, err := f.Runtime.environment()
			if err != nil || env.Tag != f.Target.Tag {
				t.Fatal("wrong installed tag", env, err)
			}
			// Once the command disappears, follow the ordinary desired update path.
			f.Command = nil
			f.OnSnapshot = func(s *imageSnapshot) {
				copy := *s.Desired
				copy.Tag = strings.Repeat("c", 12)
				copy.Revision = strings.Repeat("c", 40)
				if flavor != "" {
					copy.Tag += "-client-python"
				}
				s.Desired = &copy
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			var state *UpdateState
			if err := f.Runtime.load("state.json", &state); err != nil || state == nil || state.Command != nil || state.Target.Tag[:12] != strings.Repeat("c", 12) {
				t.Fatal("desired follow lost", err)
			}
		})
	}
}

func TestUnsupportedResultRetrySurvivesCommandReplacement(t *testing.T) {
	f := newInstallFixture(t)
	f.Command.Kind = "rollback"
	f.ResultStatus = 503
	if f.Runtime.updateCycle(context.Background()) == nil {
		t.Fatal("report error ignored")
	}
	first := f.CommandResults[0]
	f.Command = nil
	f.ResultStatus = 400
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.CommandResults) != 2 || f.CommandResults[1] != first {
		t.Fatal("pending result changed or lost")
	}
	receipts, err := f.Runtime.receipts()
	if err != nil || receipts[first.ID].Pending || receipts[first.ID].Result != nil {
		t.Fatal("400 result not discarded", err)
	}
}
