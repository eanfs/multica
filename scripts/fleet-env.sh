#!/usr/bin/env bash
# Fleet component helpers, sourced by scripts/dev-env.sh.
#
# Fleet runs in Docker (the Compose `fleet` service in docker-compose.fleet.yml)
# and manages node containers through the mounted Docker socket. Each worktree
# gets its own Compose project (multica-fleet-<namespace>), so starting or
# destroying one checkout never recreates another checkout's Fleet. The service
# only joins the network of this checkout's shared PostgreSQL; it never creates
# a second database or network.
#
# Everything that would start, stop or delete work therefore fails closed when
# the daemon is unreachable; fleet_down is the one exception, because "make down"
# must still succeed on a machine whose daemon was already shut down.
set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

# Compose resolves relative bind-mount sources against the project directory
# (the checkout root). Resolve them the same way here, so the shell helpers and
# the container read the same files.
fleet_host_path() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *) printf '%s/%s' "$REPO_ROOT" "$1" ;;
  esac
}

fleet_compose_file() { printf '%s/docker-compose.fleet.yml' "$REPO_ROOT"; }
fleet_port() { printf '%s' "${MULTICA_FLEET_PORT:-8091}"; }
fleet_url() { printf 'http://127.0.0.1:%s' "$(fleet_port)"; }
fleet_namespace() { printf '%s' "${MULTICA_LOCAL_FLEET_NAMESPACE:-local}"; }

# One Compose project per namespace is the isolation boundary: the compose file
# names the project multica-fleet, and -p narrows it to this checkout. Compose
# would otherwise share the top-level "multica" project with the database and
# every other worktree.
fleet_project() { printf 'multica-fleet-%s' "$(fleet_namespace)"; }
fleet_compose() { docker compose -f "$(fleet_compose_file)" -p "$(fleet_project)" "$@"; }

# Docker backends differ in how a container reaches the host. Docker Desktop
# resolves host.docker.internal itself; Colima (Lima) exposes the host as
# host.lima.internal inside the VM, and a node container may not inherit that
# hosts entry, so the address is resolved through colima when possible. An
# explicit MULTICA_FLEET_HOST_GATEWAY wins for any other backend.
fleet_docker_context() {
  docker context show 2>/dev/null || true
}

fleet_uses_colima() {
  local context
  context="$(fleet_docker_context)"
  case "$context" in
    colima|colima-*) return 0 ;;
  esac
  case "${DOCKER_HOST:-}" in
    */.colima/*|*colima*) return 0 ;;
  esac
  return 1
}

fleet_host_gateway() {
  local ip
  if [ -n "${MULTICA_FLEET_HOST_GATEWAY:-}" ]; then
    printf '%s' "$MULTICA_FLEET_HOST_GATEWAY"
    return 0
  fi
  if fleet_uses_colima; then
    if command -v colima >/dev/null 2>&1; then
      ip="$(colima ssh -- sh -c 'getent hosts host.lima.internal' 2>/dev/null | awk 'NR==1 {print $1}')"
      if [ -n "$ip" ]; then
        printf '%s' "$ip"
        return 0
      fi
    fi
    printf 'host.lima.internal'
    return 0
  fi
  printf 'host.docker.internal'
}

fleet_api_url() {
  printf 'http://%s:%s' "$(fleet_host_gateway)" "${1:-${BACKEND_PORT:-8080}}"
}

# The database's Compose project owns this network. It is external here so the
# two projects can share it without either one creating or deleting it.
fleet_network() { printf 'multica_default'; }
fleet_secret_file() { fleet_host_path "${MULTICA_LOCAL_FLEET_SECRET_FILE:-.dev/fleet-secret}"; }
fleet_credentials_file() { fleet_host_path "${MULTICA_FLEET_CREDENTIALS_FILE:-.dev/fleet-credentials.json}"; }

# Prints why Docker is unusable and returns non-zero; prints nothing on
# success. Keeping the reason in a variable lets fleet_status report it without
# sending diagnostics to stderr on a status read.
fleet_docker_unavailable_reason() {
  if ! command -v docker >/dev/null 2>&1; then
    printf 'docker CLI not found on PATH'
    return 1
  fi
  if ! docker info >/dev/null 2>&1; then
    printf 'Docker daemon is not running'
    return 1
  fi
  return 0
}

fleet_require_docker() {
  local reason
  if ! reason="$(fleet_docker_unavailable_reason)"; then
    printf 'fleet: %s\n' "$reason" >&2
    return 1
  fi
}

# The fleet project attaches to the database project's network instead of
# creating one. Without this preflight, "compose up" fails on a bare
# "network multica_default declared as external, but could not be found" after
# the images were already built, which reads like a Fleet bug.
fleet_require_network() {
  local network
  network="$(fleet_network)"
  if docker network inspect "$network" >/dev/null 2>&1; then
    return 0
  fi
  printf "fleet: Docker network %s is missing. It is created by this checkout's PostgreSQL.\n" "$network" >&2
  printf 'fleet: start the database first (make db-up, or make up C=api), then retry make up C=fleet.\n' >&2
  return 1
}

# The service secret must exist before the API starts: the server reads it at
# boot to enable local Fleet, while fleet_up runs after the API. "up" creates it
# there; fleet_ensure_secret_files calls this again alongside the credentials
# template. Silent on purpose — a caller's stdout can be a machine-readable
# contract.
fleet_ensure_secret_file() {
  local secret
  secret="$(fleet_secret_file)"
  if [ ! -f "$secret" ]; then
    mkdir -p "$(dirname "$secret")" || return 1
    ( umask 077; head -c 32 /dev/urandom | base64 > "$secret" ) || return 1
  fi
  chmod 600 "$secret"
}

# Both files are bind-mounted into the Fleet container. Docker creates a missing
# bind source as a directory, which Fleet then rejects as "not a regular file",
# so generate the local defaults up front with owner-only permissions. The
# files live under .dev/ (git-ignored); no key is ever printed.
fleet_ensure_secret_files() {
  local secret creds had_secret=1
  secret="$(fleet_secret_file)"
  creds="$(fleet_credentials_file)"
  [ -f "$secret" ] || had_secret=0
  fleet_ensure_secret_file || return 1
  if [ "$had_secret" = 0 ]; then
    printf 'fleet: generated a new service secret at %s\n' "$secret"
  fi
  if [ ! -f "$creds" ]; then
    mkdir -p "$(dirname "$creds")" || return 1
    ( umask 077; printf '{"users":{},"profiles":{}}\n' > "$creds" ) || return 1
    printf 'fleet: wrote an empty owner/profile map at %s\n' "$creds"
  fi
  chmod 600 "$creds"
}

fleet_build_images() {
  local runtime_image="${MULTICA_FLEET_RUNTIME_IMAGE:-multica/runtime-claude:dev}"
  docker build -f "$REPO_ROOT/docker/runtime/Dockerfile" -t "$runtime_image" "$REPO_ROOT"
  docker build -f "$REPO_ROOT/docker/fleet/Dockerfile" -t multica/fleet:dev "$REPO_ROOT"
}

fleet_up() {
  fleet_require_docker || return 1
  # Compose substitutes this into the Fleet container and node containers call
  # back to it. Derive it for the active Docker backend unless pinned.
  if [ -z "${MULTICA_FLEET_API_URL:-}" ]; then
    MULTICA_FLEET_API_URL="$(fleet_api_url "${PORT:-8080}")"
  fi
  export MULTICA_FLEET_API_URL

  # Short-circuit when this checkout's service is already serving traffic: a
  # repeated "make up C=fleet" must not rebuild images or recreate a live
  # container (which would drop the node containers attached to it).
  local existing
  if existing="$(fleet_compose ps -q fleet 2>/dev/null)" \
    && [ -n "$existing" ] \
    && curl -fsS --max-time 2 "$(fleet_url)/readyz" >/dev/null 2>&1; then
    printf 'fleet: already running at %s\n' "$(fleet_url)"
    return 0
  fi

  fleet_require_network || return 1
  fleet_ensure_secret_files || return 1
  fleet_build_images || return 1
  fleet_compose up -d fleet || return 1

  local url waited=0 limit="${MULTICA_FLEET_READY_TIMEOUT:-120}"
  url="$(fleet_url)/readyz"
  until curl -fsS --max-time 2 "$url" >/dev/null 2>&1; do
    if [ "$waited" -ge "$limit" ]; then
      printf 'fleet: not ready after %ss (%s); inspect: docker compose -f %s -p %s logs fleet\n' \
        "$limit" "$url" "$(fleet_compose_file)" "$(fleet_project)" >&2
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

fleet_down() {
  local reason
  if ! reason="$(fleet_docker_unavailable_reason)"; then
    printf 'fleet: %s; nothing to stop.\n' "$reason"
    return 0
  fi
  fleet_compose stop fleet >/dev/null || return 1
}

# Output is the state|url|detail triple dev-env.sh renders for every component.
# It never fails: an unreachable daemon is a state to display, not an error that
# would abort "make status" (or the JSON contract agents parse).
fleet_status() {
  local reason container
  if ! reason="$(fleet_docker_unavailable_reason)"; then
    printf 'unavailable|-|%s' "$reason"
    return 0
  fi
  if ! container="$(fleet_compose ps -q fleet 2>/dev/null)"; then
    printf 'error|-|docker compose ps failed'
    return 0
  fi
  if [ -z "$container" ]; then
    printf 'stopped|-|not running'
    return 0
  fi
  if curl -fsS --max-time 2 "$(fleet_url)/readyz" >/dev/null 2>&1; then
    printf 'running|%s|ready' "$(fleet_url)"
  else
    printf 'running|%s|starting' "$(fleet_url)"
  fi
}

# Prints present, absent or unknown. "unknown" (Docker unreachable, or a listing
# that failed halfway) is never "nothing to clean": node containers live outside
# the Compose project, so destroying the only record of them while they cannot
# be listed would leak them forever. dev-env.sh destroy fails closed on it.
# fleet_down is the one lifecycle verb that tolerates an unreachable daemon.
fleet_resources_present() {
  local ns ids
  if ! fleet_docker_unavailable_reason >/dev/null; then
    printf 'unknown\n'
    return 0
  fi
  ns="$(fleet_namespace)"
  if ! ids="$(fleet_compose ps -aq fleet 2>/dev/null)"; then printf 'unknown\n'; return 0; fi
  [ -z "$ids" ] || { printf 'present\n'; return 0; }
  if ! ids="$(docker ps -aq --filter "label=multica.fleet.namespace=$ns" 2>/dev/null)"; then printf 'unknown\n'; return 0; fi
  [ -z "$ids" ] || { printf 'present\n'; return 0; }
  if ! ids="$(docker volume ls -q --filter "label=multica.fleet.namespace=$ns" 2>/dev/null)"; then printf 'unknown\n'; return 0; fi
  [ -z "$ids" ] || { printf 'present\n'; return 0; }
  if ! ids="$(docker network ls -q --filter "label=multica.fleet.namespace=$ns" 2>/dev/null)"; then printf 'unknown\n'; return 0; fi
  [ -z "$ids" ] || { printf 'present\n'; return 0; }
  printf 'absent\n'
}

# Removes this namespace's compose project and the Docker resources Fleet
# created for it. Only that project's service and namespace-labelled resources
# are matched; a global "docker prune" would delete other checkouts' data.
#
# Every failure is counted instead of swallowed: dev-env.sh destroy runs this
# before it drops the database, and a half-removed Fleet must keep the manifest
# so the cleanup can be retried rather than silently leaking node containers.
fleet_destroy() {
  fleet_require_docker || return 1
  local ns failures=0 containers="" volumes="" networks="" id
  ns="$(fleet_namespace)"

  if ! fleet_compose rm -sf fleet >/dev/null 2>&1; then
    printf 'fleet: failed to remove the compose fleet service\n' >&2
    failures=$((failures + 1))
  fi

  if ! containers="$(docker ps -aq --filter "label=multica.fleet.namespace=$ns" 2>/dev/null)"; then
    printf 'fleet: failed to list node containers for namespace %s\n' "$ns" >&2
    failures=$((failures + 1))
    containers=""
  fi
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ! docker rm -f "$id" >/dev/null 2>&1; then
      printf 'fleet: failed to remove node container %s\n' "$id" >&2
      failures=$((failures + 1))
    fi
  done <<EOF
$containers
EOF

  if ! volumes="$(docker volume ls -q --filter "label=multica.fleet.namespace=$ns" 2>/dev/null)"; then
    printf 'fleet: failed to list node volumes for namespace %s\n' "$ns" >&2
    failures=$((failures + 1))
    volumes=""
  fi
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ! docker volume rm "$id" >/dev/null 2>&1; then
      printf 'fleet: failed to remove node volume %s\n' "$id" >&2
      failures=$((failures + 1))
    fi
  done <<EOF
$volumes
EOF

  if ! networks="$(docker network ls --filter "label=multica.fleet.namespace=$ns" --format '{{.Name}}' 2>/dev/null)"; then
    printf 'fleet: failed to list node networks for namespace %s\n' "$ns" >&2
    failures=$((failures + 1))
    networks=""
  fi
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ! docker network rm "$id" >/dev/null 2>&1; then
      printf 'fleet: failed to remove node network %s\n' "$id" >&2
      failures=$((failures + 1))
    fi
  done <<EOF
$networks
EOF

  if [ "$failures" -ne 0 ]; then
    printf 'fleet: %s cleanup failure(s); namespace %s is not fully removed\n' "$failures" "$ns" >&2
    return 1
  fi
}
