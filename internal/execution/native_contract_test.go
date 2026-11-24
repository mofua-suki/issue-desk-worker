package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeployedAppleAssignmentContract(t *testing.T) {
	raw := json.RawMessage(`{"hostRole":"implementation","testRepositories":["owner/other"],"git":"/usr/bin/git","giteaUrl":"https://git.invalid","workerTokenFile":"/private/git-token","repositories":[{"name":"owner/project","nativeTestHost":"apple","testCommand":["sh","scripts/verify-apple.sh","macos"]}],"assignments":{"hosts":["linux","apple"],"hostRoles":{"linux":"implementation","apple":"test-only"},"testRepositories":{"apple":["owner/project"]}},"nativeTestVersions":{"apple":"0123456789abcdef"}}`)
	var fleet Fleet
	if err := json.Unmarshal(raw, &fleet); err != nil {
		t.Fatal(err)
	}
	target := Configuration{Fleet: fleet, Host: "apple", AssignmentHost: "linux"}
	repos, err := target.NativeRepositories()
	if err != nil || len(repos) != 1 || repos[0].Name != "owner/project" || !target.TestOnly() {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	e := Executor{Config: Configuration{Fleet: fleet, Host: "linux"}}
	who := map[string]any{}
	e.addNativeTestGate("OWNER/PROJECT", who)
	gate := who["nativeTestGate"].(map[string]any)
	if gate["configurationVersion"] != "0123456789abcdef" || gate["host"] != "apple" {
		t.Fatalf("gate=%v", gate)
	}
	delete(fleet.Assignments.HostRoles, "apple")
	delete(fleet.Assignments.TestRepositories, "apple")
	target.Fleet = fleet
	if _, err = target.NativeRepositories(); err == nil {
		t.Fatal("sender assignment reused for target")
	}
	// The same deployment retains ordinary Linux implementation admission.
	linux := Configuration{Fleet: fleet, Host: "linux"}
	if linux.TestOnly() {
		t.Fatal("Linux classified as tester")
	}
}

func TestNativeRequestIdentitySeparatesAttemptsAndSenders(t *testing.T) {
	p := nativeTarget{Host: "apple", Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)}
	first := nativeID("owner/project", p, "version", "linux-a", "issue-1/generation-2")
	if nativeID("owner/project", p, "version", "linux-a", "issue-1/generation-2") != first {
		t.Fatal("resume changed request ID")
	}
	for _, args := range [][]string{{"linux-b", "issue-1/generation-2"}, {"linux-a", "issue-1/generation-3"}, {"linux-a", "manual-new"}} {
		if nativeID("owner/project", p, "version", args...) == first {
			t.Fatal("new attempt reused failed request")
		}
	}
}

func TestMergeTurnEnforcesNativeFleetGateOnAcquireAndRenew(t *testing.T) {
	for _, scenario := range []string{"current", "old-worker", "configuration-changed", "turn-lost"} {
		t.Run(scenario, func(t *testing.T) {
			c := Configuration{Host: "linux", Fleet: Fleet{Repositories: []Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"true"}}}, NativeTestVersions: map[string]string{"apple": "0123456789abcdef"}}}
			if scenario == "old-worker" {
				c.Repositories[0].NativeTestHost = ""
			}
			acquired := false
			api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "linux", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), nativeTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/release") {
					return nativeResponse(200, map[string]any{}), nil
				}
				var who struct {
					NativeTestGate struct {
						Version              int
						Host                 string
						ConfigurationVersion string
					} `json:"nativeTestGate"`
					TurnEpoch int `json:"turnEpoch"`
				}
				if err := json.NewDecoder(r.Body).Decode(&who); err != nil {
					t.Fatal(err)
				}
				if who.NativeTestGate.Version != 1 || who.NativeTestGate.Host != "apple" || who.NativeTestGate.ConfigurationVersion != "0123456789abcdef" {
					return nativeResponse(409, map[string]any{"code": "NATIVE_TEST_GATE_REQUIRED"}), nil
				}
				if strings.HasSuffix(r.URL.Path, "/acquire") {
					acquired = true
					return nativeResponse(200, map[string]any{"granted": true, "turn": map[string]any{"epoch": 4, "expiresAt": time.Now().Add(time.Minute)}}), nil
				}
				if who.TurnEpoch != 4 {
					t.Fatal("turn fence absent")
				}
				if scenario == "configuration-changed" || scenario == "turn-lost" {
					return nativeResponse(409, map[string]any{"code": "NATIVE_TEST_GATE_REQUIRED"}), nil
				}
				return nativeResponse(200, map[string]any{"turn": map[string]any{"epoch": 4, "expiresAt": time.Now().Add(time.Minute)}}), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			e := Executor{Config: c, API: api}
			turn, err := e.acquireMergeTurn(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project"}}, &journal.Record{Epoch: 2}, func(context.Context) error { return nil })
			if scenario == "old-worker" {
				if err == nil || acquired {
					t.Fatal("old client acquired protected turn")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer turn.release()
			err = turn.renew(context.Background())
			if scenario == "current" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || turn.ctx.Err() == nil {
				t.Fatal("lost gate did not cancel merge context")
			}
		})
	}
}
