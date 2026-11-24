#!/bin/sh
# cleanEnv() を通る監視の子プロセスでも、固定版とファイル認証を使う。
set -eu
export AGY_CLI_DISABLE_AUTO_UPDATE=true
unset DBUS_SESSION_BUS_ADDRESS GEMINI_API_KEY GOOGLE_API_KEY AGY_ADC_AUTH
exec /opt/agent-tools/lib/antigravity/antigravity "$@"
