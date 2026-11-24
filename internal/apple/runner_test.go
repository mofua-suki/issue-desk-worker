package apple

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/nativetest"
	"issue-desk-worker/internal/requeststore"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
)

func TestAppleConfigurationNeedsNoImplementationTools(t *testing.T) {
	fleet := fleetconfig.Fleet{Git: "/usr/bin/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}}}
	fleet.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
	fleet.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	fleet.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
	snapshot := intake.ConfigSnapshot{Config: encode(fleet), ConfigurationVersion: "v"}
	c, err := fleetconfig.DecodeApple(snapshot, "apple")
	if err != nil {
		t.Fatal(err)
	}
	if c.Codex != "" || c.Claude != "" || !c.TestOnly() {
		t.Fatal("unexpected implementation requirements")
	}
	runner := &Runner{OS: "linux", Config: c}
	if runner.Check(context.Background()) == nil {
		t.Fatal("non-Darwin admitted")
	}
	fleet.Assignments.HostRoles["apple"] = "invalid-role"
	snapshot.Config = encode(fleet)
	if _, err = fleetconfig.DecodeApple(snapshot, "apple"); err == nil {
		t.Fatal("invalid host role accepted")
	}
}
func TestAppleExecutionChecksTreeAndRunsCommand(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "tree-mismatch", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "executed")
			git := filepath.Join(root, "git")
			sandbox := filepath.Join(root, "sandbox")
			tree := strings.Repeat("c", 40)
			if err := os.WriteFile(git, []byte("#!/bin/sh\nfor arg do if [ \"$arg\" = write-tree ]; then echo "+tree+"; exit 0; fi; done\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			// Substitute only the external Darwin sandbox executable; the product still
			// invokes its command, timeout, cancellation and result classification.
			if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &Runner{Root: root, sandbox: sandbox, Config: fleetconfig.Configuration{Fleet: fleetconfig.Fleet{Git: git, GiteaURL: "https://git.invalid", TestTimeoutSeconds: 1}}}
			target := nativetest.Target{Host: "apple", Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tree: tree}
			if outcome == "tree-mismatch" {
				target.Tree = target.Head
			}
			command := []string{"/bin/sh", "-c", "echo executed > " + shellArg(marker)}
			if outcome == "failed" {
				command[2] += "; exit 1"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if outcome == "cancelled" {
				cancel()
			}
			result := runner.execute(ctx, "owner/project", target, fleetconfig.Repository{TestCommand: command})
			want := outcome
			if outcome == "cancelled" {
				want = "interrupted"
			}
			if outcome == "tree-mismatch" {
				want = "unrunnable"
			}
			if result.Status != want {
				t.Fatalf("result: %+v", result)
			}
			_, err := os.Stat(marker)
			executed := err == nil
			if executed != (outcome == "passed" || outcome == "failed") {
				t.Fatalf("command execution=%v", executed)
			}
		})
	}
}
func TestAppleRestartDoesNotExecuteClaimedWorkTwice(t *testing.T) {
	root := t.TempDir()
	target := nativetest.Target{Host: "apple", Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)}
	request := requeststore.Request{ID: "123-aabbccdd", Kind: "native_test", Repo: "owner/project", Payload: encode(target)}
	c := fleetconfig.Configuration{Host: "apple", Version: "v", Fleet: fleetconfig.Fleet{Repositories: []fleetconfig.Repository{{Name: request.Repo, NativeTestHost: "apple", TestCommand: []string{"must-not-execute"}}}}}
	c.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
	c.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	c.Assignments.TestRepositories = map[string][]string{"apple": {request.Repo}}
	posts := 0
	transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/client-config" {
			return nativeResponse(200, intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "v", SettingsVersion: "s", Config: encode(c.Fleet)}), nil
		}
		if strings.HasSuffix(r.URL.Path, "/result") {
			posts++
			var body struct {
				Result nativetest.Result `json:"result"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Result.Status != "interrupted" || body.Result.Head != target.Head {
				t.Fatalf("forged restart result: %+v", body.Result)
			}
			return nativeResponse(200, map[string]any{}), nil
		}
		return nativeResponse(200, map[string]any{"request": nativetest.RequestState{Request: request, Status: "claimed"}}), nil
	})
	api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Config: c, API: api, Root: root}
	e := runner.stateExecutor()
	p := &requeststore.Progress{Request: request, Stage: "running", NativeVersion: c.Version}
	if err = e.saveRequest(p); err != nil {
		t.Fatal(err)
	}
	if err = runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("result posts=%d", posts)
	}
	saved, err := e.store.Load(request)
	if err != nil || saved.Stage != "done" {
		t.Fatalf("state=%+v err=%v", saved, err)
	}
}

func TestAppleIsolationFailureNeverClaimsRequest(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "git-token")
	pcToken := filepath.Join(root, "pc-token")
	if err := os.WriteFile(token, []byte("git_token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pcToken, []byte("idk_"+strings.Repeat("a", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	c := fleetconfig.Configuration{Host: "apple", Fleet: fleetconfig.Fleet{Git: "/usr/bin/git", GiteaURL: "https://git.invalid", WorkerTokenFile: token, Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}}}}
	c.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
	c.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	c.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
	c.Assignments.WorkerAccounts = map[string]string{"apple": "monitor"}
	claims := 0
	transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/requests/next") {
			claims++
			return nativeResponse(200, map[string]any{}), nil
		}
		if strings.HasSuffix(r.URL.Path, "/user") {
			return nativeResponse(200, map[string]string{"login": "monitor"}), nil
		}
		if r.Method == "PATCH" {
			return nativeResponse(404, map[string]any{}), nil
		}
		return nativeResponse(200, map[string]any{}), nil
	})
	old := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = old }()
	b := config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: pcToken}
	api, err := intake.New(b, "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{OS: "darwin", Root: filepath.Join(root, "runtime"), Bootstrap: b, Config: c, API: api, sandbox: "/bin/false"}
	if err = runner.Run(context.Background()); err == nil {
		t.Fatal("failed isolation admitted runner")
	}
	if claims != 0 {
		t.Fatal("claimed before isolation check")
	}
	data, err := os.ReadFile(pcToken)
	if err != nil || string(data) != "idk_"+strings.Repeat("a", 43) {
		t.Fatal("preflight changed PC credential")
	}
}

func TestAppleStopsBackgroundTestProcesses(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "background-survived")
	runner := &Runner{Root: root, sandbox: sandbox}
	result, err := runner.runSandbox(context.Background(), tree, []string{"/bin/sh", "-c", "(/bin/sleep 0.2; echo survived > " + shellArg(marker) + ") >/dev/null 2>&1 & exit 0"}, time.Second)
	if err != nil || result.Code != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("background test process survived command completion")
	}
}
