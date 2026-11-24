#!/bin/sh
set -eu
unset GODEBUG
GO=${GO:-go}
# deadcode reports findings on stdout even when it exits successfully.
report=$("$GO" run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./...) || exit $?
if [ -n "$report" ]; then
  printf '%s\n' "$report" >&2
  exit 1
fi
"$GO" run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
