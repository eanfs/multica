#!/usr/bin/env bash
# Positive and negative test for verify-aurora-sandbox-managed-agent.sh.
#
# Builds two minimal images that differ only in how the agent launcher is
# exposed to the managed daemon:
#
#   misplaced  the launcher exists at /opt/agent/claude but is NOT on PATH and
#              MULTICA_CLAUDE_PATH is unset. This is exactly the class of defect
#              that shipped latent: the content verifier proved the shim existed
#              and ran, but the daemon's exec.LookPath("claude") failed and the
#              sandbox container exited 1 at startup. The probe MUST reject it.
#   declared   the same image plus ENV MULTICA_CLAUDE_PATH=/opt/agent/claude.
#              The probe MUST accept it.
#
# The test fails if either expectation is inverted, and never calls a provider.
#
# Usage:
#   scripts/verify-aurora-sandbox-managed-agent.test.sh
#
# The base image defaults to the digest-pinned Node runtime the sandbox locks;
# override with AURORA_MANAGED_AGENT_TEST_BASE.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
probe="$script_dir/verify-aurora-sandbox-managed-agent.sh"

base="${AURORA_MANAGED_AGENT_TEST_BASE:-node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c}"

fail() { printf 'verify-aurora-sandbox-managed-agent.test: FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'verify-aurora-sandbox-managed-agent.test: ok: %s\n' "$*"; }

command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
[ -f "$probe" ] || fail "missing $probe"
[ -f "$repo_root/deploy/aurora-sandbox/Dockerfile" ] || fail "test must run from the repository"

work="$(mktemp -d)"
cleanup() {
  docker image rm -f aurora-managed-agent-misplaced aurora-managed-agent-declared >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# The launcher is a stand-in for the real Claude Code shim: it must be
# executable by the sandbox user and report a version.
cat >"$work/Dockerfile.misplaced" <<DOCKER
FROM $base
RUN groupadd --gid 10001 aurora \
 && useradd --uid 10001 --gid 10001 --no-create-home aurora \
 && mkdir -p /opt/agent \
 && printf '#!/bin/sh\necho "2.1.282 (Claude Code)"\n' > /opt/agent/claude \
 && chmod 0555 /opt/agent/claude
DOCKER

# Identical image, but the daemon is pointed at the launcher. This is the fix.
cat >"$work/Dockerfile.declared" <<DOCKER
FROM $base
RUN groupadd --gid 10001 aurora \
 && useradd --uid 10001 --gid 10001 --no-create-home aurora \
 && mkdir -p /opt/agent \
 && printf '#!/bin/sh\necho "2.1.282 (Claude Code)"\n' > /opt/agent/claude \
 && chmod 0555 /opt/agent/claude
ENV MULTICA_CLAUDE_PATH=/opt/agent/claude
DOCKER

docker build -q -t aurora-managed-agent-misplaced -f "$work/Dockerfile.misplaced" "$work" >/dev/null
docker build -q -t aurora-managed-agent-declared -f "$work/Dockerfile.declared" "$work" >/dev/null
pass "built the misplaced (latent-defect) and declared (fixed) images"

if "$probe" aurora-managed-agent-misplaced >"$work/misplaced.out" 2>&1; then
  cat "$work/misplaced.out" >&2
  fail "the probe accepted an image whose agent launcher is unreachable from the daemon PATH"
fi
grep -q 'UNRESOLVED' "$work/misplaced.out" || {
  cat "$work/misplaced.out" >&2
  fail "the probe rejected the latent-defect image without naming the resolution failure"
}
pass "the probe rejects the latent-defect image (UNRESOLVED)"

if ! "$probe" aurora-managed-agent-declared >"$work/declared.out" 2>&1; then
  cat "$work/declared.out" >&2
  fail "the probe rejected an image that declares a resolvable agent launcher"
fi
grep -q 'MANAGED_AGENT ' "$work/declared.out" || {
  cat "$work/declared.out" >&2
  fail "the probe accepted the fixed image without reporting the resolution"
}
pass "the probe accepts the fixed image ($(grep 'MANAGED_AGENT ' "$work/declared.out" | head -n1))"

printf 'verify-aurora-sandbox-managed-agent.test: PASS\n'
