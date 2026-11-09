#!/usr/bin/env bash
# Fleet component helpers, sourced by scripts/dev-env.sh.
#
# Fleet runs in Docker (the Compose `fleet` service) and manages node
# containers through the mounted Docker socket. Everything that would start,
# stop or delete work therefore fails closed when the daemon is unreachable;
# fleet_down is the one exception, because "make down" must still succeed on a
# machine whose daemon was already shut down.
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

fleet_compose_file() { printf '%s/docker-compose.yml' "$REPO_ROOT"; }
fleet_port() { printf '%s' "${MULTICA_FLEET_PORT:-8091}"; }
fleet_url() { printf 'http://127.0.0.1:%s' "$(fleet_port)"; }
fleet_namespace() { printf '%s' "${MULTICA_LOCAL_FLEET_NAMESPACE:-local}"; }
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

# Both files are bind-mounted into the Fleet container. Docker creates a missing
# bind source as a directory, which Fleet then rejects as "not a regular file",
# so generate the local defaults up front with owner-only permissions. The
# files live under .dev/ (git-ignored); no key is ever printed.
fleet_ensure_secret_files() {
  local secret creds
  secret="$(fleet_secret_file)"
  creds="$(fleet_credentials_file)"
  mkdir -p "$(dirname "$secret")" "$(dirname "$creds")"
  if [ ! -f "$secret" ]; then
    ( umask 077; head -c 32 /dev/urandom | base64 > "$secret" )
    printf 'fleet: generated a new service secret at %s\n' "$secret"
  fi
  chmod 600 "$secret"
  if [ ! -f "$creds" ]; then
    ( umask 077; printf '{"users":{},"profiles":{}}\n' > "$creds" )
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

  # Short-circuit when this checkout's service is already serving traffic: a
  # repeated "make up C=fleet" must not rebuild images or recreate a live
  # container (which would drop the node containers attached to it).
  local existing
  if existing="$(docker compose -f "$(fleet_compose_file)" ps -q fleet 2>/dev/null)" \
    && [ -n "$existing" ] \
    && curl -fsS --max-time 2 "$(fleet_url)/readyz" >/dev/null 2>&1; then
    printf 'fleet: already running at %s\n' "$(fleet_url)"
    return 0
  fi

  fleet_ensure_secret_files || return 1
  fleet_build_images || return 1
  docker compose -f "$(fleet_compose_file)" up -d fleet || return 1

  local url waited=0 limit="${MULTICA_FLEET_READY_TIMEOUT:-120}"
  url="$(fleet_url)/readyz"
  until curl -fsS --max-time 2 "$url" >/dev/null 2>&1; do
    if [ "$waited" -ge "$limit" ]; then
      printf 'fleet: not ready after %ss (%s); inspect: docker compose -f %s logs fleet\n' \
        "$limit" "$url" "$(fleet_compose_file)" >&2
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
  docker compose -f "$(fleet_compose_file)" stop fleet >/dev/null || return 1
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
  if ! container="$(docker compose -f "$(fleet_compose_file)" ps -q fleet 2>/dev/null)"; then
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

# True when this checkout has anything for Fleet to remove: the compose
# service (running or stopped) or namespace-labelled node containers, volumes or
# networks. An unreachable daemon is "nothing to clean", so destroy can still
# run on a machine whose Docker Desktop was closed.
fleet_resources_present() {
  local reason ns
  if ! reason="$(fleet_docker_unavailable_reason)"; then
    return 1
  fi
  ns="$(fleet_namespace)"
  [ -z "$(docker compose -f "$(fleet_compose_file)" ps -aq fleet 2>/dev/null || true)" ] || return 0
  [ -z "$(docker ps -aq --filter "label=multica.fleet.namespace=$ns" 2>/dev/null || true)" ] || return 0
  [ -z "$(docker volume ls -q --filter "label=multica.fleet.namespace=$ns" 2>/dev/null || true)" ] || return 0
  [ -z "$(docker network ls -q --filter "label=multica.fleet.namespace=$ns" 2>/dev/null || true)" ] || return 0
  return 1
}

# Removes the compose service and the Docker resources Fleet created for this
# checkout's namespace. Only namespace-labelled resources are matched; a global
# "docker prune" would delete other checkouts' data.
#
# Every failure is counted instead of swallowed: dev-env.sh destroy runs this
# before it drops the database, and a half-removed Fleet must keep the manifest
# so the cleanup can be retried rather than silently leaking node containers.
fleet_destroy() {
  fleet_require_docker || return 1
  local ns failures=0 containers="" volumes="" networks="" id
  ns="$(fleet_namespace)"

  if ! docker compose -f "$(fleet_compose_file)" rm -sf fleet >/dev/null 2>&1; then
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
