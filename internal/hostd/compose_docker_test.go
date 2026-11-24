package hostd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the shipped mounts with disposable volumes, never the deployed state.
func TestComposeClaudeKeyDocker(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker is not installed")
	}
	root := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "DOCKER_CONFIG=" + filepath.Join(root, "docker-config")}
	probe := exec.Command(docker, "info")
	probe.Env = env
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("local Docker daemon unavailable: %s", out)
	}
	compose, err := filepath.Abs("../../deploy/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Unique project and volume names ensure no existing application volumes are used.
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	project := "keymount" + hex.EncodeToString(id[:])
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	toolsImage := project + "-tools:temporary"
	workerImage := project + "-worker:temporary"
	dockerRun := func(args ...string) ([]byte, error) {
		cmd := exec.Command(docker, args...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		if out, err := dockerRun("image", "rm", "-f", workerImage, toolsImage); err != nil {
			t.Logf("image cleanup: %v: %s", err, out)
		}
	})
	if out, err := dockerRun("build", "-f", filepath.Join(repo, "Dockerfile.tools"), "-t", toolsImage, repo); err != nil {
		t.Fatalf("build shipped tools image: %v: %s", err, out)
	}
	if out, err := dockerRun("build", "-f", filepath.Join(repo, "Dockerfile"), "--build-arg", "TOOLS_IMAGE="+toolsImage, "-t", workerImage, repo); err != nil {
		t.Fatalf("build shipped worker image: %v: %s", err, out)
	}
	override := filepath.Join(root, "override.yaml")
	content := `services:
  issue-desk-client:
    image: ` + workerImage + `
volumes:
  issue-desk-worker-state:
    external: false
    name: ` + project + `-state
  issue-desk-worker-codex:
    external: false
    name: ` + project + `-codex
  issue-desk-worker-cli:
    external: false
    name: ` + project + `-cli
  issue-desk-worker-agy:
    name: ` + project + `-agy
`
	if err := os.WriteFile(override, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	envfile := filepath.Join(root, "env")
	if err := os.WriteFile(envfile, []byte("REGISTRY=example.invalid\nISSUE_DESK_CLIENT_TAG=test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"compose", "--project-name", project, "--project-directory", root, "--env-file", envfile, "-f", compose, "-f", override}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(docker, append(append([]string{}, base...), args...)...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		if out, err := run("down", "--volumes", "--remove-orphans"); err != nil {
			t.Errorf("cleanup: %v: %s", err, out)
		}
	})
	cmd := exec.Command(docker, append(append([]string{}, base...), "config", "--format", "json")...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve shipped compose: %v: %s", err, stderr.String())
	}
	var config struct {
		Services map[string]struct {
			StopGracePeriod string `json:"stop_grace_period"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("decode compose config: %v: %s", err, out)
	}
	grace, err := time.ParseDuration(config.Services["issue-desk-client"].StopGracePeriod)
	if err != nil || grace != 60*time.Second {
		t.Fatalf("effective stop grace = %q, want 60s: %v", config.Services["issue-desk-client"].StopGracePeriod, err)
	}
	worker := filepath.Join(root, "synthetic-worker")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\ntest \"$$\" -eq 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err = dockerRun("run", "--rm", "--init=false", "--mount", "type=bind,src="+worker+",dst=/usr/local/bin/worker,readonly", workerImage)
	if err != nil {
		t.Fatalf("shipped entrypoint did not replace its shell: %v: %s", err, out)
	}
	out, err = run("run", "--rm", "--no-deps", "--entrypoint", "sh", "issue-desk-client", "-c", `test "$ISSUE_DESK_SANDBOX_TMPFS_DIR" = /sandbox-tmp && test "$(stat -f -c %T /sandbox-tmp)" = tmpfs && test -c /state/private/claude-api-key && test ! -f /state/private/claude-api-key && test "$(stat -c %u /state/private)" = 0 && awk '$5 == "/state/private/claude-api-key" { split($6, o, ","); if (o[1] == "ro") found = 1 } END { exit !found }' /proc/self/mountinfo`)
	if err != nil {
		t.Fatalf("default /dev/null mount and fresh volume ownership: %v: %s", err, out)
	}
	missing := filepath.Join(root, "missing-key")
	if err := os.WriteFile(envfile, []byte("REGISTRY=example.invalid\nISSUE_DESK_CLIENT_TAG=test\nCLAUDE_API_KEY_FILE="+missing+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = run("run", "--rm", "--no-deps", "--entrypoint", "true", "issue-desk-client")
	if err == nil || !strings.Contains(string(out), missing) {
		t.Fatalf("missing bind source should fail: %v: %s", err, out)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing bind source was created: %v", err)
	}
}
