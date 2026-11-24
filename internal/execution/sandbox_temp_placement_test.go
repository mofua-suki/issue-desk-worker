package execution

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

func sandboxPlacementFixture(t *testing.T) (*Executor, *journal.Record, string) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "runtime", "workspaces", "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "sandbox")
	script := `#!/bin/sh
printf 'temp=%s\ncache=%s\nmod=%s\npath=%s\nconfig=%s\nxdgcache=%s\ndata=%s\nstate=%s\n' "$TMPDIR" "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME"
printf 'args=%s\n' "$*"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$XDG_CACHE_HOME" || exit 2
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: filepath.Join(root, "runtime"), WorkerTokenFile: filepath.Join(root, "private", "token"), Codex: executable}}}
	return e, &journal.Record{Tree: tree}, root
}

func placementRun(t *testing.T, e *Executor, r *journal.Record) (string, string) {
	t.Helper()
	out, err := e.sandbox(context.Background(), r, []string{"/bin/true"}, 10)
	if err != nil || out.Code != 0 {
		t.Fatalf("sandbox: %v code=%d %s %s", err, out.Code, out.Out, out.Err)
	}
	lines := strings.Split(out.Out, "\n")
	if len(lines) < 9 || !strings.HasPrefix(lines[0], "temp=") {
		t.Fatalf("unexpected output %q", out.Out)
	}
	temp := strings.TrimPrefix(lines[0], "temp=")
	for _, line := range lines[1:8] {
		_, value, ok := strings.Cut(line, "=")
		if !ok || !containsPath(temp, value) {
			t.Fatalf("environment outside temporary directory: %q", line)
		}
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temporary directory remains: %s (%v)", temp, err)
	}
	return temp, out.Out
}

func TestSandboxTempPlacement(t *testing.T) {
	e, r, root := sandboxPlacementFixture(t)
	tmpfs, err := os.MkdirTemp("/dev/shm", "worker-placement-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpfs)
	t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", tmpfs)
	temp, output := placementRun(t, e, r)
	if filepath.Dir(temp) != tmpfs {
		t.Fatalf("expected tmpfs, got %s", temp)
	}
	for path, permission := range map[string]string{filepath.Join(root, "private"): "deny", filepath.Join(r.Tree, ".git"): "deny", temp: "write"} {
		if !strings.Contains(output, strconv.Quote(path)+"="+strconv.Quote(permission)) {
			t.Fatalf("sandbox policy missing %s=%s", path, permission)
		}
	}
	t.Run("unwritable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses Unix directory mode bits")
		}
		unwritable := filepath.Join(tmpfs, "unwritable")
		if err := os.Mkdir(unwritable, 0500); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", unwritable)
		temp, _ = placementRun(t, e, r)
		if filepath.Dir(temp) != filepath.Join(e.Config.RuntimeDir, "workspaces") {
			t.Fatalf("unwritable tmpfs did not fall back: %s", temp)
		}
		if err := os.Remove(unwritable); err != nil {
			t.Fatal(err)
		}

	})

	for _, candidate := range []string{filepath.Join(root, "missing"), root, filepath.Join(root, "sandbox")} {
		t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", candidate)
		temp, _ = placementRun(t, e, r)
		if filepath.Dir(temp) != filepath.Join(e.Config.RuntimeDir, "workspaces") {
			t.Fatalf("expected fallback, got %s", temp)
		}
	}
	t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", tmpfs)
	// process.CleanEnv deliberately does not inherit arbitrary variables; use a fixture script that signals a full tmpfs.
	script := "#!/bin/sh\ncase \"$TMPDIR\" in " + tmpfs + "/*) echo 'no space left on device' >&2; exit 1;; esac\nprintf 'temp=%s\\ncache=%s\\nmod=%s\\npath=%s\\nconfig=%s\\nxdgcache=%s\\ndata=%s\\nstate=%s\\nargs=%s\\n' \"$TMPDIR\" \"$GOCACHE\" \"$GOMODCACHE\" \"$GOPATH\" \"$XDG_CONFIG_HOME\" \"$XDG_CACHE_HOME\" \"$XDG_DATA_HOME\" \"$XDG_STATE_HOME\" \"$*\"\n"
	if err := os.WriteFile(e.Config.Codex, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	temp, _ = placementRun(t, e, r)
	if filepath.Dir(temp) != filepath.Join(e.Config.RuntimeDir, "workspaces") {
		t.Fatalf("full tmpfs did not fall back: %s", temp)
	}
	entries, err := os.ReadDir(tmpfs)
	if err != nil || len(entries) != 0 {
		t.Fatalf("tmpfs not cleaned: %v %v", entries, err)
	}
}

func TestSandboxTempNotReusedAcrossRuns(t *testing.T) {
	e, r, _ := sandboxPlacementFixture(t)
	tmpfs, err := os.MkdirTemp("/dev/shm", "worker-placement-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpfs)
	for _, candidate := range []string{tmpfs, ""} {
		t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", candidate)
		first, _ := placementRun(t, e, r)
		second, _ := placementRun(t, e, r)
		if first == second {
			t.Fatal("temporary directory reused")
		}
		entries, err := os.ReadDir(filepath.Dir(first))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".test-") {
				t.Fatalf("cache remains: %s", entry.Name())
			}
		}
	}
}
