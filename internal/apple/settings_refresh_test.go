package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/failurelog"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/readiness"
)

func TestSettingsRefreshHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name                                                         string
		fetchFailure, decodeFailure, versionChange, credentialChange bool
	}{
		{name: "same effective version"},
		{name: "fetch failure", fetchFailure: true},
		{name: "decode failure", decodeFailure: true},
		{name: "effective version change", versionChange: true},
		{name: "credential change", credentialChange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var beats []map[string]any
			fleet := fleetconfig.Fleet{Git: "/missing/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"}, Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}}}
			if tc.credentialChange {
				fleet.GiteaURL = "https://other.invalid"
			}
			if tc.decodeFailure {
				fleet.Git = ""
			}
			version := "v1"
			if tc.versionChange {
				version = "v2"
			}
			a := appleRoundRunner(t, func(r *http.Request) *http.Response {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/v1/hosts/"):
					var status intake.HostStatus
					if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
						t.Fatal(err)
					}
					beats = append(beats, status.Checks)
					return nativeResponse(200, map[string]any{})
				case strings.HasSuffix(r.URL.Path, "/client-config"):
					if tc.fetchFailure {
						return nativeResponse(503, map[string]any{})
					}
					return nativeResponse(200, intake.ConfigSnapshot{Version: version, ConfigurationVersion: version, SettingsVersion: "s2", Config: encode(fleet)})
				case strings.HasSuffix(r.URL.Path, "/next"):
					return nativeResponse(200, map[string]any{"request": nil})
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
					return nil
				}
			})
			a.Config.SettingsVersion = "s1"
			a.checks = readiness.Report{At: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), SettingsVersion: "s1", Items: []readiness.Item{{Name: "runtime", Label: "runtime label", OK: false, Detail: "original failure"}, {Name: "config", Label: "config label", OK: true, Detail: "original success"}}}
			original := a.checks.Value()
			err := a.round(context.Background(), &failurelog.Log{Out: &bytes.Buffer{}})
			var restart *appleRestart
			if errors.As(err, &restart) != (tc.versionChange || tc.credentialChange) {
				t.Fatalf("restart = %v", err)
			}
			if err := a.heartbeat(context.Background(), true, false); err != nil {
				t.Fatal(err)
			}
			if len(beats) != 2 {
				t.Fatalf("heartbeats = %d", len(beats))
			}
			if !reflect.DeepEqual(beats[0], original) {
				t.Fatalf("initial heartbeat = %#v, want %#v", beats[0], original)
			}
			if tc.versionChange {
				if beats[1]["settingsVersion"] != "s2" || reflect.DeepEqual(beats[1]["items"], original["items"]) || beats[1]["at"] == original["at"] || beats[1]["ok"] != false {
					t.Fatalf("recheck heartbeat = %#v", beats[1])
				}
			} else {
				if !tc.fetchFailure && !tc.decodeFailure && !tc.credentialChange {
					original["settingsVersion"] = "s2"
				}
				if !reflect.DeepEqual(beats[1], original) {
					t.Fatalf("next heartbeat = %#v, want %#v", beats[1], original)
				}
			}
		})
	}
}
