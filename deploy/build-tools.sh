#!/usr/bin/env bash
# 固定commitから独立tools imageを作る。旧アプリのcheckout・imageは不要。
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
revision=$(git -C "$root" rev-parse HEAD)
[[ $revision =~ ^[a-f0-9]{40}$ ]] || exit 2
[[ -z $(git -C "$root" status --porcelain) ]] || { echo 'toolsimageは変更をcommitしてから構築してください' >&2; exit 1; }
repository=${1:-issue-desk-tools}
[[ $repository =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]*$ ]] || exit 2
image=$repository:${revision:0:12}
docker build -f "$root/Dockerfile.tools" --build-arg "ISSUE_DESK_TOOLS_REVISION=$revision" -t "$image" "$root"
printf 'TOOLS_IMAGE=%s\n' "$image"
docker image inspect --format 'TOOLS_IMAGE_ID={{.Id}}' "$image"
