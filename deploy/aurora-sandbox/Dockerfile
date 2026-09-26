# Managed Aurora sandbox image.
#
# Final image contract (docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md):
#   - runs as UID/GID 10001:10001, read-only root, no volume, no public port;
#   - holds no provider credential, enrollment token, daemon token, Git metadata,
#     package-manager cache, or build secret;
#   - contains no skills installer, npm/pnpm/npx/corepack, Git, curl, wget, or SSH.
# The four provider credentials are host files the fleet mounts read-only at
# /run/secrets/*; nothing here bakes a value in.
#
# Build inputs arrive through Bake named contexts: `server` (Go module),
# `reporoot` (root pnpm workspace), and `scripts` (lock verifiers). The
# image-local context is deploy/aurora-sandbox, whose .dockerignore is deny-all.

# ---------------------------------------------------------------------------
# Stage 1: Go builder. Compiles the static daemon and runs focused Go tests.
# ---------------------------------------------------------------------------
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS gobuilder
ARG TARGETARCH
ARG SOURCE_DATE_EPOCH=0
WORKDIR /src/server
COPY --from=server . /src/server/
# VCS metadata is disabled so the binary is reproducible from the source tree.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH \
      go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid=" \
      -o /out/multica ./cmd/multica
# Focused Go tests for the managed secret boundary that this image enforces.
RUN go test ./internal/aurorafleet ./internal/daemon \
      -run 'TestManagedSecret|TestProviderSecretMount' -count=1

# ---------------------------------------------------------------------------
# Stage 2: Node runtime dependencies. Fetches only the broker runtime package
# graph from the frozen lockfile, then installs it offline without lifecycle
# scripts (only esbuild/electron are allow-listed in the root manifest, and
# neither belongs to the broker graph).
# ---------------------------------------------------------------------------
FROM node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS nodedeps
WORKDIR /src
COPY --from=reporoot package.json pnpm-workspace.yaml pnpm-lock.yaml .npmrc /src/
COPY runtime/package.json runtime/src /src/deploy/aurora-sandbox/runtime/
# pnpm purges a partially-populated node_modules after an interrupted fetch;
# without a TTY it aborts unless CI=true (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY).
ENV CI=true
RUN corepack enable \
 && corepack prepare pnpm@10.28.2 --activate \
 && pnpm fetch --frozen-lockfile --filter @multica/aurora-runtime \
 && pnpm install --offline --prod --frozen-lockfile --filter @multica/aurora-runtime --ignore-scripts

# ---------------------------------------------------------------------------
# Stage 3: Vendor verification and hardening. Re-runs the offline vendor
# security tests (which apply the audited patch series to a throwaway tree and
# exercise the hardened adapters) and writes the patched-tree hash.
#
# The full scripts/verify-aurora-volc-skills.mjs is deliberately not invoked
# here: at this revision it hard-fails on the Seedance license gate, which is a
# separate, still-open distribution blocker recorded in vendor-lock.json. Only
# the vendored Seedream tree is shipped.
# ---------------------------------------------------------------------------
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS vendor
WORKDIR /src
COPY --from=nodedeps /usr/local/bin/node /usr/local/bin/node
COPY --from=nodedeps /src /src
COPY vendor /src/deploy/aurora-sandbox/vendor
COPY runtime/test /src/deploy/aurora-sandbox/runtime/test
RUN node --test /src/deploy/aurora-sandbox/runtime/test/volc-vendor-security.test.mjs \
 && rm -rf /tmp/patched && mkdir -p /tmp/patched \
 && cp -a /src/deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedream-skill /tmp/patched/ \
 && cd /tmp/patched && git init -q \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0002-seedream-broker-adapter.patch \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0003-seedream-fail-closed-cli.patch \
 && rm -rf /tmp/patched/.git \
 && mkdir -p /opt/aurora/vendor/volcengine \
 && cp -a /tmp/patched/byted-ark-seedream-skill /opt/aurora/vendor/volcengine/ \
 && rm -rf /tmp/patched \
 && find /opt/aurora/vendor -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}' > /opt/aurora/vendor-tree.sha256

# ---------------------------------------------------------------------------
# Stage 4: Final runtime. Digest-pinned Debian snapshot apt set, root-owned
# non-writable runtime files, non-root user, fixed entrypoint and health check.
# ---------------------------------------------------------------------------
FROM node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS sandbox
ARG SOURCE_DATE_EPOCH=0
# Exact versions from deploy/aurora-sandbox/apt-packages.lock (20260925T000000Z).
# The node base ships no CA bundle, so the bootstrap apt transport disables TLS
# peer/host verification and relies on apt GPG verification plus the SHA-256 in
# the authenticated Packages index, exactly as scripts/update-aurora-sandbox-apt-lock.sh
# does; the override is removed again below.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    printf '%s\n' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260925T000000Z bookworm main' \
      'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20260925T000000Z bookworm-security main' \
      > /etc/apt/sources.list; \
    rm -f /etc/apt/sources.list.d/*; \
    printf '%s\n' \
      'Acquire::https::Verify-Peer "false";' \
      'Acquire::https::Verify-Host "false";' \
      > /etc/apt/apt.conf.d/99snapshot-tls; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates=20250419~deb12u1 \
      chromium=153.0.8010.52-1~deb12u1 \
      ffmpeg=7:5.1.9-0+deb12u1 \
      fonts-noto-cjk=1:20220127+repack1-1 \
      fonts-noto-color-emoji=2.042-0+deb12u1 \
      imagemagick=8:6.9.11.60+dfsg-1.6+deb12u13 \
      libnss3=2:3.87.1-1+deb12u4 \
      poppler-utils=22.12.0-2+deb12u3 \
      tini=0.19.0-1+b3 \
      unzip=6.0-28+deb12u1 && \
    rm -f /etc/apt/apt.conf.d/99snapshot-tls && \
    rm -rf /var/lib/apt/lists/* /var/cache/apt /var/cache/debconf /var/log/apt /tmp/*

# Runtime payload, all root-owned and non-writable by the sandbox user.
COPY --from=gobuilder --chown=0:0 /out/multica /usr/local/bin/multica
# The pnpm workspace layout is preserved so the broker node_modules symlinks
# keep resolving; the runtime tree lives under /opt/aurora/runtime/deploy/.
COPY --from=nodedeps --chown=0:0 /src /opt/aurora/runtime
COPY --from=vendor --chown=0:0 /opt/aurora/vendor /opt/aurora/vendor
COPY --from=vendor --chown=0:0 /opt/aurora/vendor-tree.sha256 /opt/aurora/vendor-tree.sha256

RUN set -eux; \
    groupadd --gid 10001 aurora; \
    useradd --uid 10001 --gid 10001 --home-dir /workspace --shell /usr/sbin/nologin aurora; \
    rm -f /opt/aurora/runtime/package.json /opt/aurora/runtime/pnpm-lock.yaml \
          /opt/aurora/runtime/pnpm-workspace.yaml /opt/aurora/runtime/.npmrc; \
    rm -rf \
      /usr/local/lib/node_modules/npm \
      /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /usr/local/bin/pnpm /usr/local/bin/pnpx \
      /root/.cache /root/.npm /root/.local/share/pnpm /root/.cache/node/corepack; \
    chown -R 0:0 /opt/aurora /usr/local/bin/multica; \
    chmod -R go-w /opt/aurora /usr/local/bin/multica; \
    find /opt/aurora -type d -exec chmod 0555 {} +; \
    find /opt/aurora -type f -exec chmod 0444 {} +

USER 10001:10001
WORKDIR /workspace
# Fixed managed entrypoint. The fleet cannot override the command.
ENTRYPOINT ["/usr/local/bin/multica", "daemon", "start", "--managed", "--foreground", "--managed-enrollment-token-file=/run/secrets/aurora-enrollment"]
CMD []
# Non-shell health check: the same binary probes its own managed health endpoint.
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/multica", "daemon", "managed-healthcheck", "--url=http://127.0.0.1:19514/health", "--max-age=90s"]
