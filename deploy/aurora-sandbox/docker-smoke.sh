#!/usr/bin/env bash
# Aurora sandbox macOS Docker Desktop functional smoke.
#
# This script exercises the complete Plan B development path on Docker Desktop:
# the authenticated fleet control API, the hardened Docker backend, and the
# enforced egress sidecar. It starts fake Multica control and fake provider
# endpoints, ensures one workspace node through the fleet API, polls until the
# node is online and healthy, confirms a repeat ensure returns the same node,
# then deletes it and asserts no labeled resource or staged secret survives.
#
# Docker Desktop's Linux virtual machine does not enforce the AppArmor profile
# or the Linux cgroup/pids/namespace semantics, so this is functional evidence
# only. It never substitutes for the Linux acceptance in
# deploy/aurora-sandbox/docker-security-test.sh, and it must not be read as
# satisfying Task 6, issue #29, or the master plan's Linux security gates.
#
# The script never calls a real provider or agent: the only network targets are
# the local fake endpoints and the local Docker daemon.
#
# Usage:
#   deploy/aurora-sandbox/docker-smoke.sh
#   AURORA_SANDBOX_IMAGE='repo@sha256:...' AURORA_PROXY_IMAGE='repo@sha256:...' \
#     deploy/aurora-sandbox/docker-smoke.sh
#
# Set both image variables to reuse digest-pinned fixture images; otherwise the
# script cross-compiles and builds the fixture image pair locally.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
server_dir="$repo_root/server"

fleet_pid=""
fake_pid=""
staging=""
api_body=""
created_uplink=0
sandbox=""
proxy=""
network=""

log() { printf 'docker-smoke: %s\n' "$*"; }
fail() { printf 'docker-smoke: ERROR: %s\n' "$*" >&2; exit 1; }

# cleanup removes every process, container, network, and temporary secret this
# script created, including on failure. It never removes the fleet uplink when
# another operator or test already owned it.
cleanup() {
  if [ -n "$fleet_pid" ]; then
    kill "$fleet_pid" >/dev/null 2>&1 || true
    wait "$fleet_pid" >/dev/null 2>&1 || true
  fi
  if [ -n "$fake_pid" ]; then
    kill "$fake_pid" >/dev/null 2>&1 || true
    wait "$fake_pid" >/dev/null 2>&1 || true
  fi
  if [ -n "$sandbox" ]; then docker rm -f "$sandbox" >/dev/null 2>&1 || true; fi
  if [ -n "$proxy" ]; then docker rm -f "$proxy" >/dev/null 2>&1 || true; fi
  if [ -n "$network" ]; then docker network rm "$network" >/dev/null 2>&1 || true; fi
  if [ "$created_uplink" -eq 1 ]; then docker network rm aurora-egress-uplink >/dev/null 2>&1 || true; fi
  if [ -n "$staging" ]; then rm -rf "$staging"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# require_digest fails unless the image reference carries an immutable digest.
require_digest() {
  local image="$1"
  case "$image" in
    *@sha256:*) ;;
    *) fail "image $image must be digest-pinned as <name>@sha256:<64 lowercase hex>" ;;
  esac
  printf '%s' "$image" | grep -Eq '@sha256:[0-9a-f]{64}$' || fail "image $image digest must be 64 lowercase hex"
}

# image_ref resolves a locally tagged image to the digest-qualified reference
# the hardened policy requires.
image_ref() {
  local tag="$1" ref id repo
  ref="$(docker inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$tag" 2>/dev/null || true)"
  if [ -z "$ref" ]; then
    id="$(docker inspect --format '{{.Id}}' "$tag")"
    repo="${tag%:*}"
    ref="$repo@$id"
  fi
  require_digest "$ref"
  printf '%s' "$ref"
}

# free_port returns an unbound loopback TCP port for the fleet listener.
free_port() {
  local p
  for p in $(jot -r 100 20000 29999 2>/dev/null || shuf -i 20000-29999 -n 100 2>/dev/null || true); do
    if ! nc -z 127.0.0.1 "$p" >/dev/null 2>&1; then
      printf '%s' "$p"
      return 0
    fi
  done
  return 1
}

for tool in docker go curl openssl nc uuidgen; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

docker info >/dev/null 2>&1 || fail "Docker Desktop is not reachable"
docker_os="$(docker info --format '{{.OperatingSystem}}')"
case "$docker_os" in
  *"Docker Desktop"*) ;;
  *) fail "this smoke requires Docker Desktop (docker reports '$docker_os'); use deploy/aurora-sandbox/docker-security-test.sh on Linux" ;;
esac

docker_arch="$(docker info --format '{{.Architecture}}')"
case "$docker_arch" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) fail "unsupported Docker architecture $docker_arch" ;;
esac
log "Docker Desktop $docker_os ($docker_arch); docker client $(docker version --format '{{.Client.Version}}') server $(docker version --format '{{.Server.Version}}')"

staging="$(cd "$(mktemp -d)" && pwd -P)"
api_body="$staging/api-body.json"

# 1. Temporary control token and enrollment secret, both mode 0400.
#
# The bearer value is the decoded token, so the token is 32 printable random
# bytes and the file stores its base64url encoding: LoadControlAuth decodes the
# file and the fleet compares the decoded bytes.
raw_token="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 32 || true)"
[ "${#raw_token}" -eq 32 ] || fail "failed to generate a 32-character control token"
token_file="$staging/fleet-control-token"
printf '%s' "$raw_token" | openssl base64 -A | tr '+/' '-_' | tr -d '=' >"$token_file"
chmod 400 "$token_file"

enrollment_secret="mse_$(openssl rand -hex 20)"
printf '%s' "$enrollment_secret" >"$staging/enrollment-secret"
chmod 400 "$staging/enrollment-secret"

secret_root="$staging/secrets"
mkdir -p "$secret_root"
chmod 700 "$secret_root"
log "staged a 0400 control token and a 0400 enrollment secret under a 0700 secret root"

# 2. Fixture image pair: reuse the operator's digest-pinned images or build the
# local fixture pair for the Docker Desktop architecture.
sandbox_image="${AURORA_SANDBOX_IMAGE:-}"
proxy_image="${AURORA_PROXY_IMAGE:-}"
if [ -n "$sandbox_image" ] || [ -n "$proxy_image" ]; then
  [ -n "$sandbox_image" ] && [ -n "$proxy_image" ] || fail "set both AURORA_SANDBOX_IMAGE and AURORA_PROXY_IMAGE, or neither"
  require_digest "$sandbox_image"
  require_digest "$proxy_image"
  docker image inspect "$sandbox_image" >/dev/null 2>&1 || fail "sandbox image $sandbox_image is not present locally (the policy runs with --pull never)"
  docker image inspect "$proxy_image" >/dev/null 2>&1 || fail "proxy image $proxy_image is not present locally (the policy runs with --pull never)"
else
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$staging/aurora-sandbox-probe" ./cmd/aurora-sandbox-probe )
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$staging/aurora-egress-proxy" ./cmd/aurora-egress-proxy )
  docker build -q -f "$script_dir/fixture/Dockerfile.sandbox" -t "multica-aurora-sandbox-smoke:local" "$staging" >/dev/null
  docker build -q -f "$script_dir/fixture/Dockerfile.egress" -t "multica-aurora-egress-smoke:local" "$staging" >/dev/null
  sandbox_image="$(image_ref "multica-aurora-sandbox-smoke:local")"
  proxy_image="$(image_ref "multica-aurora-egress-smoke:local")"
fi
seccomp_profile="$repo_root/deploy/aurora-sandbox/seccomp.json"
[ -f "$seccomp_profile" ] || fail "missing seccomp profile: $seccomp_profile"
log "sandbox image $sandbox_image"
log "proxy image   $proxy_image"
log "seccomp       $seccomp_profile"

# 3. Fake Multica control origin and fake provider endpoint. They bind ephemeral
# host ports and report them through a ready file; the sandbox reaches them only
# through the egress sidecar as host.docker.internal.
cat >"$staging/smoke_fakes.go" <<'GO'
// Command smoke-fakes serves the fake Multica origin and the fake provider
// endpoint used by the Docker Desktop functional smoke. It writes the bound
// ports to the file named by -ready, then serves until killed.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	ready := flag.String("ready", "", "file to write the origin and provider ports to")
	flag.Parse()

	origin, err := listen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "origin listener:", err)
		os.Exit(1)
	}
	provider, err := listen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "provider listener:", err)
		os.Exit(1)
	}
	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(fmt.Sprintf("%d %d\n", origin.port, provider.port)), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write ready file:", err)
			os.Exit(1)
		}
	}
	go serve(origin, "aurora-smoke-origin")
	serve(provider, "aurora-smoke-provider")
}

type listener struct {
	ln   net.Listener
	port int
}

func listen() (listener, error) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return listener{}, err
	}
	return listener{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}, nil
}

func serve(l listener, body string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	_ = http.Serve(l.ln, mux)
}
GO
( cd "$staging" && go build -o "$staging/smoke-fakes" smoke_fakes.go )
ready_file="$staging/fakes.ready"
"$staging/smoke-fakes" -ready "$ready_file" >"$staging/fakes.log" 2>&1 &
fake_pid=$!
deadline=$(( $(date +%s) + 15 ))
while [ ! -s "$ready_file" ]; do
  kill -0 "$fake_pid" 2>/dev/null || fail "fake endpoint server exited: $(cat "$staging/fakes.log")"
  [ "$(date +%s)" -lt "$deadline" ] || fail "fake endpoints never became ready"
  sleep 0.2
done
read -r origin_port provider_port <"$ready_file" || true
origin="http://host.docker.internal:$origin_port"
log "fake Multica origin $origin; fake provider endpoint http://host.docker.internal:$provider_port"

# 4. Pre-create the fleet uplink bridge (an operator step in production) and
# start the fleet controller with the full immutable Docker policy.
if docker network inspect aurora-egress-uplink >/dev/null 2>&1; then
  log "reusing the existing aurora-egress-uplink network"
else
  docker network create aurora-egress-uplink >/dev/null
  created_uplink=1
  log "created the aurora-egress-uplink network"
fi

fleet_port="$(free_port)" || fail "could not find a free loopback port for the fleet"
fleet_addr="127.0.0.1:$fleet_port"
fleet_url="http://$fleet_addr"
( cd "$server_dir" && go build -trimpath -o "$staging/aurora-fleet" ./cmd/aurora-fleet )
AURORA_FLEET_ADDR="$fleet_addr" \
  AURORA_FLEET_BACKEND=docker \
  AURORA_FLEET_CONTROL_TOKEN_FILE="$token_file" \
  AURORA_FLEET_SECRET_ROOT="$secret_root" \
  AURORA_SANDBOX_IMAGE="$sandbox_image" \
  AURORA_PROXY_IMAGE="$proxy_image" \
  AURORA_SECCOMP_PROFILE="$seccomp_profile" \
  AURORA_EGRESS_SERVER_ORIGIN="$origin" \
  "$staging/aurora-fleet" >"$staging/fleet.log" 2>&1 &
fleet_pid=$!

deadline=$(( $(date +%s) + 30 ))
until curl -fsS "$fleet_url/healthz" >/dev/null 2>&1; do
  kill -0 "$fleet_pid" 2>/dev/null || fail "fleet exited before listening: $(tail -n 20 "$staging/fleet.log")"
  [ "$(date +%s)" -lt "$deadline" ] || fail "fleet never became healthy"
  sleep 0.5
done
until curl -fsS -H "Authorization: Bearer $raw_token" "$fleet_url/readyz" >/dev/null 2>&1; do
  kill -0 "$fleet_pid" 2>/dev/null || fail "fleet exited before becoming ready: $(tail -n 20 "$staging/fleet.log")"
  [ "$(date +%s)" -lt "$deadline" ] || fail "fleet node backend never became ready"
  sleep 0.5
done
log "fleet listening at $fleet_url with the docker backend"

# api_call performs one authenticated fleet request and writes the body to
# api_body. The HTTP status is printed on stdout.
api_call() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS -o "$api_body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $raw_token")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  curl "${args[@]}" "$fleet_url$path" || true
}

# 5. Ensure one workspace node through the real authenticated API.
node_id="$(uuidgen | tr 'A-Z' 'a-z')"
workspace_id="$(uuidgen | tr 'A-Z' 'a-z')"
runtime_id="$(uuidgen | tr 'A-Z' 'a-z')"
daemon_id="$(uuidgen | tr 'A-Z' 'a-z')"
hash="$(printf 'workspace=%s;node=%s' "$workspace_id" "$node_id" | openssl dgst -sha256 | awk '{print $NF}' | cut -c1-16)"
sandbox="aurora-sbx-$hash"
proxy="aurora-egr-$hash"
network="aurora-ws-$hash"

ensure_body="$(printf '{"node_id":"%s","workspace_id":"%s","runtime_id":"%s","daemon_id":"%s","enrollment_token":"%s"}' \
  "$node_id" "$workspace_id" "$runtime_id" "$daemon_id" "$enrollment_secret")"
code="$(api_call PUT "/internal/v1/workspace-nodes/$node_id" "$ensure_body")"
[ "$code" = "200" ] || fail "ensure returned HTTP $code: $(cat "$api_body")"
returned_sandbox="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' "$api_body")"
[ "$returned_sandbox" = "$sandbox" ] || fail "ensure returned node id '$returned_sandbox', want policy-derived '$sandbox'"
log "ensured workspace node $node_id (sandbox $sandbox)"

# 6. Wait for online/healthy state by polling the fleet API with a 90s deadline.
poll_deadline=$(( $(date +%s) + 90 ))
state=""
health=""
while :; do
  code="$(api_call GET "/internal/v1/workspace-nodes/$node_id")"
  if [ "$code" = "200" ]; then
    state="$(sed -n 's/.*"state":"\([^"]*\)".*/\1/p' "$api_body")"
    health="$(sed -n 's/.*"health":"\([^"]*\)".*/\1/p' "$api_body")"
  fi
  if [ "$state" = "online" ] && [ "$health" = "healthy" ]; then
    break
  fi
  [ "$(date +%s)" -lt "$poll_deadline" ] || fail "node $node_id never reached online/healthy within 90s (last HTTP $code, state '$state', health '$health')"
  sleep 1
done
log "node $node_id is online and healthy"

# The sandbox reaches the fake Multica origin only through the egress sidecar,
# and the fake provider endpoint stays unreachable through the same policy.
origin_url="$origin/aurora-smoke"
provider_url="http://host.docker.internal:$provider_port/"
probe() { docker exec "$sandbox" /opt/aurora/bin/aurora-sandbox-probe "$@"; }
egress_deadline=$(( $(date +%s) + 30 ))
while :; do
  probe_out="$(probe proxy-get "$origin_url" 2>&1 || true)"
  case "$probe_out" in
    *'"allowed":true'*) break ;;
  esac
  [ "$(date +%s)" -lt "$egress_deadline" ] || fail "sandbox never reached the fake Multica origin through the egress sidecar: $probe_out"
  sleep 0.5
done
denied_out="$(probe proxy-get "$provider_url" 2>&1 || true)"
case "$denied_out" in
  *'"allowed":false'*) ;;
  *) fail "sandbox reached the fake provider endpoint through the egress sidecar: $denied_out" ;;
esac
log "egress allows the exact fake Multica origin and refuses the fake provider endpoint"

# 7. A second ensure must confirm the same running node.
enrollment_secret2="mse_$(openssl rand -hex 20)"
ensure_body2="$(printf '{"node_id":"%s","workspace_id":"%s","runtime_id":"%s","daemon_id":"%s","enrollment_token":"%s"}' \
  "$node_id" "$workspace_id" "$runtime_id" "$daemon_id" "$enrollment_secret2")"
code="$(api_call PUT "/internal/v1/workspace-nodes/$node_id" "$ensure_body2")"
[ "$code" = "200" ] || fail "second ensure returned HTTP $code: $(cat "$api_body")"
returned_sandbox2="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' "$api_body")"
[ "$returned_sandbox2" = "$sandbox" ] || fail "second ensure returned node id '$returned_sandbox2', want '$sandbox'"
log "second ensure confirmed the same node id $sandbox"

# 8. Delete the node and assert nothing labeled or secret survives.
code="$(api_call DELETE "/internal/v1/workspace-nodes/$node_id")"
[ "$code" = "204" ] || fail "delete returned HTTP $code: $(cat "$api_body")"

# node_absent reports whether every labeled container and the node network are
# gone. It is polled so the assertion does not race Docker's own propagation.
node_absent() {
  local labeled managed
  labeled="$(docker ps -a --filter "label=com.multica.aurora.node=$node_id" --format '{{.Names}}')"
  [ -z "$labeled" ] || return 1
  managed="$(docker ps -a --filter 'label=com.multica.aurora.managed' --format '{{.Names}}')"
  [ -z "$managed" ] || return 1
  for name in "$sandbox" "$proxy"; do
    if docker inspect "$name" >/dev/null 2>&1; then return 1; fi
  done
  if docker network inspect "$network" >/dev/null 2>&1; then return 1; fi
  return 0
}
absence_deadline=$(( $(date +%s) + 15 ))
until node_absent; do
  [ "$(date +%s)" -lt "$absence_deadline" ] || fail "delete left node resources: containers '$(docker ps -a --filter "label=com.multica.aurora.node=$node_id" --format '{{.Names}}')', workspace network '$network', all containers: $(docker ps -a --format '{{.Names}}' | tr '\n' ' ')"
  sleep 0.5
done
secret_left="$(find "$secret_root" -type f 2>/dev/null || true)"
[ -z "$secret_left" ] || fail "staged secret files remain: $secret_left"
if grep -qF "$raw_token" "$staging/fleet.log"; then fail "the fleet log contains the control token"; fi
if grep -qF "$enrollment_secret" "$staging/fleet.log"; then fail "the fleet log contains the enrollment secret"; fi
log "delete left no labeled container, network, or staged secret"

log "no real provider or agent was called; only local fake endpoints and Docker were used"
printf '%s\n' 'FUNCTIONAL SMOKE ONLY: AppArmor and Linux cgroup acceptance not evaluated on Docker Desktop'
