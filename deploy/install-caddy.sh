#!/bin/sh
set -eu
# image-versions.mts が検証して生成したファイルだけを読み込む。
. "$1"
out=${2:-/out}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cd "$work"
cat > main.go <<'EOF'
package main

import (
    caddycmd "github.com/caddyserver/caddy/v2/cmd"
    _ "github.com/caddyserver/caddy/v2/modules/standard"
)

func main() { caddycmd.Main() }
EOF
go mod init issue-desk-caddy-build
go get "github.com/caddyserver/caddy/v2/cmd@v${CADDY_VERSION}" "github.com/caddyserver/caddy/v2/modules/standard@v${CADDY_VERSION}"
GOEXPERIMENT=nojsonv2 go build -buildvcs=false -o "$out/caddy" .
