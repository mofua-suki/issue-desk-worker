package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestTestSandboxEnvIsNonInteractive(t *testing.T) {
	temp := filepath.Join("work", "tmp")
	env := testSandboxEnv(temp)
	if env["CI"] != "true" {
		t.Fatalf("CI = %q, want true (pnpm must not stop at a prompt when there is no terminal)", env["CI"])
	}
	if env["TEMP"] != temp || env["TMPDIR"] != temp {
		t.Fatalf("TEMP = %q, TMPDIR = %q, want %q", env["TEMP"], env["TMPDIR"], temp)
	}
	if env["TZ"] != "UTC" || env["GOTOOLCHAIN"] != "local" {
		t.Fatalf("TZ = %q, GOTOOLCHAIN = %q, want UTC and local", env["TZ"], env["GOTOOLCHAIN"])
	}
	if env["GRADLE_USER_HOME"] != filepath.Join(temp, "gradle") || env["PUB_CACHE"] != filepath.Join(temp, "pub-cache") || env["JAVA_HOME"] != providers.JavaHome || env["JAVA_TOOL_OPTIONS"] != "-Djava.io.tmpdir="+temp || env["FLUTTER_SUPPRESS_ANALYTICS"] != "true" {
		t.Fatalf("JVM と Flutter のキャッシュが一時領域の外です: %v", env)
	}
	if env["XDG_CACHE_HOME"] != filepath.Join(temp, "cache") || env["GOMODCACHE"] != filepath.Join(temp, "go-mod") {
		t.Fatalf("cache directories moved away from the sandbox temp: %v", env)
	}
}

func TestChromiumProbeFailureIsEnvironment(t *testing.T) {
	for _, tc := range []struct{ name, file, content string }{{"test", "", ""}, {"go-format", "go.mod", "module test\n"}, {"prettier", "package.json", `{ "devDependencies": { "prettier": "1" } }`}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			dir := t.TempDir()
			caddy, chromium, codex := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium"), filepath.Join(dir, "codex")
			for _, path := range []string{caddy, chromium} {
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(codex, []byte("#!/bin/sh\ncase $* in *--dump-dom*) exit 1;; esac\nprintf '%s\\n' '"+strings.Repeat("x", 5000)+"'\nprintf 'fixed-test-ran: %s %s\\n' \"${ISSUE_DESK_TEST_CADDY-unset}\" \"${ISSUE_DESK_TEST_CHROMIUM-unset}\"\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Codex = codex
			f.Executor.browserTools = func() (string, string) { return caddy, chromium }
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.file == "package.json" {
				f.Executor.Config.PNPM = "/bin/true"
			}
			calls := 0
			f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, nil
			}
			r := &journal.Record{ID: 1, Repo: "mofua-suki/issue-desk-intake", Tree: dir, Generation: 1, Epoch: 3, Stage: "test", Result: json.RawMessage(`{}`)}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			result, err := f.Executor.implementationTests(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project"}}, Repository{TestCommand: []string{"/bin/true"}, TestTimeoutSeconds: 10}, r, "")
			if err != nil || result != nil || calls != 0 || r.FixedTestFailures != 0 {
				t.Fatalf("result=%+v err=%v calls=%d failures=%d", result, err, calls, r.FixedTestFailures)
			}
			var tested TestResult
			if err := json.Unmarshal(r.Test, &tested); err != nil || !tested.Passed || tested.Unavailable || !strings.Contains(tested.Detail, "Chromium") || !strings.Contains(tested.Detail, "fixed-test-ran: unset unset") {
				t.Fatalf("test=%s err=%v", r.Test, err)
			}
		})
	}
	t.Run("actual test failure remains countable", func(t *testing.T) {
		dir := t.TempDir()
		caddy, chromium, codex := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium"), filepath.Join(dir, "codex")
		for _, path := range []string{caddy, chromium} {
			if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(codex, []byte("#!/bin/sh\ncase $* in *--dump-dom*) exit 1;; esac\nexit 2\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
			t.Fatal(err)
		}
		e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: dir, Codex: codex}}}
		e.browserTools = func() (string, string) { return caddy, chromium }
		result, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/false"}, TestTimeoutSeconds: 10}, &journal.Record{Repo: "mofua-suki/issue-desk-intake", Tree: dir, Result: json.RawMessage(`{}`)})
		if err != nil || result.Passed || !result.countableFailure || result.Unavailable || !strings.Contains(result.Detail, chromiumProbeWarning) {
			t.Fatalf("result=%+v countable=%v err=%v", result, result.countableFailure, err)
		}
	})
}

func TestSandboxEnvIntakeBrowserTools(t *testing.T) {
	dir := t.TempDir()
	caddy, chromium := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium")
	for _, path := range []string{caddy, chromium} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	check := func(repo, c, browser string, want bool) {
		t.Helper()
		env := testSandboxEnv(dir)
		addIntakeBrowserTools(env, &journal.Record{Repo: repo}, c, browser, func(string) error { return nil })
		for key, path := range map[string]string{"ISSUE_DESK_TEST_CADDY": c, "ISSUE_DESK_TEST_CHROMIUM": browser} {
			value, ok := env[key]
			if ok != want || (want && (value != path || !filepath.IsAbs(value))) {
				t.Errorf("repo %q: %s = %q, present %v; want present %v with path %q", repo, key, value, ok, want, path)
			}
		}
	}
	check("mofua-suki/issue-desk-intake", caddy, chromium, true)
	check("owner/project", caddy, chromium, false)
	check("mofua-suki/issue-desk-intake", filepath.Join(dir, "missing"), chromium, false)
	check("mofua-suki/issue-desk-intake", caddy, filepath.Join(dir, "missing"), false)

	// Exercise the production sandbox entry and process environment with the deployed paths.
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\ncase $* in *--dump-dom*) exit 0;; esac\nprintf '%s\\n' \"${ISSUE_DESK_TEST_CADDY-unset}\" \"${ISSUE_DESK_TEST_CHROMIUM-unset}\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: runtimeDir, Codex: sandbox}}}
	for _, repo := range []string{"mofua-suki/issue-desk-intake", "owner/project"} {
		out, err := e.sandbox(context.Background(), &journal.Record{Repo: repo, Tree: dir}, []string{"/bin/true"}, 10)
		if err != nil || out.Code != 0 {
			t.Fatalf("sandbox %s: %v, code %d", repo, err, out.Code)
		}
		want := "unset\nunset"
		if repo == "mofua-suki/issue-desk-intake" {
			env := testSandboxEnv(dir)
			addIntakeBrowserTools(env, &journal.Record{Repo: repo}, "/opt/agent-tools/bin/caddy", "/usr/lib/chromium/chromium-headless-shell", func(string) error { return nil })
			if env["ISSUE_DESK_TEST_CADDY"] != "" {
				want = "/opt/agent-tools/bin/caddy\n/usr/lib/chromium/chromium-headless-shell"
			}
		}
		if got := strings.TrimSpace(out.Out); got != want {
			t.Errorf("sandbox %s: got %q, want %q", repo, got, want)
		}
	}
}

func TestChromiumProbeGatesBrowserTools(t *testing.T) {
	dir := t.TempDir()
	caddy, chromium, codex := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium"), filepath.Join(dir, "codex")
	for _, path := range []string{caddy, chromium} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, code string
		success    bool
	}{{"success", "0", true}, {"failure", "1", false}} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\ncase $* in *--dump-dom*) exit " + tc.code + ";; esac\nprintf '%s\\n' \"${ISSUE_DESK_TEST_CADDY-unset}\" \"${ISSUE_DESK_TEST_CHROMIUM-unset}\"\n"
			if err := os.WriteFile(codex, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: dir, Codex: codex}}}
			out, err := e.runSandboxTempWithBrowserTools(context.Background(), &journal.Record{Repo: "mofua-suki/issue-desk-intake", Tree: dir}, []string{"/bin/true"}, 10, filepath.Join(dir, "tmp"), caddy, chromium)
			if tc.success {
				if err != nil || strings.TrimSpace(out.Out) != caddy+"\n"+chromium {
					t.Fatalf("output %q, error %v", out.Out, err)
				}
			} else if err != nil || strings.TrimSpace(out.Out) != "unset\nunset" || !strings.Contains(out.Err, "Chromium") {
				t.Fatalf("output %q, stderr %q, error %v", out.Out, out.Err, err)
			}
		})
	}
}
