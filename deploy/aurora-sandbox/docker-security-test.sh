#!/usr/bin/env bash
# Aurora sandbox Linux Docker security acceptance entry point.
#
# This script runs the Linux Docker Engine acceptance preflight: it loads the
# AppArmor profile, builds the fixture sandbox and egress images, and resolves
# every image to an immutable reference. The auroradocker-tagged isolation/egress
# boundary and the containerized fake pipeline smoke were deleted with the retired
# controller, so the matrix is not run today: the script records it as an explicit
# skip and reports the acceptance as not evaluated until Task 2 restores the
# Fleet-based suite. Docker Desktop on macOS cannot satisfy the AppArmor, cgroup,
# or kernel gates, so this script fails loudly there.
#
# Image references are immutable. The release sandbox image that carries the
# Node runtime for the fake pipeline smoke is named by AURORA_PIPELINE_IMAGE;
# prebuilt digest-pinned fixture images can be reused with
# AURORA_FIXTURE_SANDBOX_REF and AURORA_FIXTURE_PROXY_REF. A caller may pass a
# digest or content-addressed image ID, or a tag that is already present in the
# local image store and that this script resolves to its repository digest or
# content-addressed image ID. A missing local image is an error: the script
# never pulls, and every container run uses --pull never.
#
# Usage:
#   AURORA_PIPELINE_IMAGE='sha256:<64 hex>' \
#     deploy/aurora-sandbox/docker-security-test.sh
#   AURORA_DOCKER_SECURITY_COUNT=2 deploy/aurora-sandbox/docker-security-test.sh
#
# The script writes a sanitized machine-readable report to
# .scratch/aurora-sandbox-acceptance/linux-acceptance.json (override with
# AURORA_ACCEPTANCE_REPORT_DIR). It records the Docker client and server
# versions, kernel, architecture, cgroup mode, AppArmor status, and every image
# digest before any test runs.

set -euo pipefail

script_dir="$(cd "$(dirname "$BASH_SOURCE")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
server_dir="$repo_root/server"

fail() {
  echo "docker-security-test: $*" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# Acceptance report scaffolding
# ---------------------------------------------------------------------------
acceptance_mode="linux-security-acceptance"
security_acceptance_evaluated=true
linux_matrix_skipped=false
report_dir="${AURORA_ACCEPTANCE_REPORT_DIR:-$repo_root/.scratch/aurora-sandbox-acceptance}"
mkdir -p "$report_dir"
report_file="$report_dir/linux-acceptance.json"
tests_file="$(mktemp)"
security_log=""
staging=""
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

cleanup() {
  if [ -n "$staging" ] && [ -d "$staging" ]; then
    rm -rf "$staging"
  fi
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

# parse_security_tests turns go test -v result lines into report entries. The
# most recent indented log line is kept as the per-test skip or failure reason.
parse_security_tests() {
  local log="$1" tsv
  tsv="$(mktemp)"
  awk '
    function flush() {
      if (name == "") return
      printf "%s\t%s\t%s\t%s\n", name, status, seconds, reason
      name = ""
    }
    /^[[:space:]]*--- (PASS|FAIL|SKIP): / {
      flush()
      line = $0
      sub(/^[[:space:]]*--- /, "", line)
      n = split(line, parts, " ")
      status = tolower(parts[1])
      sub(/:$/, "", status)
      name = parts[2]
      seconds = parts[3]
      gsub(/[()s]/, "", seconds)
      reason = ""
      next
    }
    /^[[:space:]]+[^[:space:]]/ {
      reason = $0
      sub(/^[[:space:]]+/, "", reason)
      next
    }
    END { flush() }
  ' "$log" >"$tsv"
  while IFS=$'\t' read -r name status seconds reason; do
    [ -n "$name" ] || continue
    [ -n "$seconds" ] || seconds=0
    record_test "$name" "$status" "$reason" "$seconds"
  done <"$tsv"
  rm -f "$tsv"
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
  printf 'docker-security-test: wrote %s\n' "$report_file" >&2
}

on_exit() {
  local code=$?
  trap - EXIT
  local result="fail" reason=""
  if [ "$code" -eq 0 ]; then
    result="pass"
  else
    reason="step failed: $last_step"
  fi
  # A skipped matrix is never recorded as a pass. Keep the explicit per-test skip
  # entry, but report the acceptance as not evaluated with a non-pass result. The
  # exit code stays 0 until Task 2 restores the matrix.
  if [ "$code" -eq 0 ] && [ "$linux_matrix_skipped" = true ]; then
    result="skipped"
  fi
  if [ -n "$security_log" ] && [ -f "$security_log" ]; then
    parse_security_tests "$security_log"
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

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
last_step="verify Linux and Docker prerequisites"
[ "$(uname -s)" = "Linux" ] || fail "this acceptance runs only on Linux Docker Engine; on $(uname -s) use the functional smoke instead"

command -v docker >/dev/null 2>&1 || fail "docker is required"
command -v jq >/dev/null 2>&1 || fail "jq is required to write the acceptance report"
docker info >/dev/null 2>&1 || fail "the Docker daemon is not reachable"

docker_client_version="$(docker version --format '{{.Client.Version}}' 2>/dev/null || printf 'unknown')"
docker_server_version="$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf 'unknown')"
cgroup_version="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || printf 'unknown')"
cgroup_driver="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || printf 'unknown')"

seccomp_profile="$repo_root/deploy/aurora-sandbox/seccomp.json"
apparmor_profile="$repo_root/deploy/aurora-sandbox/multica-aurora-sandbox.apparmor"
[ -f "$seccomp_profile" ] || fail "missing seccomp profile: $seccomp_profile"
[ -f "$apparmor_profile" ] || fail "missing AppArmor profile: $apparmor_profile"

# AppArmor is a mandatory part of the Linux acceptance boundary. Its status is
# recorded before any test runs, including when it is what blocks the run.
if [ ! -e /sys/module/apparmor ]; then
  apparmor_status="unavailable"
  fail "the kernel does not expose AppArmor; the acceptance requires it"
elif [ -r /sys/module/apparmor/parameters/enabled ] && [ "$(cat /sys/module/apparmor/parameters/enabled)" != "Y" ]; then
  apparmor_status="disabled"
  fail "AppArmor is present but disabled in the kernel"
else
  apparmor_status="enabled"
fi
command -v apparmor_parser >/dev/null 2>&1 || fail "apparmor_parser is required to load multica-aurora-sandbox"
if [ "$(id -u)" -eq 0 ]; then
  apparmor_parser -r "$apparmor_profile" || fail "failed to load the AppArmor profile"
elif command -v sudo >/dev/null 2>&1; then
  sudo apparmor_parser -r "$apparmor_profile" || fail "failed to load the AppArmor profile"
else
  fail "loading the AppArmor profile needs root; rerun as root"
fi

go_bin="${GO:-go}"
command -v "$go_bin" >/dev/null 2>&1 || fail "the Go toolchain is required"

docker_arch="$(docker info --format '{{.Architecture}}')"
case "$docker_arch" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) fail "unsupported Docker architecture $docker_arch" ;;
esac

staging="$(mktemp -d)"

# ---------------------------------------------------------------------------
# Resolve every image to an immutable reference before any test runs
# ---------------------------------------------------------------------------
last_step="resolve the release sandbox image"
pipeline_image_source="${AURORA_PIPELINE_IMAGE:-${AURORA_PIPELINE_IMAGE_TAG:-multica-aurora-sandbox:local}}"
[ -n "$pipeline_image_source" ] || fail "AURORA_PIPELINE_IMAGE must name the release sandbox image used by the fake pipeline smoke"
pipeline_image_ref="$(resolve_image "$pipeline_image_source" AURORA_PIPELINE_IMAGE)"
pipeline_digest="$(ref_digest "$pipeline_image_ref")"

fixture_sandbox_ref="${AURORA_FIXTURE_SANDBOX_REF:-}"
fixture_proxy_ref="${AURORA_FIXTURE_PROXY_REF:-}"
if [ -n "$fixture_sandbox_ref" ] || [ -n "$fixture_proxy_ref" ]; then
  [ -n "$fixture_sandbox_ref" ] && [ -n "$fixture_proxy_ref" ] || fail "set both AURORA_FIXTURE_SANDBOX_REF and AURORA_FIXTURE_PROXY_REF, or neither"
  fixture_sandbox_ref="$(resolve_image "$fixture_sandbox_ref" AURORA_FIXTURE_SANDBOX_REF)"
  fixture_proxy_ref="$(resolve_image "$fixture_proxy_ref" AURORA_FIXTURE_PROXY_REF)"
fi

if [ -z "$fixture_sandbox_ref" ]; then
  last_step="build the fixture image pair"
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$go_bin" build -trimpath -o "$staging/aurora-sandbox-probe" ./cmd/aurora-sandbox-probe ) || fail "failed to build the probe binary"
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$go_bin" build -trimpath -o "$staging/aurora-egress-proxy" ./cmd/aurora-egress-proxy ) || fail "failed to build the egress proxy binary"
  docker build -f "$repo_root/deploy/aurora-sandbox/fixture/Dockerfile.sandbox" -t "${AURORA_FIXTURE_SANDBOX_TAG:-multica-aurora-sandbox-fixture:local}" "$staging" || fail "failed to build the fixture sandbox image"
  docker build -f "$repo_root/deploy/aurora-sandbox/fixture/Dockerfile.egress" -t "${AURORA_FIXTURE_PROXY_TAG:-multica-aurora-egress-fixture:local}" "$staging" || fail "failed to build the fixture egress image"
  fixture_sandbox_ref="$(resolve_image "${AURORA_FIXTURE_SANDBOX_TAG:-multica-aurora-sandbox-fixture:local}" AURORA_FIXTURE_SANDBOX_TAG)"
  fixture_proxy_ref="$(resolve_image "${AURORA_FIXTURE_PROXY_TAG:-multica-aurora-egress-fixture:local}" AURORA_FIXTURE_PROXY_TAG)"
fi
sandbox_digest="$(ref_digest "$fixture_sandbox_ref")"
egress_digest="$(ref_digest "$fixture_proxy_ref")"

# The preflight inventory is printed and recorded before the acceptance tests.
printf 'docker-security-test: preflight os=%s arch=%s kernel=%s docker-client=%s docker-server=%s cgroup=v%s/%s apparmor=%s\n' \
  "$host_os" "$host_arch" "$host_kernel" "$docker_client_version" "$docker_server_version" "$cgroup_version" "$cgroup_driver" "$apparmor_status"
printf 'docker-security-test: image digests sandbox=%s egress=%s pipeline=%s\n' \
  "$sandbox_digest" "$egress_digest" "$pipeline_digest"
printf 'AURORA_SANDBOX_IMAGE=%s\n' "$fixture_sandbox_ref"
printf 'AURORA_PIPELINE_IMAGE=%s\n' "$pipeline_image_ref"
printf 'AURORA_PROXY_IMAGE=%s\n' "$fixture_proxy_ref"
printf 'AURORA_SECCOMP_PROFILE=%s\n' "$seccomp_profile"

export AURORA_RUN_DOCKER_SECURITY_TEST=1
export AURORA_SANDBOX_IMAGE="$fixture_sandbox_ref"
export AURORA_PIPELINE_IMAGE="$pipeline_image_ref"
export AURORA_PROXY_IMAGE="$fixture_proxy_ref"
export AURORA_SECCOMP_PROFILE="$seccomp_profile"
export AURORA_APPARMOR_PROFILE="multica-aurora-sandbox"

# ---------------------------------------------------------------------------
# Acceptance matrix
# ---------------------------------------------------------------------------
# The auroradocker-tagged Linux suite ran here. It was deleted with the retired
# external controller package (TestDockerSandboxLinuxSecurityBoundary and
# TestDockerSandboxFakeAuroraPipelines); Task 2 delivers the Fleet-based
# replacement. The container-level preflight above (immutable image resolution,
# fixture image pair, AppArmor profile load) still runs, and the matrix is
# recorded as a skip rather than a pass so the report never claims a security
# acceptance that was not evaluated.
last_step="record the removed Linux acceptance matrix"
linux_matrix_skipped=true
security_acceptance_evaluated=false
record_test "auroradocker-acceptance-matrix" "skip" \
  "the auroradocker suite was deleted with the retired controller; the Fleet-based Linux acceptance is Task 2" 0
printf 'docker-security-test: SKIP the auroradocker matrix: the tagged suite was deleted with the retired controller; Task 2 restores the Fleet-based suite\n' >&2
printf '::warning::Aurora Linux isolation/egress acceptance was NOT evaluated: the auroradocker suite was deleted with the retired controller; Task 2 restores the Fleet-based Linux acceptance matrix\n' >&2
exit 0
