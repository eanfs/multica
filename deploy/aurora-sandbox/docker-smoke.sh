#!/usr/bin/env bash
# Aurora sandbox macOS Docker Desktop functional smoke.
#
# This script exercises the complete Plan B development path on Docker Desktop:
# the authenticated fleet control API (provision and enrollment issuance), the
# hardened Docker backend, and the enforced egress sidecar. It starts fake
# Multica control and fake provider endpoints, ensures one workspace node
# through the fleet API, polls until the node is online and healthy, confirms a
# repeat ensure returns the same node, runs the real release sandbox image
# through the containerized fake xhs-image pipeline (fake Seedream/import/
# manifest/report, real Node runtime), then deletes the node and asserts no
# labeled resource or staged secret survives.
#
# Docker Desktop's Linux virtual machine does not enforce the AppArmor profile
# or the Linux cgroup/pids/namespace semantics, so this is functional evidence
# only. It never substitutes for the Linux acceptance in
# deploy/aurora-sandbox/docker-security-test.sh, and it must not be read as
# satisfying Task 6, issue #29, or the master plan's Linux security gates. The
# report records security_acceptance_evaluated=false.
#
# The script never calls a real provider or agent: the only network targets are
# the local fake endpoints and the local Docker daemon.
#
# Image references are immutable. AURORA_SANDBOX_IMAGE and AURORA_PROXY_IMAGE
# name the fixture or release pair; a caller may pass a digest or
# content-addressed image ID, or a tag already present in the local image store
# that the script resolves to its repository digest or content-addressed ID.
# AURORA_PIPELINE_IMAGE optionally names the release sandbox image that carries
# the Node runtime for the fake xhs-image pipeline; it defaults to the sandbox
# image when that image has Node. A missing local image is an error: the script
# never pulls.
#
# Usage:
#   deploy/aurora-sandbox/docker-smoke.sh
#   AURORA_PIPELINE_IMAGE=ghcr.io/eanfs/multica-aurora-sandbox:ci \
#     deploy/aurora-sandbox/docker-smoke.sh
#
# The script writes a sanitized machine-readable report to
# .scratch/aurora-sandbox-acceptance/macos-smoke.json (override with
# AURORA_ACCEPTANCE_REPORT_DIR). It records the Docker client and server
# versions, kernel, architecture, cgroup mode, AppArmor status, and every image
# digest before any test runs.

set -euo pipefail

script_dir="$(cd "$(dirname "$BASH_SOURCE")" && pwd)"
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
pipeline_container=""

log() { printf 'docker-smoke: %s\n' "$*"; }
fail() { printf 'docker-smoke: ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Acceptance report scaffolding
# ---------------------------------------------------------------------------
acceptance_mode="macos-functional-smoke"
security_acceptance_evaluated=false
report_dir="${AURORA_ACCEPTANCE_REPORT_DIR:-$repo_root/.scratch/aurora-sandbox-acceptance}"
mkdir -p "$report_dir"
report_file="$report_dir/macos-smoke.json"
tests_file="$(mktemp)"
last_step="preflight"
test_count=0
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
started_epoch="$(date +%s)"
host_os="$(uname -s | tr 'A-Z' 'a-z')"
host_arch="$(uname -m)"
host_kernel="$(uname -r)"
docker_client_version=""
docker_server_version=""
cgroup_version=""
cgroup_driver=""
apparmor_status="not-evaluated"
sandbox_digest=""
egress_digest=""
pipeline_digest=""

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
  if [ -n "$pipeline_container" ]; then docker rm -f "$pipeline_container" >/dev/null 2>&1 || true; fi
  if [ -n "$sandbox" ]; then docker rm -f "$sandbox" >/dev/null 2>&1 || true; fi
  if [ -n "$proxy" ]; then docker rm -f "$proxy" >/dev/null 2>&1 || true; fi
  if [ -n "$network" ]; then docker network rm "$network" >/dev/null 2>&1 || true; fi
  if [ "$created_uplink" -eq 1 ]; then docker network rm aurora-egress-uplink >/dev/null 2>&1 || true; fi
  if [ -n "$staging" ]; then rm -rf "$staging"; fi
}

# sanitize strips URLs and credential-shaped tokens from any text that reaches
# the report. Reports must never carry a token, a provider URL, or a prompt.
sanitize() {
  printf '%s' "$1" \
    | sed -E 's#https?://[^[:space:]]+#[redacted-url]#g' \
    | sed -E 's#(mse_|mdt_|mtt_)[A-Za-z0-9._-]+#\1[redacted]#g' \
    | sed -E 's#sk-[A-Za-z0-9._-]+#sk-[redacted]#g' \
    | tr '\n' ' '
}

# ref_digest prints the immutable digest portion of a reference.
ref_digest() {
  case "$1" in
    *@*) printf '%s' "${1##*@}" ;;
    *) printf '%s' "$1" ;;
  esac
}

# is_digest_ref is true for sha256:<64 lowercase hex> or <name>@sha256:<64 hex>.
is_digest_ref() {
  case "$1" in
    sha256:*) printf '%s' "$1" | grep -Eq '^sha256:[0-9a-f]{64}$' ;;
    *@sha256:*) printf '%s' "$1" | grep -Eq '@sha256:[0-9a-f]{64}$' ;;
    *) return 1 ;;
  esac
}

# resolve_image proves an image is present locally and returns an immutable
# reference the local store can resolve: the supplied digest/ID unchanged, or,
# for a locally present tag, its repository digest or content-addressed image
# ID. It never pulls; an absent image is an error.
resolve_image() {
  local source="$1" label="$2" ref id
  [ -n "$source" ] || fail "$label must not be empty"
  if is_digest_ref "$source"; then
    docker image inspect "$source" >/dev/null 2>&1 \
      || fail "$label $source is not present locally; refusing to pull"
    printf '%s' "$source"
    return 0
  fi
  id="$(docker image inspect --format '{{.Id}}' "$source" 2>/dev/null)" \
    || fail "$label $source is not in the local image store; build it or pass an immutable digest or image ID (the script never pulls)"
  ref="$(docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$source" 2>/dev/null || true)"
  if [ -n "$ref" ] && is_digest_ref "$ref" && docker image inspect "$ref" >/dev/null 2>&1; then
    printf '%s' "$ref"
    return 0
  fi
  is_digest_ref "$id" || fail "$label $source did not resolve to a digest-pinned reference"
  docker image inspect "$id" >/dev/null 2>&1 \
    || fail "$label $source is not resolvable locally; refusing to pull"
  printf '%s' "$id"
}

# record_test appends one sanitized test result to the report test list.
record_test() {
  local name="$1" status="$2" reason="$3" seconds="$4"
  jq -nc \
    --arg name "$name" \
    --arg status "$status" \
    --arg reason "$(sanitize "$reason")" \
    --argjson seconds "$seconds" \
    '{name:$name,status:$status,reason:(if $reason=="" then null else $reason end),duration_seconds:$seconds}' \
    >>"$tests_file"
  test_count=$((test_count + 1))
}

# has_fail is true when a recorded test already carries a failure.
has_fail() {
  jq -s 'any(.[]; .status=="fail")' "$tests_file" 2>/dev/null | grep -q true
}

write_report() {
  local result="$1" tests_json counts finished_at finished_epoch duration
  tests_json="$(jq -s '.' "$tests_file" 2>/dev/null || printf '[]')"
  counts="$(printf '%s' "$tests_json" | jq -c '{pass:([.[]|select(.status=="pass")]|length),fail:([.[]|select(.status=="fail")]|length),skip:([.[]|select(.status=="skip")]|length),total:length}')"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  finished_epoch="$(date +%s)"
  duration=$((finished_epoch - started_epoch))
  jq -n \
    --arg result "$result" \
    --arg schema "com.multica.aurora.sandbox-acceptance" \
    --arg mode "$acceptance_mode" \
    --argjson evaluated "$security_acceptance_evaluated" \
    --arg os "$host_os" \
    --arg arch "$host_arch" \
    --arg kernel "$host_kernel" \
    --arg docker_client "$docker_client_version" \
    --arg docker_server "$docker_server_version" \
    --arg cgroup_version "$cgroup_version" \
    --arg cgroup_driver "$cgroup_driver" \
    --arg apparmor "$apparmor_status" \
    --arg sandbox_digest "$sandbox_digest" \
    --arg egress_digest "$egress_digest" \
    --arg pipeline_digest "$pipeline_digest" \
    --arg started "$started_at" \
    --arg finished "$finished_at" \
    --argjson duration "$duration" \
    --argjson tests "$tests_json" \
    --argjson counts "$counts" \
    '{schema:$schema,version:1,mode:$mode,security_acceptance_evaluated:$evaluated,
      platform:{os:$os,arch:$arch,kernel:$kernel,docker_client:$docker_client,
        docker_server:$docker_server,cgroup_version:$cgroup_version,
        cgroup_driver:$cgroup_driver,apparmor:$apparmor},
      images:{sandbox:$sandbox_digest,egress:$egress_digest,pipeline:$pipeline_digest},
      started_at:$started,finished_at:$finished,duration_seconds:$duration,
      tests:$tests,counts:$counts,result:$result}' >"$report_file"
  printf 'docker-smoke: wrote %s\n' "$report_file" >&2
}

on_exit() {
  local code=$?
  trap - EXIT INT TERM
  local result="fail" reason=""
  if [ "$code" -eq 0 ]; then
    result="pass"
  else
    reason="step failed: $last_step"
  fi
  if [ "$test_count" -eq 0 ]; then
    record_test "acceptance-preflight" "$result" "$reason" 0
  elif [ "$result" = "fail" ] && ! has_fail; then
    record_test "acceptance-failure" "fail" "$reason" 0
  fi
  write_report "$result"
  rm -f "$tests_file"
  cleanup
  exit "$code"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

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

# ---------------------------------------------------------------------------
# Preflight: platform and Docker inventory before any test runs
# ---------------------------------------------------------------------------
last_step="verify prerequisites"
for tool in docker go curl openssl nc uuidgen jq; do
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
docker_client_version="$(docker version --format '{{.Client.Version}}' 2>/dev/null || printf 'unknown')"
docker_server_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf 'unknown')"
cgroup_version="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || printf 'unknown')"
cgroup_driver="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || printf 'unknown')"
apparmor_status="not-evaluated-on-docker-desktop"
log "Docker Desktop $docker_os ($docker_arch); docker client $docker_client_version server $docker_server_version; cgroup v$cgroup_version/$cgroup_driver"
log "preflight os=$host_os arch=$host_arch kernel=$host_kernel; AppArmor/cgroup security acceptance is NOT evaluated on Docker Desktop"

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

# 2. Fixture or release image pair. Reuse the operator's immutable images or
# build the local fixture pair for the Docker Desktop architecture.
last_step="resolve the sandbox image pair"
sandbox_image="${AURORA_SANDBOX_IMAGE:-}"
proxy_image="${AURORA_PROXY_IMAGE:-}"
if [ -n "$sandbox_image" ] || [ -n "$proxy_image" ]; then
  [ -n "$sandbox_image" ] && [ -n "$proxy_image" ] || fail "set both AURORA_SANDBOX_IMAGE and AURORA_PROXY_IMAGE, or neither"
  sandbox_image="$(resolve_image "$sandbox_image" AURORA_SANDBOX_IMAGE)"
  proxy_image="$(resolve_image "$proxy_image" AURORA_PROXY_IMAGE)"
else
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$staging/aurora-sandbox-probe" ./cmd/aurora-sandbox-probe )
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -o "$staging/aurora-egress-proxy" ./cmd/aurora-egress-proxy )
  docker build -q -f "$script_dir/fixture/Dockerfile.sandbox" -t "multica-aurora-sandbox-smoke:local" "$staging" >/dev/null
  docker build -q -f "$script_dir/fixture/Dockerfile.egress" -t "multica-aurora-egress-smoke:local" "$staging" >/dev/null
  sandbox_image="$(resolve_image "multica-aurora-sandbox-smoke:local" fixture-sandbox)"
  proxy_image="$(resolve_image "multica-aurora-egress-smoke:local" fixture-egress)"
fi
sandbox_digest="$(ref_digest "$sandbox_image")"
egress_digest="$(ref_digest "$proxy_image")"

# The fake xhs-image pipeline needs the release image's Node runtime. Name it
# with AURORA_PIPELINE_IMAGE, or fall back to the sandbox image when it carries
# Node (the release image does, the fixture does not).
pipeline_image_ref=""
if [ -n "${AURORA_PIPELINE_IMAGE:-}" ]; then
  pipeline_image_ref="$(resolve_image "$AURORA_PIPELINE_IMAGE" AURORA_PIPELINE_IMAGE)"
elif docker run --rm --pull never --entrypoint /usr/local/bin/node "$sandbox_image" --version >/dev/null 2>&1; then
  pipeline_image_ref="$sandbox_image"
fi
if [ -n "$pipeline_image_ref" ]; then
  pipeline_digest="$(ref_digest "$pipeline_image_ref")"
fi

seccomp_profile="$repo_root/deploy/aurora-sandbox/seccomp.json"
[ -f "$seccomp_profile" ] || fail "missing seccomp profile: $seccomp_profile"
log "preflight image digests sandbox=$sandbox_digest egress=$egress_digest pipeline=$pipeline_digest"

preflight_start="$(date +%s)"
record_test "docker-desktop-preflight" "pass" "" "$(( $(date +%s) - preflight_start ))"

# 3. Fake Multica control origin and fake provider endpoint. They bind ephemeral
# host ports and report them through a ready file; the sandbox reaches them only
# through the egress sidecar as host.docker.internal.
last_step="start fake control and provider endpoints"
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
last_step="start the fleet controller"
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
  local method="$1" path="$2" data="$3"
  local args=(-sS -o "$api_body" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $raw_token")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  curl "${args[@]}" "$fleet_url$path" || true
}

# 5. Ensure one workspace node through the real authenticated API.
last_step="provision a workspace node through the fleet API"
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
provision_start="$(date +%s)"
code="$(api_call PUT "/internal/v1/workspace-nodes/$node_id" "$ensure_body")"
[ "$code" = "200" ] || fail "ensure returned HTTP $code: $(cat "$api_body")"
returned_sandbox="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' "$api_body")"
[ "$returned_sandbox" = "$sandbox" ] || fail "ensure returned node id '$returned_sandbox', want policy-derived '$sandbox'"
record_test "fleet-provision-enrollment" "pass" "" "$(( $(date +%s) - provision_start ))"
log "ensured workspace node $node_id (sandbox $sandbox)"

# 6. Wait for online/healthy state by polling the fleet API with a 90s deadline.
last_step="wait for the enrolled node to become online and idle"
online_start="$(date +%s)"
poll_deadline=$(( $(date +%s) + 90 ))
state=""
health=""
while :; do
  code="$(api_call GET "/internal/v1/workspace-nodes/$node_id" "")"
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
record_test "node-online-idle" "pass" "" "$(( $(date +%s) - online_start ))"
log "node $node_id is online and healthy (idle)"

# The sandbox reaches the fake Multica origin only through the egress sidecar,
# and the fake provider endpoint stays unreachable through the same policy.
last_step="verify the egress boundary"
origin_url="$origin/aurora-smoke"
provider_url="http://host.docker.internal:$provider_port/"
probe() { docker exec "$sandbox" /opt/aurora/bin/aurora-sandbox-probe "$@"; }
egress_start="$(date +%s)"
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
record_test "egress-allow-multica-deny-provider" "pass" "" "$(( $(date +%s) - egress_start ))"
log "egress allows the exact fake Multica origin and refuses the fake provider endpoint"

# 7. A second ensure must confirm the same running node.
last_step="confirm a second ensure is idempotent"
idempotent_start="$(date +%s)"
enrollment_secret2="mse_$(openssl rand -hex 20)"
ensure_body2="$(printf '{"node_id":"%s","workspace_id":"%s","runtime_id":"%s","daemon_id":"%s","enrollment_token":"%s"}' \
  "$node_id" "$workspace_id" "$runtime_id" "$daemon_id" "$enrollment_secret2")"
code="$(api_call PUT "/internal/v1/workspace-nodes/$node_id" "$ensure_body2")"
[ "$code" = "200" ] || fail "second ensure returned HTTP $code: $(cat "$api_body")"
returned_sandbox2="$(sed -n 's/^{"id":"\([^"]*\)".*/\1/p' "$api_body")"
[ "$returned_sandbox2" = "$sandbox" ] || fail "second ensure returned node id '$returned_sandbox2', want '$sandbox'"
record_test "second-ensure-idempotent" "pass" "" "$(( $(date +%s) - idempotent_start ))"
log "second ensure confirmed the same node id $sandbox"

# 7b. Run the real release sandbox image through the containerized fake
# xhs-image pipeline (fake Seedream/import/manifest/report, real Node runtime).
# This is the fake xhs-image and artifact evidence on Docker Desktop; the full
# control-plane claim/complete path for a real daemon needs a live Multica
# server and is covered by the Linux/DB managed lifecycle test instead.
last_step="run the fake xhs-image pipeline in the release image"
if [ -z "$pipeline_image_ref" ]; then
  record_test "fake-xhs-image-artifact" "skip" "no release image with the Node runtime; set AURORA_PIPELINE_IMAGE" 0
  log "SKIP fake xhs-image pipeline: set AURORA_PIPELINE_IMAGE to a release sandbox image"
else
  pipeline_start="$(date +%s)"
  pipeline_container="aurora-smoke-pipelines-$hash"
  pipeline_log="$staging/pipelines.log"
  docker run --rm --pull never --name "$pipeline_container" \
    --network none --user 10001:10001 --read-only \
    --tmpfs /tmp:rw,size=4g,mode=1777 \
    --tmpfs /workspace:rw,size=4g,uid=10001,gid=10001,mode=0700 \
    --shm-size 512m --cap-drop ALL --security-opt no-new-privileges \
    --pids-limit 256 --memory 4g --cpus 2 -e HOME=/tmp \
    -v "$repo_root/deploy/aurora-sandbox/fixtures:/opt/aurora/smoke:ro" \
    --entrypoint /usr/local/bin/node "$pipeline_image_ref" \
    /opt/aurora/smoke/smoke/aurora-fake-pipelines.mjs >"$pipeline_log" 2>&1 \
    || fail "the fake xhs-image pipeline failed: $(tail -n 3 "$pipeline_log")"
  pipeline_container=""
  grep -q 'AURORA_SMOKE_RESULT' "$pipeline_log" || fail "the fake xhs-image pipeline printed no result marker: $(tail -n 3 "$pipeline_log")"
  grep -q '"xhs-image"' "$pipeline_log" || fail "the fake xhs-image pipeline did not report xhs-image: $(tail -n 3 "$pipeline_log")"
  record_test "fake-xhs-image-artifact" "pass" "" "$(( $(date +%s) - pipeline_start ))"
  log "fake xhs-image pipeline produced its artifact: $(grep 'AURORA_SMOKE_RESULT' "$pipeline_log" | tail -n 1)"
fi

# 8. Delete the node and assert nothing labeled or secret survives.
last_step="delete the node and confirm no resource remains"
delete_start="$(date +%s)"
code="$(api_call DELETE "/internal/v1/workspace-nodes/$node_id" "")"
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
  [ "$(date +%s)" -lt "$absence_deadline" ] || fail "delete left node resources: containers '$(docker ps -a --filter "label=com.multica.aurora.node=$node_id" --format '{{.Names}}')', workspace network '$network'"
  sleep 0.5
done
record_test "node-delete-absent" "pass" "" "$(( $(date +%s) - delete_start ))"

secret_left="$(find "$secret_root" -type f 2>/dev/null || true)"
[ -z "$secret_left" ] || fail "staged secret files remain: $secret_left"
if grep -qF "$raw_token" "$staging/fleet.log"; then fail "the fleet log contains the control token"; fi
if grep -qF "$enrollment_secret" "$staging/fleet.log"; then fail "the fleet log contains the enrollment secret"; fi
record_test "no-staged-secret-leak" "pass" "" 0
log "delete left no labeled container, network, or staged secret"

log "no real provider or agent was called; only local fake endpoints and Docker were used"
printf '%s\n' 'FUNCTIONAL SMOKE ONLY: AppArmor and Linux cgroup acceptance not evaluated on Docker Desktop'
printf '%s\n' 'SECURITY ACCEPTANCE NOT EVALUATED: this macOS Docker Desktop smoke does not evaluate AppArmor or cgroup isolation; the Linux acceptance matrix runs in CI.'
