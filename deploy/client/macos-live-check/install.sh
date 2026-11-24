#!/usr/bin/env bash
set +x
set -euo pipefail
umask 077
fail() { echo "$1" >&2; exit 1; }
[[ $# -ge 8 ]] || fail '使い方: install.sh hostd PC名 HTTPS-URL repo 絶対path timeoutSeconds checkout command [args...]'
[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || fail 'macOS Apple Siliconが必要です'
for tool in python3 git file launchctl; do command -v "$tool" >/dev/null || fail "$tool が必要です"; done
hostd=$1 host=$2 url=$3 repo=$4 path=$5 timeout=$6 checkout=$7
shift 7
[[ $hostd == /* && -f $hostd && -x $hostd ]] || fail 'hostdは実行可能な絶対パスを指定してください'
LC_ALL=C file -b "$hostd" | grep -Eq '^Mach-O 64-bit executable arm64([[:space:]]|$)' || fail 'hostdはMach-O arm64実行ファイルを指定してください'
[[ $path == /* && -d $path ]] || fail 'Git作業ツリーの絶対パスを指定してください'
[[ $(git -C "$path" rev-parse --is-inside-work-tree 2>/dev/null) == true ]] || fail 'Git作業ツリーが不正です'
git -C "$path" remote get-url origin >/dev/null 2>&1 || fail 'Gitのoriginがありません'
[[ -n $repo && $timeout =~ ^[0-9]+$ && $timeout != 0 && ( $checkout == true || $checkout == false ) && -n $1 ]] || fail 'live check設定が不正です'
python3 - "$url" "$host" <<'PY' || exit 1
import re,sys,urllib.parse
try:
 u=urllib.parse.urlsplit(sys.argv[1]); port=u.port
 assert u.scheme=='https' and u.hostname and not u.username and not u.password and not u.query and '?' not in sys.argv[1] and '#' not in sys.argv[1] and not u.fragment and u.path in ('','/')
 assert re.fullmatch(r'[a-z0-9][a-z0-9._-]{0,63}',sys.argv[2])
except (ValueError,AssertionError):
 print('接続先またはPC名が不正です',file=sys.stderr); sys.exit(1)
PY
IFS= read -r token || fail 'トークンを1行入力してください'
[[ $token =~ ^idh_[A-Za-z0-9_-]{43}$ ]] || fail 'トークンが不正です'
base="$HOME/Library/Application Support/issue-desk/live-check"
agent="$HOME/Library/LaunchAgents/com.issue-desk.live-check.plist"
mkdir -p "$base" "$(dirname "$agent")"
[[ ! -L $base && -O $base ]] || fail '状態ディレクトリの所有者またはリンクが不正です'
chmod 700 "$base"
stage=$(mktemp -d "$base/.install.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
printf '%s\n' "$token" > "$stage/token"
unset token
script_dir=$(cd "$(dirname "$0")" && pwd -P)
python3 - "$stage" "$base" "$script_dir/com.issue-desk.live-check.plist" "$hostd" "$host" "$url" "$repo" "$path" "$timeout" "$checkout" "$@" <<'PY'
import json,os,plistlib,sys
stage,base,template,exe,host,url,repo,path,timeout,checkout,*command=sys.argv[1:]
config=dict(url=url,host=host,tokenFile=base+'/token',stateFile=base+'/state.json',liveChecks=[dict(repo=repo,path=path,command=command,timeoutSeconds=int(timeout),checkout=checkout=='true')])
with open(stage+'/config.json','w') as f: json.dump(config,f,ensure_ascii=False)
with open(template,'rb') as f: plist=plistlib.load(f)
plist['ProgramArguments']=[exe,'live-check','--config',base+'/config.json']
with open(stage+'/agent.plist','wb') as f: plistlib.dump(plist,f)
PY
uid=$(id -u)
# bootout reports an absent service as "Could not find service" or "No such process".
if LC_ALL=C launchctl bootout "gui/$uid/com.issue-desk.live-check" >"$stage/bootout.log" 2>&1; then :
elif grep -Eq 'Could not find (specified )?service|No such process' "$stage/bootout.log"; then :
else fail '既存LaunchAgentの解除に失敗しました'; fi
mv -f "$stage/token" "$base/token"
mv -f "$stage/config.json" "$base/config.json"
mv -f "$stage/agent.plist" "$agent"
launchctl bootstrap "gui/$uid" "$agent" >/dev/null 2>&1 || fail 'LaunchAgentの登録に失敗しました'
echo 'LaunchAgentを登録しました'
