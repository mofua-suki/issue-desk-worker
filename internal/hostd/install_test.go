package hostd

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/process"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type installFixture struct {
	*updateFixture
	Pulls, Stops, Restarts                           int
	FailPull, FailStop, Running, BadState, BadDigest bool
	FailBeforeSwitch, FailReady                      bool
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{updateFixture: newUpdateFixture(t)}
	f.Command = &HostCommand{ID: "install-1", Kind: "install", Tag: f.Target.Tag, ExpiresAt: f.Now.Add(time.Hour)}
	f.OnSnapshot = func(s *imageSnapshot) {
		one := 1
		s.Heartbeat.Publishing = &one
		s.Desired.Kind = "lazy"
		// Install is allowed despite a stale failed-tag entry. The real API
		// clears this entry on issuance; either response must allow the command.
		s.FailedTags = []string{f.Target.Tag}
		if f.FailReady {
			s.Heartbeat.Ready = false
			zero := 0
			s.Heartbeat.Publishing = &zero
		}
	}
	base := f.Runtime.Run
	f.Runtime.Run = func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
		var state *UpdateState
		if err := f.Runtime.load("state.json", &state); err != nil {
			t.Fatal(err)
		}
		if name == "bash" && args[len(args)-1] == "--pull-only" {
			if state == nil || state.Command == nil || state.Command.ID != f.Command.ID || state.Phase != "command-pulling" {
				t.Fatal("pull before durable command")
			}
			f.Pulls++
			f.Events = append(f.Events, "pull-only")
			if f.FailPull {
				return process.Result{Code: 1}, nil
			}
			return process.Result{}, nil
		}
		joined := strings.Join(args, " ")
		if name == "docker" && len(args) > 0 && args[0] == "compose" {
			switch {
			case strings.Contains(joined, "ps --all --quiet"):
				return process.Result{Out: strings.Repeat("c", 64)}, nil
			case strings.Contains(joined, "stop --timeout 60 issue-desk-client"):
				if state == nil || state.Command == nil || state.Phase != "command-stopping" || f.Drain == nil {
					t.Fatal("stop before durable drained state")
				}
				if options.Timeout < 60*time.Second {
					t.Fatal("stop timeout shorter than grace")
				}
				f.Stops++
				f.Events = append(f.Events, "stop")
				if f.FailStop {
					return process.Result{Code: 1}, nil
				}
				return process.Result{}, nil
			case strings.Contains(joined, "up -d --no-deps --pull never issue-desk-client"):
				f.Restarts++
				f.Events = append(f.Events, "restart")
				return process.Result{}, nil
			}
		}
		if name == "docker" && args[0] == "inspect" {
			if args[2] == "{{json .State}}" {
				f.Events = append(f.Events, "confirm-stopped")
				if f.BadState {
					return process.Result{Out: `{}`}, nil
				}
				return process.Result{Out: `{"Running":` + map[bool]string{true: "true", false: "false"}[f.Running] + `,"Restarting":false,"Status":"exited"}`}, nil
			}
			return process.Result{Out: `{"Image":"sha256:` + strings.Repeat("e", 64) + `","State":{"Running":true}}`}, nil
		}
		if name == "docker" && args[0] == "image" && f.BadDigest && f.Starts > 0 {
			return process.Result{Out: `[]`}, nil
		}
		if name == "bash" && args[len(args)-1] != "--restore-previous" {
			if f.FailBeforeSwitch {
				f.Starts++
				return process.Result{Code: 1}, nil
			}
			f.Events = append(f.Events, "switch")
		}
		return base(ctx, name, args, options)
	}
	return f
}

func installCycles(t *testing.T, f *installFixture) {
	t.Helper()
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func requireCommandReason(t *testing.T, f *installFixture, reason string) {
	t.Helper()
	if len(f.CommandResults) == 0 {
		t.Fatal("no command result")
	}
	result := f.CommandResults[len(f.CommandResults)-1]
	if result.ID != f.Command.ID || result.Reason != reason || result.OK != (reason == "done") {
		t.Fatalf("result: %+v", result)
	}
}

func TestInstallSwitchesPublishingWorkerOnlyAfterDrainAndStop(t *testing.T) {
	f := newInstallFixture(t)
	// Match the production API: issuing install removes the failed-tag record.
	onSnapshot := f.OnSnapshot
	f.OnSnapshot = func(s *imageSnapshot) { onSnapshot(s); s.FailedTags = nil }
	installCycles(t, f)
	requireCommandReason(t, f, "done")
	if f.Starts != 1 || f.Pulls != 1 || f.Stops != 1 || f.Drain != nil {
		t.Fatalf("unexpected lifecycle: %+v", f)
	}
	positions := []int{}
	for _, want := range []string{"PUT /api/v1/hosts/test/drain", "stop", "confirm-stopped", "switch", "POST /api/v1/hosts/test/hostd-health", "DELETE /api/v1/hosts/test/drain"} {
		index := -1
		for i, event := range f.Events {
			if event == want {
				index = i
				break
			}
		}
		if index < 0 {
			t.Fatalf("missing %s: %v", want, f.Events)
		}
		positions = append(positions, index)
	}
	for i := 1; i < len(positions); i++ {
		if positions[i] <= positions[i-1] {
			t.Fatal("unsafe order", f.Events)
		}
	}
	for _, event := range f.Events {
		if strings.Contains(event, "lease") {
			t.Fatal("lease returned")
		}
	}
}

func TestInstallAndUnsupportedKindsHaveFixedBehavior(t *testing.T) {
	for _, kind := range []string{"install", "rollback", "pin", "follow", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallFixture(t)
			f.Command.Kind = kind
			installCycles(t, f)
			if kind == "install" {
				requireCommandReason(t, f, "done")
			} else {
				requireCommandReason(t, f, "unsupported")
				if f.Starts+f.Stops+f.Pulls != 0 {
					t.Fatal("unsupported command executed")
				}
			}
		})
	}
}

func TestInstallRejectsInvalidCommandsBeforePullOrStop(t *testing.T) {
	for _, mode := range []string{"expired", "missing-expiry", "foreign-host", "invalid-tag", "shell-tag", "wrong-flavor"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallFixture(t)
			reason := "no_target"
			switch mode {
			case "expired":
				f.Command.ExpiresAt = f.Now
				reason = "expired"
			case "missing-expiry":
				f.Command.ExpiresAt = time.Time{}
				reason = "expired"
			case "foreign-host":
				f.Command.Host = "another-pc"
			case "invalid-tag":
				f.Command.Tag = "latest"
			case "shell-tag":
				f.Command.Tag = "$(touch unwanted)"
			case "wrong-flavor":
				f.Command.Tag += "-python"
			}
			if err := f.Runtime.updateCycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			requireCommandReason(t, f, reason)
			if f.Starts+f.Stops+f.Pulls != 0 || f.Drain != nil {
				t.Fatal("invalid command affected worker")
			}
		})
	}
}

func TestInstallPullFailureDoesNotStopWorker(t *testing.T) {
	f := newInstallFixture(t)
	f.FailPull = true
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCommandReason(t, f, "tag_missing")
	if f.Stops+f.Starts != 0 || f.Drain != nil {
		t.Fatal("pull failure stopped worker")
	}
}

func TestInstallOffsetExpirySurvivesDraining(t *testing.T) {
	f := newInstallFixture(t)
	f.Command.ExpiresAt = f.Command.ExpiresAt.In(time.FixedZone("offset", 5*60*60))
	installCycles(t, f)
	requireCommandReason(t, f, "done")
}

func TestInstallCycleReportsFailureOutcome(t *testing.T) {
	for _, tc := range []struct{ name, detail string }{{"pull", "tag_missing"}, {"stop", "failed"}, {"drain", "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			switch tc.name {
			case "pull":
				f.FailPull = true
			case "stop":
				f.FailStop = true
			case "drain":
				f.DrainStatus = 503
			}
			if err := f.Runtime.Cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.name == "stop" {
				f.Now = f.Now.Add(time.Minute)
				if err := f.Runtime.Cycle(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			requireCommandReason(t, f, tc.detail)
			cycle := f.Cycles[len(f.Cycles)-1]
			if cycle["result"] != "failed" || cycle["stage"] != "install" || cycle["detail"] != tc.detail {
				t.Fatalf("cycle = %v", cycle)
			}
		})
	}
}

func TestInstallFailureRecoversAndReleasesDrain(t *testing.T) {
	for _, mode := range []string{"switch", "before-switch", "digest", "ready", "stop", "still-running", "missing-state"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallFixture(t)
			reason := "verify_failed"
			switch mode {
			case "switch":
				f.FailStart = true
				reason = "switch_failed"
			case "before-switch":
				f.FailBeforeSwitch = true
				reason = "switch_failed"
			case "digest":
				f.BadDigest = true
			case "ready":
				f.FailReady = true
			case "stop":
				f.FailStop = true
				reason = "failed"
			case "still-running":
				f.Running = true
				reason = "failed"
			case "missing-state":
				f.BadState = true
				reason = "failed"
			}
			installCycles(t, f)
			requireCommandReason(t, f, reason)
			if f.Drain != nil {
				t.Fatal("drain retained")
			}
			env, err := f.Runtime.environment()
			if err != nil || env.Tag != strings.Repeat("a", 12) {
				t.Fatal("old environment not restored", err)
			}
			if mode == "stop" || mode == "still-running" || mode == "missing-state" || mode == "before-switch" {
				if f.Restarts != 1 || f.Restores != 0 {
					t.Fatal("old stopped worker not restarted")
				}
				if mode != "before-switch" && f.Starts != 0 {
					t.Fatal("switched without stopping")
				}
			} else if f.Restores != 1 {
				t.Fatal("previous environment not restored")
			}
		})
	}
}

func TestInstallRetriesIdenticalResultAcrossRuntimeRestartWithoutReexecution(t *testing.T) {
	f := newInstallFixture(t)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	f.ResultStatus = 503
	if f.Runtime.updateCycle(context.Background()) == nil {
		t.Fatal("missing result error")
	}
	if f.Drain != nil {
		t.Fatal("report failure left acquisition stopped")
	}
	first := f.CommandResults[0]
	f.ResultStatus = 0
	restarted := *f.Runtime
	f.Runtime = &restarted
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.CommandResults) != 2 || !reflect.DeepEqual(first, f.CommandResults[1]) {
		t.Fatal("result changed on retry")
	}
	// Same ID, including changed contents, must not execute another time.
	f.Command.Tag = strings.Repeat("c", 12)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 1 || f.Pulls != 1 || f.Stops != 1 {
		t.Fatal("command executed twice")
	}
	info, err := os.Stat(filepath.Join(f.Runtime.Config.Home, "command-results.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions", err)
	}
}

func TestInstallDiscardsRejectedResultAndKeepsConsumedID(t *testing.T) {
	f := newInstallFixture(t)
	f.ResultStatus = 400
	installCycles(t, f)
	f.ResultStatus = 0
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.CommandResults) != 1 || f.Starts != 1 {
		t.Fatal("400 retried or install repeated")
	}
	var state *UpdateState
	if err := f.Runtime.load("state.json", &state); err != nil || state != nil {
		t.Fatal("finished state retained", err)
	}
}

func TestSnapshotNeverReportsCommandOrFailsOnUnsupportedKind(t *testing.T) {
	f := newInstallFixture(t)
	f.Command.Kind = "rollback"
	env, _ := f.Runtime.environment()
	for i := 0; i < 3; i++ {
		if _, err := f.Runtime.snapshot(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.CommandResults) != 0 || f.Starts != 0 {
		t.Fatal("snapshot had effects")
	}
}

// Keep JSON shape verification next to real HTTP serialization in the fixture.
func TestCommandReadsProductionWireFields(t *testing.T) {
	var snapshot imageSnapshot
	if err := json.Unmarshal([]byte(`{"command":{"id":"id","kind":"install","tag":"bbbbbbbbbbbb","issuedAt":"2026-11-01T00:00:00Z","expiresAt":"2026-11-02T00:00:00Z","by":"operator"}}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Command.Kind != "install" || snapshot.Command.Tag != "bbbbbbbbbbbb" || snapshot.Command.ExpiresAt.IsZero() {
		t.Fatal("missing command fields")
	}
	_, reason := commandTag(*snapshot.Command, environment{}, "test", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if reason != "" {
		t.Fatal(reason)
	}
}
