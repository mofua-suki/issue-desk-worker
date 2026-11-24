#!/usr/bin/env bash
# Manual blue-only update. Never remove volumes or store registry authentication on disk.
set +x
set -euo pipefail
umask 077
command -v python3 >/dev/null || { echo '更新にはPython 3が必要です' >&2; exit 1; }
unset registry_secret
operation='prepare'
auth_dir=''
cleanup() { unset registry_secret; if [[ -n $auth_dir ]]; then rm -rf -- "$auth_dir"; fi; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
fail() { printf 'Linux client update failed (%s); use --restore-previous if a previous environment exists.\n' "$operation" >&2; exit 1; }
trap fail ERR
# Keep the end of docker's own message so a failed pull or start says why.
show_docker_error() {
  local text=$1
  if (( ${#text} > 2000 )); then text=${text: -2000}; fi
  if [[ -n $text ]]; then printf 'docker: %s\n' "$text" >&2; fi
}
[[ $# -eq 2 || ( $# -eq 3 && $3 == --pull-only && $2 != --restore-previous && $2 != --mark-green-stopped ) ]] || fail
pull_only=${3:-}
client_dir=$(cd -- "$1" && pwd)
choice=$2
[[ $choice == --restore-previous || $choice == --mark-green-stopped || $choice =~ ^[0-9a-f]{12}(-client-python)?$ ]] || fail
# Explicit .env selection prevents caller-provided image overrides changing the target.
unset ISSUE_DESK_CLIENT_TAG ISSUE_DESK_CLIENT_BLUE_TAG ISSUE_DESK_CLIENT_GREEN_TAG REGISTRY FLAVOR COMPOSE_FILE COMPOSE_PROFILES CODEX_AUTH_DIR WEBHOOK_BIND WEBHOOK_PORT CLAUDE_API_KEY_FILE
compose=(docker compose --project-directory "$client_dir" --env-file "$client_dir/.env" -f "$client_dir/compose.yaml")
exec 9>"$client_dir/.linux-update.lock"
flock -n 9 || fail
previous="$client_dir/.env.linux.previous"
pending="$client_dir/.linux-update.pending"
# Remove this compatibility action only after every PC has upgraded hostd.
if [[ $choice == --mark-green-stopped ]]; then
  printf '%s\n' 'Compatibility request completed; environment and containers unchanged.'
  exit 0
fi
raw=$("${compose[@]}" config --format json 2>/dev/null)
image=$(printf '%s' "$raw" | python3 -c 'import json,sys;sys.stdout.write(json.load(sys.stdin)["services"]["issue-desk-client"]["image"])' 2>/dev/null)
# The old entry may still own the host port even though it is no longer in compose.
stop_old_entry() {
  local ids id
  ids=$(docker ps -q --filter label=com.docker.compose.project=issue-desk-client --filter label=com.docker.compose.service=issue-desk-webhook-entry) || return 1
  for id in $ids; do docker stop "$id" >/dev/null || return 1; done
}
restore() {
  [[ -f $previous ]] || return 1
  cp -- "$previous" "$client_dir/.env.linux.next" || return 1
  mv -- "$client_dir/.env.linux.next" "$client_dir/.env" || return 1
  stop_old_entry || return 1
  "${compose[@]}" up -d --no-deps --pull never issue-desk-client >/dev/null 2>&1 || return 1
  rm -f -- "$pending"
}
if [[ $choice == --restore-previous ]]; then
  operation='rollback'
  restore
  printf '%s\n' 'Previous environment restored; check logs, self-check and受付 ready before resuming.'
  exit 0
fi
[[ ! -e $pending ]] || fail
registry=${image%%/*}
[[ $image == "$registry/issue-desk-client:"* || $image == "$registry/issue-desk-client-python:"* ]] || fail
repository=${image%:*}
if [[ $repository == *-python ]]; then [[ $choice == *-client-python ]] || fail; else [[ $choice != *-client-python ]] || fail; fi
operation='registry authentication and pull'
# /dev/shm is tmpfs on Ubuntu. Do not fall back to a persistent temp directory.
[[ $(stat -f -c %T /dev/shm) == tmpfs ]] || fail
auth_dir=$(mktemp -d /dev/shm/issue-desk-docker.XXXXXXXX)
export DOCKER_CONFIG="$auth_dir"
if [[ -t 0 ]]; then
  IFS= read -r -s -p 'Read-only registry password (hidden): ' registry_secret
  printf '\n' >&2
else
  IFS= read -r registry_secret || [[ -n ${registry_secret:-} ]]
fi
[[ -n $registry_secret ]] || fail
printf '%s' "$registry_secret" | python3 -c '
import base64,json,os,sys
file=os.path.join(os.environ["DOCKER_CONFIG"],"config.json")
auth=base64.b64encode(("issue-desk-reader:"+sys.stdin.read()).encode()).decode()
fd=os.open(file,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,"w") as f:json.dump({"auths":{sys.argv[1]:{"auth":auth}}},f)
' "$registry" >/dev/null 2>&1
unset registry_secret
if ! docker_error=$(docker pull "$repository:$choice" 2>&1 >/dev/null); then
  show_docker_error "$docker_error"
  fail
fi
rm -rf -- "$auth_dir"
auth_dir=''
unset DOCKER_CONFIG
if [[ -n $pull_only ]]; then
  printf '%s\n' 'Published image pulled; environment and containers unchanged.'
  exit 0
fi
operation='save environment'
# A pending marker prevents retry from overwriting the original recovery environment.
cp -- "$client_dir/.env" "$client_dir/.env.linux.next"
mv -- "$client_dir/.env.linux.next" "$previous"
: > "$pending"
python3 - "$client_dir/.env" "$choice" <<'PYENV' >/dev/null 2>&1
import os,re,sys
file,tag=sys.argv[1:]
with open(file) as f:old=f.read()
lines=[line for line in old.split('\n') if not re.match(r'^\s*(?:export\s+)?ISSUE_DESK_CLIENT_(?:BLUE_)?TAG\s*=',line)]
next='\n'.join(lines)+'\nISSUE_DESK_CLIENT_TAG='+tag+'\nISSUE_DESK_CLIENT_BLUE_TAG='+tag+'\n'
fd=os.open(file+'.linux.next',os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
with os.fdopen(fd,'w') as f:f.write(next)
os.replace(file+'.linux.next',file)
PYENV
operation='start'
docker_error=''
if ! stop_old_entry || ! docker_error=$("${compose[@]}" up -d --no-deps --pull never issue-desk-client 2>&1 >/dev/null); then
  show_docker_error "$docker_error"
  operation='start failed; restoring previous environment'
  restore || fail
  fail
fi
rm -f -- "$pending"
printf '%s\n' 'Client started; check logs, self-check and受付 ready. Use --restore-previous if checks fail.'
