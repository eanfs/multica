# Managed Aurora sandbox image.
#
# Final image contract (docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md):
#   - runs as UID/GID 10001:10001, read-only root, no volume, no public port;
#   - holds no provider credential, enrollment token, daemon token, Git metadata,
#     package-manager cache, or build secret;
#   - contains no skills installer, npm/pnpm/npx/corepack, Git, curl, wget, or SSH.
# One artifact satisfies two fixed contracts:
#   - the Aurora sandbox runtime under /opt/aurora;
#   - the Fleet node contract: /usr/local/bin/fleet-node plus /usr/local/bin/multica,
#     ENTRYPOINT fleet-node run, HEALTHCHECK fleet-node health, and the fixed
#     /data + /secrets layout owned by 10001:10001. The Fleet mounts a fresh data
#     volume at /data and its read-only secrets volume at /secrets, so nothing
#     here bakes an enrollment secret or provider credential in.
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
# The same image is a Fleet node: fleet-node owns the node contract (fixed
# entrypoint, layout and health) and execs the managed daemon above. Building it
# here keeps both contracts on one digest-pinned artifact.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH \
      go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid=" \
      -o /out/fleet-node ./cmd/fleet-node
# Focused Go tests for the managed secret boundary that this image enforces.
RUN go test ./internal/daemon \
      -run 'TestManagedSecret|TestProviderSecretMount' -count=1

# ---------------------------------------------------------------------------
# Stage 2: esbuild rebuild. hyperframes depends on esbuild, whose published
# linux binaries are Go executables built with esbuild's own release toolchain
# (0.25.12 ships go1.23.12). Trivy therefore flags the sandbox image with the
# stdlib CVEs fixed since, including Critical CVE-2025-68121. Rebuild the same
# locked esbuild release with this image's pinned Go toolchain so the shipped
# binary carries go1.26.6, while the JavaScript API hyperframes imports stays
# at the locked 0.25.12. scripts/verify-aurora-sandbox-locks.mjs pins this
# version to versions.json and the pnpm lockfile.
# ---------------------------------------------------------------------------
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS esbuildbuilder
ARG TARGETARCH
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/evanw/esbuild/cmd/esbuild@v0.25.12

# ---------------------------------------------------------------------------
# Stage 3: Node runtime dependencies. pnpm fetch ignores --filter and downloads
# every package in the workspace lockfile, so the filtered install is used to
# fetch and link only the broker runtime graph; a clean second install with
# --offline then proves the store is complete for the production tree.
# Lifecycle scripts stay disabled (only esbuild/electron are allow-listed in the
# root manifest, and neither belongs to the broker graph). The one exception is
# the Claude Code postinstall, run explicitly at the end: 2.1.282 ships a text
# stub at bin/claude.exe and only that local step (no network) hardlinks the
# platform-native binary over it.
# ---------------------------------------------------------------------------
FROM node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS nodedeps
WORKDIR /src
COPY --from=reporoot package.json pnpm-workspace.yaml pnpm-lock.yaml .npmrc /src/
# Keep the runtime package's src/ directory: its own tests and the smoke
# fixture import ../src/*.mjs, and Docker COPY flattens a directory source.
COPY runtime/package.json /src/deploy/aurora-sandbox/runtime/package.json
COPY runtime/src /src/deploy/aurora-sandbox/runtime/src/
# pnpm purges a partially-populated node_modules after an interrupted install;
# without a TTY it aborts unless CI=true (ERR_PNPM_ABORTED_REMOVE_MODULES_DIR_NO_TTY).
ENV CI=true
RUN corepack enable \
 && corepack prepare pnpm@10.28.2 --activate \
 && pnpm install --frozen-lockfile --prod --filter @multica/aurora-runtime --ignore-scripts \
 && rm -rf node_modules deploy/aurora-sandbox/runtime/node_modules \
 && pnpm install --offline --prod --frozen-lockfile --filter @multica/aurora-runtime --ignore-scripts \
 && node node_modules/@anthropic-ai/claude-code/install.cjs

# Replace the published esbuild platform binary with the toolchain-rebuilt one.
# esbuild resolves its executable from @esbuild/<platform>; exactly one must be
# present or the fix silently regresses.
COPY --from=esbuildbuilder /out/esbuild /out/esbuild
RUN set -eux; \
    replaced=0; \
    for binary in $(find /src/node_modules -path '*/@esbuild/*/bin/esbuild' -type f); do \
      cp /out/esbuild "$binary"; \
      replaced=$((replaced + 1)); \
    done; \
    test "$replaced" -ge 1; \
    rm -f /out/esbuild

# ---------------------------------------------------------------------------
# Stage 4: Vendor verification and hardening. Re-runs the full offline vendor
# gate (audited source digests, whole-tree hashes, and license provenance) and
# the patch security suite, then materializes the patched Seedance and Seedream
# trees and writes the patched-tree hash.
# ---------------------------------------------------------------------------
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS vendor
WORKDIR /src
COPY --from=nodedeps /usr/local/bin/node /usr/local/bin/node
COPY --from=nodedeps /src /src
COPY vendor /src/deploy/aurora-sandbox/vendor
COPY runtime/test /src/deploy/aurora-sandbox/runtime/test
COPY --from=scripts verify-aurora-volc-skills.mjs /src/scripts/verify-aurora-volc-skills.mjs
RUN node /src/scripts/verify-aurora-volc-skills.mjs \
 && node --test /src/deploy/aurora-sandbox/runtime/test/volc-vendor-security.test.mjs \
 && rm -rf /tmp/patched && mkdir -p /tmp/patched \
 && cp -a /src/deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedance-skill /tmp/patched/ \
 && cp -a /src/deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedream-skill /tmp/patched/ \
 && cd /tmp/patched && git init -q \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0001-seedance-broker-adapter.patch \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0002-seedream-broker-adapter.patch \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0003-seedream-fail-closed-cli.patch \
 && git apply /src/deploy/aurora-sandbox/vendor/volcengine/patches/0004-seedance-fail-closed-cli.patch \
 && rm -rf /tmp/patched/.git \
 && mkdir -p /opt/aurora/vendor/volcengine \
 && cp -a /tmp/patched/byted-ark-seedance-skill /tmp/patched/byted-ark-seedream-skill /opt/aurora/vendor/volcengine/ \
 && rm -rf /tmp/patched \
 && find /opt/aurora/vendor -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}' > /opt/aurora/vendor-tree.sha256

# ---------------------------------------------------------------------------
# Stage 5: Final runtime. Digest-pinned Debian snapshot apt set, root-owned
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
    rm -rf /var/lib/apt/lists/* /var/cache/apt /var/cache/debconf /var/log/apt /tmp/* && \
    rm -rf \
      /etc/apt /etc/dpkg /etc/perl \
      /var/lib/apt /var/lib/dpkg \
      /usr/lib/apt /usr/lib/dpkg /usr/libexec/dpkg /usr/share/apt /usr/share/dpkg \
      /usr/share/perl /usr/share/perl5 \
      /usr/bin/apt /usr/bin/apt-* /usr/bin/dpkg /usr/bin/dpkg-* \
      /usr/bin/perl /usr/bin/perl5* /usr/bin/perlbug /usr/bin/perldoc \
      /usr/bin/perlivp /usr/bin/perlthanks \
      /usr/sbin/dpkg-fsys-usrunmess \
      /usr/lib/*/perl-base /usr/lib/*/perl /usr/lib/*/perl5 \
      /lib/systemd/system/apt-* /lib/systemd/system/dpkg-* \
      /etc/cron.daily/apt-compat /etc/cron.daily/dpkg \
      /etc/logrotate.d/apt /etc/logrotate.d/dpkg

# Runtime payload, all root-owned and non-writable by the sandbox user.
COPY --from=gobuilder --chown=0:0 /out/multica /usr/local/bin/multica
# fleet-node is the fixed node entrypoint; it execs the managed daemon above.
COPY --from=gobuilder --chown=0:0 /out/fleet-node /usr/local/bin/fleet-node
# The pnpm workspace layout is preserved so the broker node_modules symlinks
# keep resolving; the runtime tree lives under /opt/aurora/runtime/deploy/.
COPY --from=nodedeps --chown=0:0 /src /opt/aurora/runtime
COPY --from=vendor --chown=0:0 /opt/aurora/vendor /opt/aurora/vendor
COPY --from=vendor --chown=0:0 /opt/aurora/vendor-tree.sha256 /opt/aurora/vendor-tree.sha256

# The fixed Fleet node layout. Docker seeds a fresh named volume from its
# mountpoint, so /data and /secrets arrive writable by the non-root node without
# any runtime chown.
RUN set -eux; \
    groupadd --gid 10001 aurora; \
    useradd --uid 10001 --gid 10001 --home-dir /data/home --shell /usr/sbin/nologin aurora; \
    install -d -o 10001 -g 10001 -m 0700 /data /data/home /data/workspaces /secrets; \
    rm -f /opt/aurora/runtime/package.json /opt/aurora/runtime/pnpm-lock.yaml \
          /opt/aurora/runtime/pnpm-workspace.yaml /opt/aurora/runtime/.npmrc; \
    rm -rf \
      /usr/local/lib/node_modules/npm \
      /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /usr/local/bin/pnpm /usr/local/bin/pnpx \
      /usr/local/bin/yarn /usr/local/bin/yarnpkg /opt/yarn-* \
      /root/.cache /root/.npm /root/.local/share/pnpm /root/.cache/node/corepack; \
    chown -R 0:0 /opt/aurora /usr/local/bin/multica /usr/local/bin/fleet-node; \
    chmod -R go-w /opt/aurora /usr/local/bin/multica /usr/local/bin/fleet-node; \
    find /opt/aurora -type d -exec chmod 0555 {} +; \
    find /opt/aurora -type f ! -perm /111 -exec chmod 0444 {} +; \
    find /opt/aurora -type f -perm /111 -exec chmod 0555 {} +

# The managed daemon resolves its one agent executable through
# exec.LookPath(envOrDefault("MULTICA_CLAUDE_PATH", "claude")) in
# server/internal/daemon/config.go. The pnpm shim installed by the nodedeps
# stage is not on the base image PATH, so pin the daemon to its absolute path.
# Without this the container exits 1 with "managed mode requires a claude
# executable: exec: \"claude\": executable file not found in $PATH".
# scripts/verify-aurora-sandbox-image.sh proves this exact resolution from the
# image's own environment as the sandbox user.
ENV MULTICA_CLAUDE_PATH=/opt/aurora/runtime/node_modules/.bin/claude
# HOME and the working directory follow the fixed Fleet node layout; the Fleet
# also passes HOME explicitly, so both contracts agree.
ENV HOME=/data/home

USER 10001:10001
WORKDIR /data/workspaces
# Fixed Fleet node entrypoint. It validates the read-only /secrets enrollment
# file and execs the managed daemon; neither a client nor the fleet overrides it.
ENTRYPOINT ["/usr/local/bin/fleet-node", "run"]
CMD []
# Non-shell health check: fleet-node reads the node layout and probes the
# daemon's own managed health endpoint.
HEALTHCHECK --interval=30s --timeout=10s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/fleet-node", "health"]
