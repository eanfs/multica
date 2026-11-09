#!/usr/bin/env bash
# scripts/fleet-env.sh behaviour, with a fake Docker CLI and a fake curl.
#
# Nothing here starts a container, reaches a Docker daemon, or opens a network
# socket: the fake docker only appends its argv to a log, and the fake curl
# answers the readyz probe. That keeps the suite runnable on a machine where
# Docker Desktop is closed (which is exactly where fleet_up must fail loudly).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_bin="$tmp/bin"
mkdir -p "$fake_bin"

export DOCKER_LOG="$tmp/docker.log"
export CURL_LOG="$tmp/curl.log"
: > "$DOCKER_LOG"
: > "$CURL_LOG"

cat > "$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
printf 'docker %s\n' "$*" >> "$DOCKER_LOG"
case "$*" in
  *"context show"*) printf '%s\n' "${FAKE_DOCKER_CONTEXT:-desktop-linux}" ;;
  *"ps -q fleet"*|*"ps -aq fleet"*) [ "${FAKE_DOCKER_NO_CONTAINER:-0}" = 1 ] || printf 'container-id\n' ;;
  *"network inspect multica_default"*) [ "${FAKE_DOCKER_NO_NETWORK:-0}" = 1 ] && exit 1 ;;
esac
exit 0
EOF

cat > "$fake_bin/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$CURL_LOG"
[ "${FAKE_CURL_FAIL:-0}" != 1 ]
EOF

chmod +x "$fake_bin/docker" "$fake_bin/curl"

fail_bin="$tmp/fail-bin"
mkdir -p "$fail_bin"
cat > "$fail_bin/docker" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = info ] && exit 1
exit 0
EOF
chmod +x "$fail_bin/docker"

export PATH="$fake_bin:$PATH"
export MULTICA_FLEET_PORT=8092
export MULTICA_FLEET_RUNTIME_IMAGE=multica/runtime-claude:dev
export MULTICA_LOCAL_FLEET_SECRET_FILE="$tmp/dev/fleet-secret"
export MULTICA_FLEET_CREDENTIALS_FILE="$tmp/dev/fleet-credentials.json"
export MULTICA_LOCAL_FLEET_NAMESPACE=fleet-test
unset ENV_FILE || true
unset MULTICA_FLEET_READY_TIMEOUT || true

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_log() {
  local log=$1 expected=$2
  grep -Fq -- "$expected" "$log" || fail "expected '$expected' in $log"
}

file_mode() {
  stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}

# shellcheck disable=SC1090
source "$root/scripts/fleet-env.sh"

# ---------------------------------------------------------------------------
# The container-to-host address follows the Docker backend: Docker Desktop
# resolves host.docker.internal, Colima resolves the Lima host (through the
# colima CLI when available, host.lima.internal otherwise). Compose receives
# the derived URL from fleet_up.
# ---------------------------------------------------------------------------
[ "$(FAKE_DOCKER_CONTEXT=desktop-linux fleet_host_gateway)" = "host.docker.internal" ] \
  || fail "Docker Desktop must use host.docker.internal"
[ "$(FAKE_DOCKER_CONTEXT=desktop-linux fleet_api_url 8080)" = "http://host.docker.internal:8080" ] \
  || fail "Docker Desktop API URL is wrong"

no_colima_path="$fake_bin:/usr/bin:/bin:/usr/sbin:/sbin"
[ "$(PATH="$no_colima_path" FAKE_DOCKER_CONTEXT=colima fleet_host_gateway)" = "host.lima.internal" ] \
  || fail "Colima without the CLI must fall back to host.lima.internal"

colima_bin="$tmp/colima-bin"
mkdir -p "$colima_bin"
cat > "$colima_bin/colima" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = ssh ] && printf '192.168.5.2 host.lima.internal\n'
EOF
chmod +x "$colima_bin/colima"
[ "$(PATH="$colima_bin:$no_colima_path" FAKE_DOCKER_CONTEXT=colima fleet_host_gateway)" = "192.168.5.2" ] \
  || fail "Colima must resolve the Lima host address through colima ssh"

# ---------------------------------------------------------------------------
# fleet_up: build both images, start the compose service, wait for readyz, and
# leave the two private files Fleet needs on disk with owner-only access.
# ---------------------------------------------------------------------------
FAKE_DOCKER_NO_CONTAINER=1 fleet_up || fail "fleet_up failed"

require_log "$DOCKER_LOG" "docker build -f $root/docker/runtime/Dockerfile"
require_log "$DOCKER_LOG" "docker build -f $root/docker/fleet/Dockerfile"
# The lifecycle must use the isolated fleet file AND the per-namespace project;
# the top-level compose project belongs to the database and every worktree.
require_log "$DOCKER_LOG" "docker compose -f $root/docker-compose.fleet.yml -p multica-fleet-fleet-test up -d fleet"
require_log "$DOCKER_LOG" "network inspect multica_default"
if grep -Fq "docker compose -f $root/docker-compose.yml" "$DOCKER_LOG"; then
  fail "fleet lifecycle used the shared docker-compose.yml project"
fi
require_log "$CURL_LOG" "http://127.0.0.1:8092/readyz"

[ -s "$MULTICA_LOCAL_FLEET_SECRET_FILE" ] || fail "fleet_up did not create the service secret"
[ -s "$MULTICA_FLEET_CREDENTIALS_FILE" ] || fail "fleet_up did not create the credentials file"
[ "$(file_mode "$MULTICA_LOCAL_FLEET_SECRET_FILE")" = 600 ] || fail "fleet secret mode is not 0600"
[ "$(file_mode "$MULTICA_FLEET_CREDENTIALS_FILE")" = 600 ] || fail "fleet credentials mode is not 0600"

# ---------------------------------------------------------------------------
# A ready service is not rebuilt or recreated by a repeated "make up C=fleet".
# ---------------------------------------------------------------------------
builds_before="$(grep -c 'docker build' "$DOCKER_LOG" || true)"
ups_before="$(grep -c 'up -d fleet' "$DOCKER_LOG" || true)"
up_output="$(fleet_up)" || fail "fleet_up on a ready service failed"
case "$up_output" in
  *"already running"*) ;;
  *) fail "fleet_up did not short-circuit a ready service: $up_output" ;;
esac
[ "$(grep -c 'docker build' "$DOCKER_LOG" || true)" = "$builds_before" ] \
  || fail "fleet_up rebuilt images for a ready service"
[ "$(grep -c 'up -d fleet' "$DOCKER_LOG" || true)" = "$ups_before" ] \
  || fail "fleet_up recreated a ready service"

# ---------------------------------------------------------------------------
# fleet_status: ask compose for the service, then report ready only when the
# readyz endpoint actually answers. It prints state|url|detail.
# ---------------------------------------------------------------------------
status="$(fleet_status)"
[ "$status" = "running|http://127.0.0.1:8092|ready" ] || fail "fleet_status = $status"
require_log "$DOCKER_LOG" "-p multica-fleet-fleet-test ps -q fleet"

status="$(
  export FAKE_CURL_FAIL=1
  fleet_status
)"
[ "$status" = "running|http://127.0.0.1:8092|starting" ] || fail "unready fleet_status = $status"

status="$(
  export FAKE_DOCKER_NO_CONTAINER=1
  fleet_status
)"
[ "$status" = "stopped|-|not running" ] || fail "stopped fleet_status = $status"

# "Anything to clean" is exactly what destroy needs to know before it decides
# whether to touch Docker at all. A daemon that cannot be reached is a third
# state, never "absent": the node containers live outside the compose project.
[ "$(fleet_resources_present)" = present ] || fail "a running fleet service must count as present"
[ "$(FAKE_DOCKER_NO_CONTAINER=1 fleet_resources_present)" = absent ] \
  || fail "an absent fleet service must count as absent"
[ "$(PATH="$fail_bin:$PATH" fleet_resources_present)" = unknown ] \
  || fail "an unreachable daemon must count as unknown, not absent"

# ---------------------------------------------------------------------------
# docker compose is the lifecycle: stop for down, rm for destroy. Cleanup is
# scoped to this checkout's Fleet namespace label; a global prune is forbidden.
# ---------------------------------------------------------------------------
fleet_down || fail "fleet_down failed"
require_log "$DOCKER_LOG" "-p multica-fleet-fleet-test stop fleet"

fleet_destroy || fail "fleet_destroy failed"
require_log "$DOCKER_LOG" "-p multica-fleet-fleet-test rm -sf fleet"
require_log "$DOCKER_LOG" "label=multica.fleet.namespace=fleet-test"
if grep -Fq 'prune' "$DOCKER_LOG"; then
  fail "fleet_destroy must never run a global prune"
fi

# Projects are per namespace, so another worktree's namespace addresses a
# different project and filters labels for its own resources only.
[ "$(fleet_project)" = "multica-fleet-fleet-test" ] || fail "fleet_project = $(fleet_project)"
[ "$(MULTICA_LOCAL_FLEET_NAMESPACE=other-worktree fleet_project)" = "multica-fleet-other-worktree" ] \
  || fail "fleet_project is not namespace-scoped"
[ "$(fleet_compose_file)" = "$root/docker-compose.fleet.yml" ] \
  || fail "fleet_compose_file = $(fleet_compose_file)"
: > "$DOCKER_LOG"
MULTICA_LOCAL_FLEET_NAMESPACE=other-worktree fleet_destroy || fail "fleet_destroy for another namespace failed"
require_log "$DOCKER_LOG" "-p multica-fleet-other-worktree rm -sf fleet"
require_log "$DOCKER_LOG" "label=multica.fleet.namespace=other-worktree"
if grep -Fq 'namespace=fleet-test' "$DOCKER_LOG"; then
  fail "destroy for another namespace touched fleet-test resources"
fi

# ---------------------------------------------------------------------------
# An unreachable daemon is a hard error for anything that would start or delete
# work, and a successful no-op for stop (down/destroy must not wedge on it).
# ---------------------------------------------------------------------------
: > "$DOCKER_LOG"
set +e
(
  export PATH="$fail_bin:$PATH"
  fleet_up
) >"$tmp/unavailable.out" 2>"$tmp/unavailable.err"
rc=$?
set -e
[ "$rc" -ne 0 ] || fail "fleet_up succeeded without a Docker daemon"
grep -Fq 'Docker daemon is not running' "$tmp/unavailable.err" \
  || fail "fleet_up did not explain the unreachable Docker daemon"
if grep -Fq 'up -d fleet' "$DOCKER_LOG"; then
  fail "fleet_up touched compose without a Docker daemon"
fi

status="$(
  export PATH="$fail_bin:$PATH"
  fleet_status
)"
[ "$status" = "unavailable|-|Docker daemon is not running" ] || fail "unavailable fleet_status = $status"

(
  export PATH="$fail_bin:$PATH"
  fleet_down
) >/dev/null || fail "fleet_down must be a no-op when Docker is unreachable"

# Ready wait is bounded: a container that never answers readyz must fail fast
# instead of hanging the environment start.
rc=0
(
  export FAKE_CURL_FAIL=1 MULTICA_FLEET_READY_TIMEOUT=0
  fleet_up
) >"$tmp/timeout.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail "fleet_up succeeded while readyz never answered"
grep -Fq 'not ready' "$tmp/timeout.out" || fail "fleet_up did not report the readiness timeout"

# The isolated project joins the database project's network instead of creating
# one. A missing network must fail with the database preflight, before images
# are built or compose starts.
: > "$DOCKER_LOG"
rc=0
(
  export FAKE_DOCKER_NO_CONTAINER=1 FAKE_DOCKER_NO_NETWORK=1
  fleet_up
) >"$tmp/no-network.out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail "fleet_up succeeded without the postgres network"
grep -Fq 'network multica_default is missing' "$tmp/no-network.out" \
  || fail "fleet_up did not explain the missing network"
grep -Fq 'make db-up' "$tmp/no-network.out" \
  || fail "fleet_up did not point at the database preflight"
if grep -Eq 'docker build|up -d fleet' "$DOCKER_LOG"; then
  fail "fleet_up built or started compose before the network preflight"
fi

echo "✓ fleet-env.sh compose lifecycle verified"
