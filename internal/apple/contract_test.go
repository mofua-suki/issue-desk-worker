package apple

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/nativetest"
	"issue-desk-worker/internal/requeststore"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppleAccessAcceptsReadOnlyAndRejectsWrongIdentityOrRepo(t *testing.T) {
	for _, scenario := range []string{"read-only", "other-account", "denied-repo"} {
		t.Run(scenario, func(t *testing.T) {
			old := http.DefaultTransport
			defer func() { http.DefaultTransport = old }()
			reads := 0
			http.DefaultTransport = nativeTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatalf("write probe: %s", r.Method)
				}
				if r.URL.Path == "/api/v1/user" {
					login := "desk-apple"
					if scenario == "other-account" {
						login = "other"
					}
					return nativeResponse(200, map[string]string{"login": login}), nil
				}
				reads++
				if scenario == "denied-repo" && strings.HasSuffix(r.URL.Path, "/second") {
					return nativeResponse(403, map[string]any{}), nil
				}
				return nativeResponse(200, map[string]any{}), nil
			})
			g, err := gitea.New("https://git.invalid", "read-token")
			if err != nil {
				t.Fatal(err)
			}
			c := fleetconfig.Configuration{Host: "apple"}
			c.Assignments.WorkerAccounts = map[string]string{"apple": "desk-apple"}
			err = verifyAppleAccess(context.Background(), c, g, []fleetconfig.Repository{{Name: "owner/first"}, {Name: "owner/second"}})
			if (err == nil) != (scenario == "read-only") {
				t.Fatalf("err=%v", err)
			}
			if scenario == "read-only" && reads != 2 {
				t.Fatalf("reads=%d", reads)
			}
		})
	}
}

func TestAppleSDKAndCacheEnvironment(t *testing.T) {
	root := t.TempDir()
	sdk := filepath.Join(root, "sdk")
	runtime := filepath.Join(root, "runtime")
	for _, dir := range []string{filepath.Join(sdk, "bin", "cache"), runtime} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := Runner{Root: runtime, sandbox: sandbox}
	for _, bad := range []string{"/", "relative", root, runtime, filepath.Join(root, "missing")} {
		if _, err := a.toolRoots(fleetconfig.Repository{NativeToolRoots: []string{bad}}); err == nil {
			t.Fatalf("unsafe SDK accepted: %s", bad)
		}
	}
	alias := filepath.Join(root, "sdk-alias")
	if err := os.Symlink(sdk, alias); err != nil {
		t.Fatal(err)
	}
	policy := fleetconfig.Repository{Name: "owner/project", NativeToolRoots: []string{alias}}
	if err := os.WriteFile(filepath.Join(sdk, "bin", "dart-real"), []byte("#!/bin/sh\necho original\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sdk, "bin", "dart-real"), filepath.Join(sdk, "bin", "dart")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "bin", "cache", "flutter_tools.snapshot"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	command := []string{"/bin/sh", "-c", `test "$1" = "$(command -v dart)" || exit 8; test "$(cat "$(dirname "$(command -v dart)")/cache/flutter_tools.snapshot")" = original || exit 7; test "$(dart)" = original || exit 4; tool=$(command -v dart); echo '#!/bin/sh' > "$tool"; echo 'echo poisoned' >> "$tool"; test "$(dart)" = poisoned || exit 6; echo poisoned > "$(dirname "$tool")/cache/flutter_tools.snapshot"; test ! -f "$HOME/private" || exit 5; echo private > "$HOME/private"; echo dependency > "$PUB_CACHE/dependency"`, "check", filepath.Join(alias, "bin", "dart")}
	out, err := a.runSandbox(context.Background(), root, command, time.Second, policy)
	if err != nil || out.Code != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	out, err = a.runSandbox(context.Background(), root, []string{"/bin/sh", "-c", `test "$(cat "$(dirname "$(command -v dart)")/cache/flutter_tools.snapshot")" = original && test "$(dart)" = original && test ! -f "$HOME/private" && test -f "$PUB_CACHE/dependency"`}, time.Second, policy)
	if err != nil || out.Code != 0 {
		t.Fatalf("cache not reusable: %+v %v", out, err)
	}
	if data, err := os.ReadFile(filepath.Join(sdk, "bin", "dart")); err != nil || string(data) != "#!/bin/sh\necho original\n" {
		t.Fatalf("host SDK changed: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(sdk, "bin", "cache", "flutter_tools.snapshot")); err != nil || string(data) != "original" {
		t.Fatalf("host snapshot changed: %q %v", data, err)
	}
	policy.Name = "owner/other"
	out, err = a.runSandbox(context.Background(), root, []string{"/bin/sh", "-c", `test ! -f "$PUB_CACHE/dependency"`}, time.Second, policy)
	if err != nil || out.Code != 0 {
		t.Fatalf("repository cache shared: %+v %v", out, err)
	}
	escape := filepath.Join(sdk, "bin", "cache", "escape")
	if err := os.Symlink(runtime, escape); err != nil {
		t.Fatal(err)
	}
	if _, err := a.runSandbox(context.Background(), root, []string{"/bin/true"}, time.Second, policy); err == nil {
		t.Fatal("SDK escape admitted")
	}
}

func TestAppleStaleProgressDoesNotBlockNextRequest(t *testing.T) {
	for _, status := range []int{200, 409, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := fleetconfig.Configuration{Host: "apple", Version: "new", Fleet: fleetconfig.Fleet{Git: "/usr/bin/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"}, Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}}}}
			request := requeststore.Request{ID: "123-aabbccdd", Kind: "native_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)}
			next := 0
			api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), nativeTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/release"):
					return nativeResponse(status, map[string]any{}), nil
				case strings.HasSuffix(r.URL.Path, "/client-config"):
					return nativeResponse(200, intake.ConfigSnapshot{Version: "v", SettingsVersion: "s", ConfigurationVersion: "new", Config: encode(c.Fleet)}), nil
				case strings.HasSuffix(r.URL.Path, "/next"):
					next++
					return nativeResponse(200, map[string]any{}), nil
				default:
					return nativeResponse(200, map[string]any{"request": nativetest.RequestState{Request: request, Status: "claimed"}}), nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			a := Runner{Root: t.TempDir(), API: api, Config: c}
			e := a.stateExecutor()
			p := &requeststore.Progress{Request: request, Stage: "running", NativeVersion: "old"}
			if err = e.saveRequest(p); err != nil {
				t.Fatal(err)
			}
			err = a.cycle(context.Background())
			saved, loadErr := e.store.Load(request)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if status == 503 {
				if err == nil || saved.Stage == "done" || next != 0 {
					t.Fatalf("503 discarded progress: %+v %v", saved, err)
				}
				return
			}
			if err != nil || saved.Stage != "done" {
				t.Fatalf("stale state: %+v %v", saved, err)
			}
			if err = a.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if next != 1 {
				t.Fatalf("new request blocked: next=%d", next)
			}
		})
	}
}

func TestAppleHeartbeatReportsUnknownLoadAndNoImplementation(t *testing.T) {
	calls := 0
	api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), nativeTransport(func(r *http.Request) (*http.Response, error) {
		var status intake.HostStatus
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		calls++
		if status.Implementing != 0 || status.Load != nil || status.Capacity != 1 {
			t.Fatalf("invented load: %+v", status)
		}
		if calls == 1 && (!status.Ready || !status.Busy) {
			t.Fatal("test not busy")
		}
		if calls == 2 && (status.Ready || status.Busy) {
			t.Fatal("shutdown still ready")
		}
		return nativeResponse(200, map[string]any{}), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := Runner{API: api}
	if err = a.heartbeat(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if err = a.heartbeat(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestAppleRedistributedClaimUsesFreshTargetAndRetainsUncertainResult(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "same-target"
		if changed {
			name = "new-target"
		}
		t.Run(name, func(t *testing.T) {
			c := fleetconfig.Configuration{Host: "apple", Version: "new", Fleet: fleetconfig.Fleet{
				Git: "/missing/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"},
				Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}},
			}}
			old := requeststore.Request{ID: "123-aabbccdd", Kind: "native_test", Repo: "owner/project",
				Payload: encode(nativetest.Target{Host: "apple", Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)})}
			target := nativetest.Target{Host: "apple", Head: strings.Repeat("d", 40), Base: strings.Repeat("e", 40), Tree: strings.Repeat("f", 40)}
			if !changed {
				if err := json.Unmarshal(old.Payload, &target); err != nil {
					t.Fatal(err)
				}
			}
			fresh := old
			fresh.Payload = encode(target)
			later := fresh
			later.ID = "124-aabbccdd"
			current := old
			claims, accepted, releases := 0, 0, 0
			uncertain := true
			api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43),
				nativeTransport(func(r *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/client-config"):
						return nativeResponse(200, intake.ConfigSnapshot{Version: "v", SettingsVersion: "s", ConfigurationVersion: "new", Config: encode(c.Fleet)}), nil
					case strings.HasSuffix(r.URL.Path, "/release"):
						releases++
						return nativeResponse(200, map[string]any{}), nil
					case strings.HasSuffix(r.URL.Path, "/next"):
						claims++
						current = fresh
						if claims > 1 {
							current = later
						}
						return nativeResponse(200, map[string]any{"request": current}), nil
					case strings.HasSuffix(r.URL.Path, "/result"):
						var body struct{ Result nativetest.Result }
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body.Result.Head != target.Head || body.Result.Base != target.Base || body.Result.Tree != target.Tree || body.Result.Status != "unrunnable" {
							t.Fatalf("old attempt reused: %+v", body.Result)
						}
						if uncertain {
							return nativeResponse(503, map[string]any{}), nil
						}
						accepted++
						return nativeResponse(200, map[string]any{}), nil
					default:
						return nativeResponse(200, map[string]any{"request": nativetest.RequestState{Request: current, Status: "claimed"}}), nil
					}
				}))
			if err != nil {
				t.Fatal(err)
			}
			a := Runner{Root: t.TempDir(), API: api, Config: c}
			e := a.stateExecutor()
			if err := e.saveRequest(&requeststore.Progress{Request: old, Stage: "running", NativeVersion: "old"}); err != nil {
				t.Fatal(err)
			}
			if err := a.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := a.cycle(context.Background()); err == nil {
				t.Fatal("503 treated as success")
			}
			p, err := e.store.Load(fresh)
			if err != nil || p.Stage != "result" || len(p.Result) == 0 || claims != 1 {
				t.Fatalf("uncertain result lost: %+v %v", p, err)
			}
			uncertain = false
			if err := a.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := a.cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if accepted != 2 || releases != 1 || claims != 2 {
				t.Fatalf("redistribution blocked later work: accepted=%d releases=%d claims=%d", accepted, releases, claims)
			}
		})
	}
}
