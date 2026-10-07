#!/usr/bin/env bash
# Aurora sandbox macOS Docker Desktop functional smoke.
#
# This script exercises the remaining development path on Docker Desktop: it
# runs the real release sandbox image through the containerized fake xhs-image
# pipeline (fake Seedream/import/manifest/report, real Node runtime).
#
# The former authenticated fleet-controller flow (provision and enrollment
# issuance, egress-boundary probe, idempotent ensure, node delete) was removed
# when the external controller was retired: the repository's old controller
# package and command are gone, so there is no controller binary to build or
# start. The Fleet-based node lifecycle is exercised by the managed Linux/DB
# tests (Task 2 and Task 7), not by this macOS smoke.
#
# Docker Desktop's Linux virtual machine does not enforce the AppArmor profile
# or the Linux cgroup/pids/namespace semantics, so this is functional evidence
# only. It never substitutes for the Linux acceptance in
# deploy/aurora-sandbox/docker-security-test.sh, and it must not be read as
# satisfying Task 6, issue #29, or the master plan's Linux security gates. The
# report records security_acceptance_evaluated=false.
#
# The script never calls a real provider or agent: the fake pipeline starts its
# own loopback fake endpoints inside a --network none container, so the only
# reachable targets are loopback and the local Docker daemon.
#
# Image references are immutable. AURORA_PIPELINE_IMAGE names the release
# sandbox image that carries the Node runtime; AURORA_SANDBOX_IMAGE is accepted
# as a fallback when that image carries Node. A caller may pass a digest or
# content-addressed image ID, or a tag already present in the local image store
# that the script resolves to its repository digest or content-addressed ID. A
# missing local image is an error: the script never pulls.
#
# Usage:
#   AURORA_PIPELINE_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:<index-digest>' \
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

staging=""
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

# cleanup removes the container and every temporary file this script created,
# including on failure.
cleanup() {
  if [ -n "$pipeline_container" ]; then docker rm -f "$pipeline_container" >/dev/null 2>&1 || true; fi
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

# ---------------------------------------------------------------------------
# Preflight: platform and Docker inventory before any test runs
# ---------------------------------------------------------------------------
last_step="verify prerequisites"
for tool in docker jq; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

docker info >/dev/null 2>&1 || fail "Docker Desktop is not reachable"
docker_os="$(docker info --format '{{.OperatingSystem}}')"
case "$docker_os" in
  *"Docker Desktop"*) ;;
  *) fail "this smoke requires Docker Desktop (docker reports '$docker_os'); use deploy/aurora-sandbox/docker-security-test.sh on Linux" ;;
esac

docker_client_version="$(docker version --format '{{.Client.Version}}' 2>/dev/null || printf 'unknown')"
docker_server_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf 'unknown')"
cgroup_version="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || printf 'unknown')"
cgroup_driver="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || printf 'unknown')"
apparmor_status="not-evaluated-on-docker-desktop"
log "Docker Desktop $docker_os; docker client $docker_client_version server $docker_server_version; cgroup v$cgroup_version/$cgroup_driver"
log "preflight os=$host_os arch=$host_arch kernel=$host_kernel; AppArmor/cgroup security acceptance is NOT evaluated on Docker Desktop"

staging="$(cd "$(mktemp -d)" && pwd -P)"

# 1. Resolve the release sandbox image that carries the Node runtime for the
# containerized fake xhs-image pipeline. AURORA_PIPELINE_IMAGE takes precedence;
# AURORA_SANDBOX_IMAGE is the fallback. The script never pulls.
last_step="resolve the release sandbox image"
pipeline_image_source="${AURORA_PIPELINE_IMAGE:-${AURORA_SANDBOX_IMAGE:-}}"
pipeline_image_ref=""
if [ -n "$pipeline_image_source" ]; then
  resolved_pipeline="$(resolve_image "$pipeline_image_source" AURORA_PIPELINE_IMAGE)"
  if docker run --rm --pull never --entrypoint /usr/local/bin/node "$resolved_pipeline" --version >/dev/null 2>&1; then
    pipeline_image_ref="$resolved_pipeline"
    sandbox_digest="$(ref_digest "$pipeline_image_ref")"
    pipeline_digest="$sandbox_digest"
  else
    log "image $resolved_pipeline has no /usr/local/bin/node runtime"
  fi
fi

preflight_start="$(date +%s)"
record_test "docker-desktop-preflight" "pass" "" "$(( $(date +%s) - preflight_start ))"
log "preflight image digest pipeline=${pipeline_digest:-<none>}"

# 2. Run the real release sandbox image through the containerized fake
# xhs-image pipeline (fake Seedream/import/manifest/report, real Node runtime).
# This is the remaining functional evidence on Docker Desktop; the full
# control-plane claim/complete path for a real daemon needs a live Multica
# server and is covered by the managed Linux/DB tests instead.
last_step="run the fake xhs-image pipeline in the release image"
if [ -z "$pipeline_image_ref" ]; then
  record_test "fake-xhs-image-artifact" "skip" "no release image with the Node runtime; set AURORA_PIPELINE_IMAGE" 0
  log "SKIP fake xhs-image pipeline: set AURORA_PIPELINE_IMAGE to a release sandbox image"
else
  pipeline_start="$(date +%s)"
  pipeline_container="aurora-smoke-pipelines-$$"
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

log "no real provider or agent was called; only local fake endpoints and Docker were used"
printf '%s\n' 'FUNCTIONAL SMOKE ONLY: AppArmor and Linux cgroup acceptance not evaluated on Docker Desktop'
printf '%s\n' 'SECURITY ACCEPTANCE NOT EVALUATED: this macOS Docker Desktop smoke does not evaluate AppArmor or cgroup isolation; the Linux acceptance matrix runs in CI.'
