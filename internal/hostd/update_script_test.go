package hostd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A hostd cron has only system tools on PATH. Host Node is not required;
// docker's Node calls run inside the pinned worker image.
func TestLinuxUpdateWithoutHostNode(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	client := filepath.Join(root, "client")
	for _, p := range []string{bin, client} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"python3", "flock", "stat", "mktemp", "rm", "cp", "mv"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	docker := `
import base64,json,os,sys
args=sys.argv[1:]
if args[0]=='compose':
 if 'config' in args:
  assert 'CLAUDE_API_KEY_FILE' not in os.environ
  envfile=args[args.index('--env-file')+1]
  with open(envfile) as f: env=f.read()
  assert 'CLAUDE_API_KEY_FILE=/synthetic/from-env' in env
  print(json.dumps({'services':{'issue-desk-client':{'image':'registry.invalid:5443/issue-desk-client:aaaaaaaaaaaa'}}}))
 elif 'ps' in args: pass
 elif 'up' in args:
  with open(os.environ['TEST_LOG'],'a') as f:f.write('up\n')
  if os.environ.get('TEST_FAIL_UP') and not os.path.exists(os.environ['TEST_FAIL_UP']):
   open(os.environ['TEST_FAIL_UP'],'w').close()
   sys.stderr.write('synthetic up failure: port is already allocated\n')
   raise SystemExit(1)
 else: raise SystemExit('unexpected compose')
elif args[0]=='ps':
 print('old-entry')
elif args[0]=='stop':
 assert args[1:] == ['old-entry']
 with open(os.environ['TEST_LOG'],'a') as f:f.write('stop-entry\n')
elif args[0]=='pull':
 with open(os.environ['DOCKER_CONFIG']+'/config.json') as f:auth=json.load(f)['auths']['registry.invalid:5443']['auth']
 assert base64.b64decode(auth).decode()=='issue-desk-reader:synthetic-password'
 with open(os.environ['TEST_LOG'],'a') as f:f.write('auth-ok\n')
else: raise SystemExit('unexpected docker')
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!"+python+"\n"+docker), 0700); err != nil {
		t.Fatal(err)
	}
	env := "CLAUDE_API_KEY_FILE=/synthetic/from-env\nCODEX_AUTH_DIR=/home/synthetic/.codex\nWEBHOOK_BIND=192.0.2.1\nWEBHOOK_PORT=8790\nISSUE_DESK_CLIENT_TAG=aaaaaaaaaaaa\nexport ISSUE_DESK_CLIENT_BLUE_TAG=aaaaaaaaaaaa\nISSUE_DESK_CLIENT_GREEN_TAG=cccccccccccc\n"
	if err := os.WriteFile(filepath.Join(client, ".env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../deploy/update-linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "log")
	run := func(choice string) {
		t.Helper()
		cmd := exec.Command(bash, script, client, choice)
		cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "TEST_LOG=" + log, "CLAUDE_API_KEY_FILE=/synthetic/from-caller"}
		cmd.Stdin = strings.NewReader("synthetic-password\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", choice, err, out)
		}
	}
	run("--mark-green-stopped")
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("compatibility request invoked docker", err)
	}
	run("bbbbbbbbbbbb")
	current, err := os.ReadFile(filepath.Join(client, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"CLAUDE_API_KEY_FILE=/synthetic/from-env", "CODEX_AUTH_DIR=/home/synthetic/.codex", "WEBHOOK_BIND=192.0.2.1", "WEBHOOK_PORT=8790", "ISSUE_DESK_CLIENT_TAG=bbbbbbbbbbbb", "ISSUE_DESK_CLIENT_BLUE_TAG=bbbbbbbbbbbb", "ISSUE_DESK_CLIENT_GREEN_TAG=cccccccccccc"} {
		if !strings.Contains(string(current), line+"\n") {
			t.Fatalf("missing %s: %s", line, current)
		}
	}
	if strings.Contains(string(current), "aaaaaaaaaaaa") {
		t.Fatal("old tag retained")
	}
	run("--restore-previous")
	restored, err := os.ReadFile(filepath.Join(client, ".env"))
	if err != nil || string(restored) != env {
		t.Fatalf("restore mismatch: %s %v", restored, err)
	}
	recorded, err := os.ReadFile(log)
	if err != nil || string(recorded) != "auth-ok\nstop-entry\nup\nstop-entry\nup\n" {
		t.Fatalf("calls: %s %v", recorded, err)
	}
	// A failed start restores the saved environment, but does not restore compose.yaml.
	composePath := filepath.Join(client, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("new compose"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, script, client, "dddddddddddd")
	cmd.Env = []string{"PATH=" + bin, "HOME=" + root, "TEST_LOG=" + log, "CLAUDE_API_KEY_FILE=/synthetic/from-caller", "TEST_FAIL_UP=" + filepath.Join(root, "failed")}
	cmd.Stdin = strings.NewReader("synthetic-password\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("start unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "docker: synthetic up failure: port is already allocated") {
		t.Fatalf("docker stderr dropped: %s", out)
	}
	restored, err = os.ReadFile(filepath.Join(client, ".env"))
	if err != nil || string(restored) != env {
		t.Fatalf("failed start restore mismatch: %v", err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil || string(compose) != "new compose" {
		t.Fatalf("compose changed during restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(client, ".linux-update.pending")); !os.IsNotExist(err) {
		t.Fatalf("pending retained: %v", err)
	}
}

// Check the shipped compose file, including its default, rather than a fixture.
func TestLinuxComposeClaudeKeyMount(t *testing.T) {
	data, err := os.ReadFile("../../deploy/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mount := "      - type: bind\n        source: ${CLAUDE_API_KEY_FILE:-/dev/null}\n        target: /state/private/claude-api-key\n        read_only: true\n        bind:\n          create_host_path: false\n"
	if strings.Count(string(data), mount) != 1 {
		t.Fatal("shipped single-file read-only mount is missing")
	}
	if strings.Contains(string(data), ":/state/private\n") {
		t.Fatal("private directory mounted")
	}
}
