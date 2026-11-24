#!/bin/sh
set -eu
umask 077
worker antigravity-prepare --config "${ISSUE_DESK_CONFIG:-/state/config.local.json}"
status=0
/opt/agent-tools/bin/agy -p /usage || status=$?
worker antigravity-prepare --config "${ISSUE_DESK_CONFIG:-/state/config.local.json}"
exit "$status"
