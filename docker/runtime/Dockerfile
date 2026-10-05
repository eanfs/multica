# syntax=docker/dockerfile:1
# Build context: repository root. Installation/build are separately opt-in.
# Native target-platform stages retain amd64/arm64 support without cross-ABI copies.
FROM golang:1.26.6-bookworm AS builder
WORKDIR /src
COPY server/ ./
RUN CGO_ENABLED=0 go build -o /out/multica ./cmd/multica && CGO_ENABLED=0 go build -o /out/fleet-node ./cmd/fleet-node

FROM node:22-bookworm-slim AS claude
COPY docker/runtime/claude-version.txt /tmp/claude-version.txt
# No build argument can substitute latest or override the checked-in registry pin.
RUN version="$(node -e 'const v=require("fs").readFileSync("/tmp/claude-version.txt","utf8").trim(); if(!/^[0-9]+\.[0-9]+\.[0-9]+$/.test(v)) process.exit(1); process.stdout.write(v)')" \
 && npm install --global --prefix /opt/claude --registry=https://registry.npmjs.org/ --no-audit --no-fund "@anthropic-ai/claude-code@$version"

# A neutral final base must NOT inherit the Node stage's NODE_VERSION/YARN_VERSION.
FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates libstdc++6 libgcc-s1 libatomic1 \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 runtime && useradd --uid 10001 --gid 10001 --create-home runtime
COPY --from=claude /usr/local/bin/node /usr/local/bin/node
COPY --from=claude /opt/claude/ /usr/local/
COPY --from=builder /out/ /usr/local/bin/
# Future image builds verify shared-library ABI and the package's actual entrypoint.
RUN ldd /usr/local/bin/node && /usr/local/bin/node --version && /usr/local/bin/claude --version
USER 10001:10001
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/data/home
ENTRYPOINT ["/usr/local/bin/fleet-node","run"]
HEALTHCHECK --interval=5s --timeout=5s CMD ["/usr/local/bin/fleet-node","health"]
