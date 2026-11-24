#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd -P)
python3 - "$here/install.sh" <<'PY'
import json,os,pathlib,plistlib,stat,subprocess,sys,tempfile
installer=sys.argv[1]
with tempfile.TemporaryDirectory() as tmp:
 root=pathlib.Path(tmp); home=root/'home space &'; home.mkdir(); bin=root/'bin'; bin.mkdir(); work=root/'work & space'; work.mkdir()
 subprocess.run(['git','init','-q',str(work)],check=True)
 subprocess.run(['git','-C',str(work),'remote','add','origin','https://example.org/a.git'],check=True)
 exe=root/'hostd &'; exe.write_text('fake'); exe.chmod(0o700)
 def stub(name,body):
  p=bin/name; p.write_text('#!/bin/bash\n'+body+'\n'); p.chmod(0o755)
 stub('uname','if [[ $1 == -s ]]; then echo "${TEST_OS:-Darwin}"; else echo "${TEST_CPU:-arm64}"; fi')
 stub('file','echo "${TEST_FILE:-Mach-O 64-bit executable arm64}"')
 stub('launchctl',r'''echo "$1" >> "$HOME/calls"
case "$1" in
 bootout) if [[ ${FAIL_BOOTOUT:-} ]]; then echo 'permission denied' >&2; exit 1; fi
          if [[ -f "$HOME/registered" ]]; then rm "$HOME/registered"; else echo 'Could not find service' >&2; exit 1; fi ;;
 bootstrap) [[ ! ${FAIL_BOOTSTRAP:-} ]] || exit 1; touch "$HOME/registered" ;;
esac''')
 env=dict(os.environ,HOME=str(home),PATH=str(bin)+os.pathsep+os.environ['PATH'])
 token='idh_'+'a'*43
 args=[str(exe),'mac-1','https://example.org','owner/repo',str(work),'60','true','echo','a & "b"']
 def run(a=args,t=token,extra=None):
  e=env.copy(); e.update(extra or {}); return subprocess.run(['bash',installer,*a],input=t+'\n',text=True,capture_output=True,env=e)
 def calls():
  p=home/'calls'; return p.read_text().splitlines() if p.exists() else []
 def reject(a=args,t=token,extra=None):
  before=calls().count('bootstrap'); r=run(a,t,extra); assert r.returncode != 0 and calls().count('bootstrap')==before and '登録しました' not in r.stdout,(r.stdout,r.stderr)
 for invalid in ['',token+'x','idh_'+'a'*42,'idh_'+'a'*42+'!'] : reject(t=invalid)
 for index,value in [(2,'http://example.org'),(0,str(root/'missing')),(4,str(root/'missing'))]:
  a=args.copy(); a[index]=value; reject(a)
 reject(extra={'TEST_FILE':'Mach-O 64-bit executable x86_64'})
 reject(extra={'TEST_OS':'Linux'}); reject(extra={'TEST_CPU':'x86_64'})
 noorigin=root/'no origin'; noorigin.mkdir(); subprocess.run(['git','init','-q',str(noorigin)],check=True)
 a=args.copy(); a[4]=str(noorigin); reject(a)
 r=run(); assert r.returncode==0,(r.stdout,r.stderr)
 base=home/'Library/Application Support/issue-desk/live-check'; agent=home/'Library/LaunchAgents/com.issue-desk.live-check.plist'
 cfg=json.loads((base/'config.json').read_text()); plist=plistlib.loads(agent.read_bytes())
 assert cfg==dict(url=args[2],host=args[1],tokenFile=str(base/'token'),stateFile=str(base/'state.json'),liveChecks=[dict(repo=args[3],path=args[4],command=args[7:],timeoutSeconds=60,checkout=True)])
 assert stat.S_IMODE((base/'token').stat().st_mode)==0o600 and stat.S_IMODE(base.stat().st_mode)==0o700 and base.stat().st_uid==os.getuid()
 assert plist['ProgramArguments']==[str(exe),'live-check','--config',str(base/'config.json')] and plist['StartInterval']==300
 assert token not in (base/'config.json').read_text() and token not in agent.read_text() and token not in r.stdout+r.stderr
 assert plist['StandardOutPath']=='/dev/null' and plist['StandardErrorPath']=='/dev/null'
 r=run(); assert r.returncode==0 and calls()[-2:]==['bootout','bootstrap'] and (home/'registered').exists()
 reject(extra={'FAIL_BOOTOUT':'1'})
 r=run(extra={'FAIL_BOOTSTRAP':'1'}); assert r.returncode!=0 and '登録しました' not in r.stdout
print('install tests passed')
PY
