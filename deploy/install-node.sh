#!/bin/sh
# Docker の準備用 Node で検査して出した版だけを使う。実行時 Node は公式の配布物から入れる。
set -eu
. /tmp/image-versions.env
case "$(dpkg --print-architecture)" in
amd64) arch=x64 ;;
arm64) arch=arm64 ;;
*) echo 'unsupported Node architecture' >&2; exit 1 ;;
esac
archive=node-v${NODE_VERSION}-linux-${arch}.tar.xz
mkdir -p /tmp/node-runtime
cd /tmp/node-runtime
curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/${archive}"
curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt"
grep "  ${archive}$" SHASUMS256.txt | sha256sum -c -
rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack
tar -xJf "$archive" -C /usr/local --strip-components=1
cd /
rm -rf /tmp/node-runtime
node --version
