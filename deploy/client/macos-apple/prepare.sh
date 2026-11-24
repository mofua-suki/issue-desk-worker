#!/bin/bash
# Prepare user-owned artifacts only. Never issue tokens, change permissions,
# modify Docker configuration, or replace an existing Worker.
set -euo pipefail
role= config= implementation_config= worker=
start=false
compose=
pc_token= gitea_token= host_image_token= codex_auth= claude_auth= flavor=-python
script_dir=$(cd "$(dirname "$0")" && pwd)

while (($#)); do
  case "$1" in
    --pc-token-file) pc_token=${2:?}; shift 2 ;;
    --gitea-token-file) gitea_token=${2:?}; shift 2 ;;
    --host-image-token-file) host_image_token=${2:?}; shift 2 ;;
    --codex-auth-file) codex_auth=${2:?}; shift 2 ;;
    --claude-auth-file) claude_auth=${2:?}; shift 2 ;;
    --flavor) flavor=${2:?}; shift 2 ;;
    --flavor=*) flavor=${1#--flavor=}; shift ;;
    --start) start=true; shift ;;
    --compose) [[ $# -ge 2 ]] || { echo "--composeのパスがありません" >&2; exit 2; }; compose=$2; shift 2 ;;
    --role) [[ $# -ge 2 ]] || { echo "--roleの値がありません" >&2; exit 2; }; role=${2:?}; shift 2 ;;
    --config) [[ $# -ge 2 ]] || { echo "--configの値がありません" >&2; exit 2; }; config=${2:?}; shift 2 ;;
    --implementation-config) [[ $# -ge 2 ]] || { echo "--implementation-configの値がありません" >&2; exit 2; }; implementation_config=${2:?}; shift 2 ;;
    --worker) [[ $# -ge 2 ]] || { echo "--workerの値がありません" >&2; exit 2; }; worker=${2:?}; shift 2 ;;
    *) echo '使い方: prepare.sh --role test-only|implementation --config APPLE_K2 --worker /absolute/worker [--implementation-config LINUX_K2]' >&2; exit 2 ;;
  esac
done
[[ $(uname -s) == Darwin ]] || { echo 'Darwin専用です' >&2; exit 1; }
[[ $role == test-only || $role == implementation ]] || { echo "--roleはtest-onlyまたはimplementationを指定してください" >&2; exit 2; }
[[ -f $config && -n $worker && $worker == /* && -x $worker ]] || { echo "既存K2と実行可能な絶対パスのWorkerが必要です" >&2; exit 2; }
command -v python3 >/dev/null || { echo "python3が必要です" >&2; exit 1; }
if [[ $role == implementation && -z $implementation_config ]]; then
  echo 'implementationでは別IDのLinux用K2を--implementation-configで指定してください' >&2; exit 2
fi
python3 - "$role" "$config" "$implementation_config" "$worker" "$start" "$compose" "$script_dir" "$pc_token" "$gitea_token" "$host_image_token" "$codex_auth" "$claude_auth" "$flavor" <<'PY'
import json, pathlib, plistlib, re, shlex, sys, os, subprocess, tempfile, atexit
def diagnose(kind, value, tb):
    if isinstance(value, subprocess.CalledProcessError):
        print('コマンドが失敗しました: '+str(value.cmd[0])+' (終了コード '+str(value.returncode)+')', file=sys.stderr)
    else:
        print('セットアップに失敗しました: ' + str(value), file=sys.stderr)
sys.excepthook = diagnose
role, config, linux, worker, start, compose, script_dir, pc_token, gitea_token, host_image_token, codex_auth, claude_auth, flavor = sys.argv[1:]
config = pathlib.Path(config).resolve()

def load(path):
    value = json.loads(path.read_text())
    if set(value) != {'serverUrl', 'serverTokenFile', 'workerId'}:
        raise SystemExit('K2は既存の3項目だけを指定してください')
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", value["workerId"]):
        raise SystemExit("workerIdの形式が不正です")
    return value
apple = load(config)
if not compose:
    compose = str(pathlib.Path(script_dir).parent.parent / 'compose.yaml')
if start == 'true' and role == 'implementation' and not all([pc_token, gitea_token, host_image_token, codex_auth, claude_auth]):
    raise SystemExit('implementation初期化には--pc-token-file --gitea-token-file --host-image-token-file --codex-auth-file --claude-auth-fileが必要です（本人が発行・ログイン済みの秘密ファイルのパス）')
if role == 'implementation':
    other = load(pathlib.Path(linux).resolve())
    if other['serverUrl'] != apple['serverUrl']:
        raise SystemExit('二つのK2は同じ受付を指定してください')
    if other['workerId'] == apple['workerId']:
        raise SystemExit('Linux実装WorkerとApple runnerは別workerIdで登録してください')
# Artifacts contain paths and host IDs, never token contents.
root = pathlib.Path.home() / 'Library' / 'Application Support' / 'issue-desk' / 'apple-prepared' / apple['workerId']
root.mkdir(parents=True, exist_ok=True)
label = 'com.issue-desk.apple.' + apple['workerId']
plist = root / (label + '.plist')
if plist.exists():
    raise SystemExit('既存の準備済みWorkerを上書きしません: ' + str(plist))
if subprocess.run(['launchctl', 'print', 'gui/'+str(os.getuid())+'/'+label], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
    raise SystemExit('既存Workerが登録されています。停止・実行と送信の完了を確認してください')
logs = root / 'logs'
logs.mkdir(exist_ok=True)
if start == 'true':
    subprocess.run([worker, 'apple', 'check', '--config', str(config)], check=True)
if role == 'implementation':
    if (root / 'compose.apple.yaml').exists():
        raise SystemExit('既存のcompose overlayを上書きしません')
    temporary = tempfile.NamedTemporaryFile(mode='w', prefix='compose-', suffix='.yaml', dir=root, delete=False)
    overlay = pathlib.Path(temporary.name)
    atexit.register(lambda: overlay.unlink(missing_ok=True))
    with temporary:
        temporary.write('services:\n  issue-desk-client:\n    platform: linux/amd64\n')

if start == 'true':
    if role == 'implementation':
        subprocess.run(['python3', str(pathlib.Path(script_dir) / 'bootstrap.py'),
                        '--config', str(pathlib.Path(linux).resolve()), '--compose', str(pathlib.Path(compose).resolve()),
                        '--root', str(root), '--overlay', str(overlay), '--flavor='+flavor,
                        '--pc-token-file', pc_token, '--gitea-token-file', gitea_token,
                        '--host-image-token-file', host_image_token, '--codex-auth-file', codex_auth,
                        '--claude-auth-file', claude_auth], check=True)
    if role == 'implementation':
        with (root / 'compose.apple.yaml').open('x') as f:
            f.write(overlay.read_text())
    with plist.open('xb') as f:
        plistlib.dump({'Label': label, 'ProgramArguments': [worker, 'apple', 'run', '--config', str(config)],
                      'RunAtLoad': True, 'KeepAlive': {'SuccessfulExit': False}, 'ThrottleInterval': 30,
                      'EnvironmentVariables': {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin'},
                      'StandardOutPath': str(logs / 'stdout.log'), 'StandardErrorPath': str(logs / 'stderr.log')}, f)
    subprocess.run(['launchctl', 'bootstrap', 'gui/'+str(os.getuid()), str(plist)], check=True)
    subprocess.run(['launchctl', 'kickstart', 'gui/'+str(os.getuid())+'/'+label], check=True)
if start != 'true':
    if role == 'implementation':
        with (root / 'compose.apple.yaml').open('x') as f:
            f.write(overlay.read_text())
    with plist.open('xb') as f:
        plistlib.dump({'Label': label, 'ProgramArguments': [worker, 'apple', 'run', '--config', str(config)],
                      'RunAtLoad': True, 'KeepAlive': {'SuccessfulExit': False}, 'ThrottleInterval': 30,
                      'EnvironmentVariables': {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin'},
                      'StandardOutPath': str(logs / 'stdout.log'), 'StandardErrorPath': str(logs / 'stderr.log')}, f)
print('準備先:', root)
if start != 'true':
    print('まず実行:', shlex.join([worker, 'apple', 'check', '--config', str(config)]))
    print('check成功後の起動: launchctl bootstrap gui/$(id -u)', shlex.quote(str(plist)))
print('停止: launchctl bootout gui/$(id -u)/' + label)
if role == 'implementation' and start != 'true':
    print('Linux初期化と起動には、別の未準備IDで--startと本人の秘密ファイル引数を指定してください。')
PY
