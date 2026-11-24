#!/bin/sh
set -eu
unset GODEBUG
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GO=${GO:-go}
GOFMT=${GOFMT:-gofmt}
GOCACHE=$("$GO" env GOCACHE)
GOMODCACHE=$("$GO" env GOMODCACHE)
export GOCACHE GOMODCACHE
run_dir=$(mktemp -d "${TMPDIR:-/tmp}/verify.XXXXXXXXXX")
trap 'rm -rf -- "$run_dir"' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$run_dir/home" "$run_dir/tmp"
HOME=$run_dir/home
TMPDIR=$run_dir/tmp
TEMP=$TMPDIR
export HOME TMPDIR TEMP
cd "$ROOT"
unformatted=$("$GOFMT" -l .)
if [ -n "$unformatted" ]; then
  printf 'gofmtが必要です:\n%s\n' "$unformatted" >&2
  exit 1
fi
GO="$GO" sh scripts/check-go-quality.sh
"$GO" vet ./...
# 検証全体の上限15分を考慮し、各パッケージの既定のテスト期限を12分にする。
# 引数があるとき（テストの絞り込みなど）は、そのまま1回で流す。
if [ "$#" -gt 0 ]; then
  "$GO" test -timeout=12m "$@" ./...
  exit
fi
# internal/execution はテスト時間の大半を占め、1パッケージは1プロセスで直列に流れるため、
# テスト名で組に分けて別プロセスで同時に流す。ほかのパッケージも同時に流す。
# 出力は組ごとに貯め、終わってから順に出す（行が混ざると失敗の解析が崩れるため）。
shards=${VERIFY_EXECUTION_SHARDS:-4}
slow=./internal/execution
names=$("$GO" test -list '.*' "$slow" | grep -E '^(Test|Example|Fuzz)')
others=$("$GO" list ./... | grep -vx 'issue-desk-worker/internal/execution')
i=0
pids=
while [ "$i" -lt "$shards" ]; do
  pattern=$(printf '%s\n' "$names" | awk -v n="$shards" -v k="$i" 'NR % n == k' | paste -sd'|' -)
  if [ -n "$pattern" ]; then
    "$GO" test -timeout=12m -run "^($pattern)\$" "$slow" >"$run_dir/execution-$i.out" 2>&1 &
    pids="$pids $!"
  fi
  i=$((i + 1))
done
# shellcheck disable=SC2086
"$GO" test -timeout=12m $others >"$run_dir/others.out" 2>&1 &
pids="$pids $!"
status=0
for pid in $pids; do
  wait "$pid" || status=1
done
cat "$run_dir/others.out"
i=0
while [ "$i" -lt "$shards" ]; do
  [ -f "$run_dir/execution-$i.out" ] && cat "$run_dir/execution-$i.out"
  i=$((i + 1))
done
exit "$status"
