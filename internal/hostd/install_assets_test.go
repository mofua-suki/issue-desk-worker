package hostd

import (
	"context"
	"issue-desk-worker/internal/process"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Execute the shipped update script with the shipped compose file.
// Only Docker's daemon boundary is simulated; hostd and bash run normally.
func TestInstallRunsShippedLinuxAssets(t *testing.T) {
	for _, failure := range []string{"", "pull", "switch", "ready"} {
		t.Run("failure="+failure, func(t *testing.T) {
			runInstallWithShippedLinuxAssets(t, failure)
		})
	}
}

func runInstallWithShippedLinuxAssets(t *testing.T, failure string) {
	t.Helper()
	f := newInstallFixture(t)
	f.FailReady = failure == "ready"
	client := f.Runtime.Config.ClientDir
	for _, name := range []string{"update-linux.sh", "compose.yaml"} {
		raw, err := os.ReadFile(filepath.Join("../../deploy", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(client, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial := "REGISTRY=" + f.Target.Registry + "\nFLAVOR=\nISSUE_DESK_CLIENT_TAG=" + strings.Repeat("a", 12) + "\n"
	if err := os.WriteFile(filepath.Join(client, ".env"), []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(python, filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	// This fake models immutable local image digests and a container separately,
	// so inspecting the pulled image cannot stand in for inspecting the worker.
	docker := `
import json,os,sys
args=sys.argv[1:]
root=os.environ['INSTALL_TEST_CLIENT']
log=os.environ['INSTALL_TEST_LOG']
statefile=os.environ['INSTALL_TEST_STATE']
failure=os.environ['INSTALL_TEST_FAILURE']
try:
 with open(statefile) as f: state=json.load(f)
except FileNotFoundError: state={'running':True}
with open(root+'/.env') as f:
 env=dict(line.strip().split('=',1) for line in f if '=' in line)
registry=env['REGISTRY']
tag=env.get('ISSUE_DESK_CLIENT_BLUE_TAG',env['ISSUE_DESK_CLIENT_TAG'])
image=registry+'/issue-desk-client:'+tag
digest='sha256:'+'d'*64
cid='c'*64
def record(value):
 with open(log,'a') as f:f.write(value+'\n')
if args[0]=='compose':
 assert args[args.index('--project-directory')+1]==root
 assert args[args.index('--env-file')+1]==root+'/.env'
 assert args[args.index('-f')+1]==root+'/compose.yaml'
 if 'config' in args:
  print(json.dumps({'services':{'issue-desk-client':{'image':image}}}))
 elif 'ps' in args: print(cid)
 elif 'stop' in args:
  assert args[-4:]==['stop','--timeout','60','issue-desk-client']
  state['running']=False
  record('stop60')
 elif 'up' in args:
  assert args[-6:]==['up','-d','--no-deps','--pull','never','issue-desk-client']
  record('up:'+tag)
  if failure=='switch' and tag=='b'*12: raise SystemExit(1)
  state['running']=True
 else: raise SystemExit('unexpected compose')
elif args[0]=='pull':
 assert args[1]==registry+'/issue-desk-client:'+'b'*12
 with open(os.environ['DOCKER_CONFIG']+'/config.json') as f: assert registry in json.load(f)['auths']
 record('pull')
 if failure=='pull': raise SystemExit(1)
elif args[0]=='ps': pass
elif args[0]=='inspect':
 if args[2]=='{{json .State}}':
  record('inspect-stopped')
  print(json.dumps({'Running':state['running'],'Restarting':False,'Status':'running' if state['running'] else 'exited'}))
 elif args[2]=='{{json .}}':
  print(json.dumps({'Image':'sha256:'+'e'*64,'State':{'Running':state['running']}}))
 else: raise SystemExit('unexpected inspect')
elif args[:2]==['image','inspect']:
 print(json.dumps([registry+'/issue-desk-client@'+digest]))
else: raise SystemExit('unexpected docker')
with open(statefile,'w') as f:json.dump(state,f)
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!"+python+"\n"+docker), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(bin, "events")
	f.Runtime.Run = func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
		options.Env["PATH"] = bin + ":/usr/bin:/bin"
		options.Env["INSTALL_TEST_CLIENT"] = client
		options.Env["INSTALL_TEST_LOG"] = log
		options.Env["INSTALL_TEST_STATE"] = filepath.Join(bin, "container.json")
		options.Env["INSTALL_TEST_FAILURE"] = failure
		executable := name
		if name == "docker" {
			executable = filepath.Join(bin, "docker")
		}
		result, err := process.Run(ctx, executable, args, options)
		if err != nil || result.Code != 0 {
			t.Logf("synthetic subprocess %s: %v, stderr=%s", name, err, result.Err)
		}
		if name == "bash" && len(args) == 3 && args[2] == f.Target.Tag && err == nil && result.Code == 0 {
			f.Starts++
			f.Now = f.Now.Add(time.Second)
		}
		return result, err
	}
	installCycles(t, f)
	reason := "done"
	switch failure {
	case "pull":
		reason = "tag_missing"
	case "switch":
		reason = "switch_failed"
	case "ready":
		reason = "verify_failed"
	}
	requireCommandReason(t, f, reason)
	if f.Drain != nil {
		t.Fatal("shipped lifecycle left acquisition stopped")
	}
	current, err := f.Runtime.environment()
	wantTag := f.Target.Tag
	if failure != "" {
		wantTag = strings.Repeat("a", 12)
	}
	if err != nil || current.Tag != wantTag {
		t.Fatal("shipped lifecycle left wrong environment", current, err)
	}
	previous, err := os.ReadFile(filepath.Join(client, ".env.linux.previous"))
	if failure == "pull" {
		if !os.IsNotExist(err) {
			t.Fatal("pull failure changed previous environment", err)
		}
	} else if err != nil || string(previous) != initial {
		t.Fatal("shipped script did not preserve old environment", err)
	}
	if _, err := os.Stat(filepath.Join(client, ".linux-update.pending")); !os.IsNotExist(err) {
		t.Fatal("shipped lifecycle retained pending switch", err)
	}
	events, err := os.ReadFile(log)
	wantEvents := "pull\nstop60\ninspect-stopped\ninspect-stopped\npull\nup:" + f.Target.Tag + "\n"
	if failure == "pull" {
		wantEvents = "pull\n"
	} else if failure == "switch" {
		// update-linux.sh itself restores on failed up; hostd then ensures
		// that the unchanged old environment has a running worker.
		wantEvents += "up:aaaaaaaaaaaa\nup:aaaaaaaaaaaa\n"
	} else if failure == "ready" {
		wantEvents += "up:aaaaaaaaaaaa\n"
	}
	if err != nil || string(events) != wantEvents {
		t.Fatalf("shipped lifecycle: %s (%v)", events, err)
	}
}
