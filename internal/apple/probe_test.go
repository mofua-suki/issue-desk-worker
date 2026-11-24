package apple

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/intake"
)

// probeRunner は、配布された設定でテスト専用Macの対象リポジトリを1件持ち、外部のsandbox-execだけを
// そのままコマンドを実行する偽物に差し替えた Runner を作る。
func probeRunner(t *testing.T, command []string) *Runner {
	t.Helper()
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	fleet := fleetconfig.Fleet{Git: "/usr/bin/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", TestTimeoutSeconds: 5,
		Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: command}}}
	fleet.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
	fleet.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	fleet.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
	c, err := fleetconfig.DecodeApple(intake.ConfigSnapshot{Config: encode(fleet), ConfigurationVersion: "v"}, "apple")
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Root: filepath.Join(root, "runtime"), sandbox: sandbox, Config: c}
}

func TestAppleProbeRunsDistributedCommandInGivenTree(t *testing.T) {
	runner := probeRunner(t, []string{"/bin/sh", "-c", "cat marker; echo problem >&2; exit 3"})
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "marker"), []byte("in-tree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Probe(context.Background(), "owner/project", tree)
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != 3 || strings.TrimSpace(out.Out) != "in-tree" || !strings.Contains(out.Err, "problem") {
		t.Fatalf("result: %+v", out)
	}
}

func TestAppleProbeRejectsUnassignedRepositoryAndMissingTree(t *testing.T) {
	runner := probeRunner(t, []string{"/bin/true"})
	if _, err := runner.Probe(context.Background(), "owner/other", t.TempDir()); err == nil {
		t.Fatal("unassigned repository accepted")
	}
	if _, err := runner.Probe(context.Background(), "owner/project", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing tree accepted")
	}
}
