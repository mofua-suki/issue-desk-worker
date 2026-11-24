#!/usr/bin/env bash
# Local installation only. Intake supplies existing credentials on stdin; never issue or revoke them here.
set +x
set -euo pipefail
umask 077
[[ $# -ge 4 && $# -le 5 ]] || { echo '使い方: install.sh <client-dir> <host> <https-url> <worker-image> [hostd-dir]' >&2; exit 2; }
for tool in docker python3 crontab flock; do command -v "$tool" >/dev/null || { printf '%s がありません\n' "$tool" >&2; exit 1; }; done
client_dir=$(cd -- "$1" && pwd)
hostd_dir=${5:-$client_dir/hostd}
[[ $hostd_dir == /* && -f $client_dir/compose.yaml && -f $client_dir/.env && -f $client_dir/update-linux.sh ]] || { echo '配置先とclient資産を確認してください' >&2; exit 1; }
exec 9>"$client_dir/.linux-hostd.lock"
flock -n 9 || { echo 'hostd周期が実行中です。更新しません' >&2; exit 1; }
mkdir -p -- "$hostd_dir"
[[ ! -L $hostd_dir && -O $hostd_dir ]] || { echo 'hostd配置先の所有者・リンクを確認してください' >&2; exit 1; }
chmod 700 "$hostd_dir"
work=$(mktemp -d "$hostd_dir/.install.XXXXXXXX")
container=''
cleanup() { unset hostd_token registry_password; if [[ -n $container ]]; then docker rm "$container" >/dev/null 2>&1 || true; fi; rm -rf -- "$work"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if LC_ALL=C crontab -l >"$work/cron" 2>"$work/error"; then :; elif [[ $(cat "$work/error") == 'no crontab for '* ]]; then : >"$work/cron"; else echo 'crontabを読めません' >&2; exit 1; fi
# Fetch public executable before receiving credentials. No container is started.
container=$(docker create --pull never --entrypoint /usr/local/bin/hostd "$4")
docker cp "$container:/usr/local/bin/hostd" "$work/hostd"
docker rm "$container" >/dev/null
container=''
chmod 700 "$work/hostd"
python3 - "$hostd_dir" "$client_dir" "$2" "$3" "$work" <<'PY'
import json,os,re,sys,urllib.parse
home,client,host,url,stage=sys.argv[1:]
u=urllib.parse.urlsplit(url)
if not re.fullmatch(r'[a-z0-9][a-z0-9._-]{0,63}',host) or u.scheme!='https' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('','/'):
 raise SystemExit('hostdの接続先・PC名が不正です')
old=os.path.join(home,'config.json');cfg={}
if os.path.isfile(old):
 with open(old) as f:cfg=json.load(f)
cfg.update(host=host,url=url.rstrip('/'),clientDir=client)
with open(os.path.join(stage,'config.json'),'w') as f:json.dump(cfg,f)
quote=lambda s:"'"+s.replace("'","'\\''")+"'"
with open(os.path.join(stage,'cron')) as f:oldcron=f.read()
marker='# issue-desk-linux-hostd'
cron=''.join(line for line in oldcron.splitlines(True) if not line.rstrip().endswith(marker))
command='PATH=/usr/local/bin:/usr/bin:/bin '+quote(os.path.join(home,'hostd'))+' --config '+quote(os.path.join(home,'config.json'))+' >/dev/null 2>&1'
with open(os.path.join(stage,'cron.next'),'w') as f:f.write(cron+('' if not cron or cron.endswith('\n') else '\n')+'*/5 * * * * '+command.replace('%','\\%')+' '+marker+'\n')
PY
IFS= read -r hostd_token
IFS= read -r registry_password
[[ $hostd_token =~ ^idh_[A-Za-z0-9_-]{43}$ && -n $registry_password ]] || { echo 'hostdの資格情報入力が不正です' >&2; exit 1; }
printf '%s\n' "$hostd_token" >"$work/token"
printf '%s\n' "$registry_password" >"$work/registry-password"
unset hostd_token registry_password
# Stage validation uses a private client directory, so the installation lock remains held.
mkdir "$work/client"
python3 - "$work" <<'PY'
import json,os,sys
p=sys.argv[1]
with open(p+'/config.json') as f:c=json.load(f)
c['clientDir']=p+'/client'
with open(p+'/check.json','w') as f:json.dump(c,f)
PY
"$work/hostd" --config "$work/check.json" --check
for file in hostd config.json token registry-password; do chmod 600 "$work/$file"; [[ $file != hostd ]] || chmod 700 "$work/$file"; mv -f "$work/$file" "$hostd_dir/$file"; done
crontab "$work/cron.next"
printf '%s\n' linux_hostd_installed
