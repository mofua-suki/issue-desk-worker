package nativetest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/intake"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 設定の版が変わったときと、担当から外れたときは、native testを続けない。
func TestPolicyRefusesChangedConfiguration(t *testing.T) {
	fleet := fleetconfig.Fleet{Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}}}
	fleet.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	fleet.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
	for name, tc := range map[string]struct {
		version string
		fleet   fleetconfig.Fleet
		ok      bool
	}{
		"same":    {"v1", fleet, true},
		"version": {"v2", fleet, false},
		"removed": {"v1", fleetconfig.Fleet{Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "other"}}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.fleet)
			api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "linux", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), transport(func(r *http.Request) (*http.Response, error) {
				body, _ := json.Marshal(intake.ConfigSnapshot{Version: "x", SettingsVersion: "s", ConfigurationVersion: tc.version, Config: raw})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = Policy(context.Background(), api, fleetconfig.Configuration{Host: "linux", Version: "v1"}, "owner/project", "apple")
			if (err == nil) != tc.ok {
				t.Fatalf("%v", err)
			}
		})
	}
}
