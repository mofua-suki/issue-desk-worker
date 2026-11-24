#!/bin/bash
set -euo pipefail
script=$(cd "$(dirname "$0")" && pwd)/prepare.sh
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mkdir -p "$root/bin" "$root/home"
export HOME="$root/home" PREPARE_TEST_ROOT="$root"
export PATH="$root/bin:$PATH"
cat > "$root/bin/uname" <<'EOF'
#!/bin/sh
echo Darwin
EOF
cat > "$root/bin/launchctl" <<'EOF'
#!/bin/sh
case "$1" in
 print) exit 1;;
 bootstrap)
   label="com.issue-desk.apple.$EXPECTED_ID"
   test "$#" -eq 3 && test "$2" = "gui/$(id -u)" &&
     test "$3" = "$HOME/Library/Application Support/issue-desk/apple-prepared/$EXPECTED_ID/$label.plist" &&
     test -f "$3" && test ! -f "$PREPARE_TEST_ROOT/registered-$EXPECTED_ID" || exit 1
   touch "$PREPARE_TEST_ROOT/registered-$EXPECTED_ID";;
 kickstart)
   test "$#" -eq 2 && test "$2" = "gui/$(id -u)/com.issue-desk.apple.$EXPECTED_ID" &&
     test -f "$PREPARE_TEST_ROOT/registered-$EXPECTED_ID" || exit 1
   touch "$PREPARE_TEST_ROOT/started-$EXPECTED_ID";;
 *) exit 7;;
esac
EOF
cat > "$root/bin/worker" <<'EOF'
#!/bin/sh
if test -f "$PREPARE_TEST_ROOT/fail-check"; then exit 4; fi
touch "$PREPARE_TEST_ROOT/checked"
EOF
cat > "$root/bin/docker" <<'EOF'
#!/bin/sh
touch "$PREPARE_TEST_ROOT/docker-used"
exit 6
EOF
chmod +x "$root/bin/"*
export EXPECTED_ID=apple
printf '{"serverUrl":"https://intake.invalid","serverTokenFile":"/private/token","workerId":"apple"}\n' > "$root/k2.json"
bash "$script" --start --role test-only --config "$root/k2.json" --worker "$root/bin/worker"
test -f "$root/checked"
test -f "$root/started-apple"
test ! -f "$root/docker-used"
# A second setup must not replace the prepared or running Worker.
if bash "$script" --start --role test-only --config "$root/k2.json" --worker "$root/bin/worker"; then exit 8; fi
export EXPECTED_ID=apple2
printf '{"serverUrl":"https://intake.invalid","serverTokenFile":"/private/token2","workerId":"apple2"}\n' > "$root/k2.json"
touch "$root/fail-check"
if bash "$script" --start --role test-only --config "$root/k2.json" --worker "$root/bin/worker"; then exit 9; fi
test ! -f "$root/registered-apple2"
test ! -f "$root/started-apple2"
if bash "$script" --role; then exit 10; fi
rm "$root/fail-check"
export EXPECTED_ID=apple3
printf '{"serverUrl":"https://intake.invalid","serverTokenFile":"/private/apple3","workerId":"apple3"}\n' > "$root/k2.json"
printf '{"serverUrl":"https://intake.invalid","serverTokenFile":"/state/private/pc-token","workerId":"linux3"}\n' > "$root/linux.json"
touch "$root/compose.yaml"
if bash "$script" --start --role implementation --config "$root/k2.json" --worker "$root/bin/worker" --implementation-config "$root/linux.json" --compose "$root/compose.yaml"; then exit 13; fi
test ! -f "$root/registered-apple3"
test ! -f "$root/started-apple3"
python3 "$(dirname "$script")/bootstrap.test.py"
real_python=$(command -v python3)
export PREPARE_REAL_PYTHON="$real_python"
cat > "$root/bin/python3" <<'EOF'
#!/bin/sh
case "$1" in
 */bootstrap.py) shift; exec "$PREPARE_REAL_PYTHON" "$PREPARE_TEST_ROOT/bootstrap-stub.py" "$@";;
 *) exec "$PREPARE_REAL_PYTHON" "$@";;
esac
EOF
chmod +x "$root/bin/python3"
cat > "$root/bootstrap-stub.py" <<'PY'
import json, os, pathlib, sys
root=pathlib.Path(os.environ['PREPARE_TEST_ROOT'])
# --flavor is a single --flavor=value argument.
args={}
it=iter(sys.argv[1:])
for arg in it:
    if arg.startswith('--flavor='):
        if arg != '--flavor=-python':
            raise SystemExit(24)
        continue
    args[arg]=next(it)
if pathlib.Path(args['--compose']) != root/'compose.yaml':
    raise SystemExit(21)
for key in ['--pc-token-file','--gitea-token-file','--host-image-token-file','--codex-auth-file','--claude-auth-file']:
    if not pathlib.Path(args[key]).is_file():
        raise SystemExit(22)
if (root/'bootstrap-failure').exists():
    raise SystemExit(23)
pathlib.Path(args['--overlay']).write_text(json.dumps({'services':{'issue-desk-client':{'platform':'linux/amd64'}}}))
(root/'linux-started').touch()
PY
for name in pc git host codex claude; do
  (umask 077; echo fixture > "$root/$name-secret")
done
touch "$root/bootstrap-failure"
setup_implementation() {
  bash "$script" --start --role implementation --config "$root/k2.json" --worker "$root/bin/worker" \
    --implementation-config "$root/linux.json" --compose "$root/compose.yaml" \
    --pc-token-file "$root/pc-secret" --gitea-token-file "$root/git-secret" \
    --host-image-token-file "$root/host-secret" --codex-auth-file "$root/codex-secret" \
    --claude-auth-file "$root/claude-secret"
}
if setup_implementation; then exit 14; fi
test ! -f "$root/registered-apple3"
test ! -f "$root/started-apple3"
rm "$root/bootstrap-failure"
setup_implementation
test -f "$root/linux-started"
test -f "$root/started-apple3"
