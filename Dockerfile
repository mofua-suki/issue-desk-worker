ARG TOOLS_IMAGE
FROM ${TOOLS_IMAGE} AS tools
FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
ARG ISSUE_DESK_REVISION
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-X main.revision=${ISSUE_DESK_REVISION}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -o /out/desk ./cmd/desk \
 && CGO_ENABLED=0 go build -trimpath -o /out/hostd ./cmd/hostd

FROM node:26.13.1-bookworm-slim AS worker-base
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl xz-utils chromium-headless-shell \
 && git config --system --add safe.directory /opt/flutter \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /app /state /data/codex /data/claude /data/agy/gemini /data/agy/config /opt/agent-tools \
 && ln -s /data/agy/gemini /home/node/.gemini \
 && ln -s /data/agy/config /home/node/.config \
 && chown -R node:node /app /state /data /opt/agent-tools
COPY --from=tools /usr/local/bin/node /usr/local/bin/node
COPY --from=tools /opt/agent-tools /opt/agent-tools
COPY --from=tools /opt/jdk /opt/jdk
COPY --from=tools /opt/gradle /opt/gradle
COPY --from=tools --chown=node:node /opt/flutter /opt/flutter
# flutter_tools の package_config が参照する公開パッケージも同じ位置に置く。
COPY --from=tools /opt/flutter-pub-cache /opt/flutter-pub-cache
COPY --from=build /usr/local/go /usr/local/go
COPY --from=build /out/worker /out/desk /out/hostd /usr/local/bin/
COPY deploy/entrypoint.sh /usr/local/bin/worker-entrypoint
COPY deploy /app/deploy
ENV CODEX_HOME=/data/codex CLAUDE_CONFIG_DIR=/data/claude DISABLE_AUTOUPDATER=1 ISSUE_DESK_AGY_DIR=/data/agy \
 JAVA_HOME=/opt/jdk FLUTTER_SUPPRESS_ANALYTICS=true \
 PATH=/opt/agent-tools/bin:/opt/jdk/bin:/opt/gradle/bin:/opt/flutter/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
ARG ISSUE_DESK_REVISION
ENV ISSUE_DESK_REVISION=${ISSUE_DESK_REVISION}
USER node
WORKDIR /app
RUN node /app/deploy/check-image-tools.mts --chromium
ENTRYPOINT ["sh","/usr/local/bin/worker-entrypoint"]

FROM worker-base AS flutter-check
USER root
# The writable cache parent cannot prevent replacing a snapshot, as in the runtime policy.
RUN mkdir -p /home/node/.pub-cache && chown root:root /home/node/.pub-cache && chmod 700 /home/node/.pub-cache \
 && find /opt/flutter/bin/cache -mindepth 1 -type d -exec chmod go-w {} + \
 && chgrp 1001 /opt/flutter/bin/cache && chmod g+rwx /opt/flutter/bin/cache \
 && find /opt/flutter/bin/cache -maxdepth 1 -type f \( -name '*.stamp' -o -name 'engine.realm' -o -name '*lock*' \) -exec chgrp 1001 {} + -exec chmod g+rw {} +
USER 1001:1001
RUN test ! -r /home/node/.pub-cache && test ! -w /opt/flutter-pub-cache && node /app/deploy/check-image-tools.mts --flutter-readonly && touch /tmp/flutter-check-ok

FROM worker-base AS worker
COPY --from=flutter-check /tmp/flutter-check-ok /tmp/flutter-check-ok

FROM python:3.12.14-slim-bookworm AS python-runtime
FROM worker AS client-python
USER root
COPY --from=python-runtime /usr/local/bin/python3.12 /usr/local/bin/python3.12
COPY --from=python-runtime /usr/local/lib/python3.12 /usr/local/lib/python3.12
COPY --from=python-runtime /usr/local/lib/libpython3.12.so.1.0 /usr/local/lib/libpython3.12.so.1.0
COPY --from=python-runtime /usr/local/include/python3.12 /usr/local/include/python3.12
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-0 libffi8 libbz2-1.0 liblzma5 libssl3 zlib1g libncursesw6 libreadline8 libuuid1 libgdbm6 libexpat1 \
 && rm -rf /var/lib/apt/lists/* && /sbin/ldconfig \
 && ln -sf /usr/local/bin/python3.12 /usr/local/bin/python3 \
 && /usr/local/bin/python3.12 -m venv /opt/pyfmt \
 && /opt/pyfmt/bin/pip install --no-cache-dir black==26.5.1
USER node
FROM worker
