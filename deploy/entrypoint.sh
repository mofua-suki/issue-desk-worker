#!/bin/sh
set -eu
exec /usr/local/bin/worker run --config "${ISSUE_DESK_CONFIG:-/state/config.local.json}"
