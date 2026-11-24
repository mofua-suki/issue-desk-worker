package hostd

import (
	"context"
	"issue-desk-worker/internal/process"
	"strings"
	"testing"
	"time"
)

func TestInstallCannotResumeStateAddressedToAnotherPC(t *testing.T) {
	f := newInstallFixture(t)
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Runtime.Config.Host = "another-pc"
	f.Now = f.Now.Add(time.Minute)
	if f.Runtime.updateCycle(context.Background()) == nil {
		t.Fatal("command's persisted host binding was ignored")
	}
	if f.Starts+f.Stops != 0 || len(f.CommandResults) != 0 {
		t.Fatal("another PC's command was executed or reported")
	}
}

func TestInstallDrainFailureNeverStopsWorker(t *testing.T) {
	f := newInstallFixture(t)
	f.DrainStatus = 503
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCommandReason(t, f, "failed")
	if f.Starts+f.Stops != 0 || f.Drain != nil {
		t.Fatal("drain failure switched worker")
	}
}

func TestInstallRechecksExpiryAndDrainAfterContainerStop(t *testing.T) {
	for _, mode := range []string{"command-expiry", "drain-expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstallFixture(t)
			base := f.Runtime.Run
			f.Runtime.Run = func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
				out, err := base(ctx, name, args, options)
				if strings.Contains(strings.Join(args, " "), "stop --timeout 60") {
					if mode == "command-expiry" {
						f.Now = f.Command.ExpiresAt
					} else {
						f.Now = f.Drain.Until
						f.Command.ExpiresAt = f.Now.Add(time.Hour)
					}
				}
				return out, err
			}
			// Extend the original persisted expiry so drain-expiry can be tested
			// independently of command-expiry.
			if mode == "drain-expiry" {
				f.Command.ExpiresAt = f.Now.Add(24 * time.Hour)
			}
			installCycles(t, f)
			reason := "expired"
			if mode == "drain-expiry" {
				reason = "failed"
			}
			requireCommandReason(t, f, reason)
			if f.Starts != 0 || f.Stops != 1 || f.Restarts != 1 || f.Drain != nil {
				t.Fatal("expired authorization switched or stranded worker")
			}
		})
	}
}

func TestInstallRemembersEarlierIDsAfterLaterCommands(t *testing.T) {
	f := newInstallFixture(t)
	installCycles(t, f)
	original := *f.Command
	second := original
	second.ID = "install-2"
	f.Command = &second
	installCycles(t, f)
	f.Command = &original
	if err := f.Runtime.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 2 || f.Stops != 2 || f.Pulls != 2 || len(f.CommandResults) != 2 {
		t.Fatal("older ID executed again")
	}
}

func TestInstallRejectsWrongRunningImageEvenWhenPulledImageMatches(t *testing.T) {
	f := newInstallFixture(t)
	base := f.Runtime.Run
	f.Runtime.Run = func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
		if name == "docker" && len(args) > 3 && args[0] == "image" && args[len(args)-1] == "sha256:"+strings.Repeat("e", 64) {
			return process.Result{Out: `["registry.example:5443/issue-desk-client@sha256:` + strings.Repeat("f", 64) + `"]`}, nil
		}
		return base(ctx, name, args, options)
	}
	installCycles(t, f)
	requireCommandReason(t, f, "verify_failed")
	if f.Restores != 1 {
		t.Fatal("wrong running image accepted")
	}
}
