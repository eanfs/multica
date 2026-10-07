#!/usr/bin/env bash
# Fleet sandbox containerized Linux security acceptance.
#
# This is the Task 2 replacement for the retired auroradocker-tagged Go suite
# that lived in the deleted server/internal/aurorafleet package. It is the
# "equivalent containerized acceptance" the task allows: it exercises the
# Fleet-managed sandbox contract on a Linux Docker Engine host without adding
# code under server/**.
#
# deploy/aurora-sandbox/docker-security-test.sh supplies the release sandbox
# image, the scratch probe and egress fixtures, the fake-CLI Fleet fixture, the
# seccomp and AppArmor profiles, and AURORA_DOCKER_SECURITY_COUNT (the plan's
# two-pass requirement).
#
# It emits "--- PASS|FAIL|SKIP: <name> (<seconds>s)" lines that the caller parses
# into the sanitized acceptance report. Exit codes:
#   0  every selected test passed
#   1  at least one test failed
#   3  the environment cannot evaluate the matrix (the caller records an honest
#      skip, never a pass)
#
# The gate is the first action: nothing touches Docker before it.

set -uo pipefail

gate="${AURORA_RUN_DOCKER_SECURITY_TEST:-}"
if [ "$gate" != "1" ]; then
  printf -- '--- SKIP: TestFleetSandboxAcceptance (0.00s)\n'
  printf '    set AURORA_RUN_DOCKER_SECURITY_TEST=1 to run the Linux acceptance\n'
  exit 3
fi
if [ "$(uname -s)" != "Linux" ]; then
  printf -- '--- SKIP: TestFleetSandboxAcceptance (0.00s)\n'
  printf '    the Fleet sandbox acceptance requires a Linux kernel (got %s)\n' "$(uname -s)"
  exit 3
fi

for tool in docker jq; do
  command -v "$tool" >/dev/null 2>&1 || { printf 'fleet-sandbox-acceptance: %s is required\n' "$tool" >&2; exit 1; }
done

script_dir="$(cd "$(dirname "$BASH_SOURCE")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

release_image="${AURORA_PIPELINE_IMAGE:-}"
probe_image="${AURORA_SANDBOX_IMAGE:-}"
proxy_image="${AURORA_PROXY_IMAGE:-}"
fleet_fixture="${AURORA_FLEET_FIXTURE_REF:-}"
seccomp_profile="${AURORA_SECCOMP_PROFILE:-}"
apparmor_profile="${AURORA_APPARMOR_PROFILE:-multica-aurora-sandbox}"
count="${AURORA_DOCKER_SECURITY_COUNT:-1}"

require_input() {
  # require_input <name> <value>
  [ -n "$2" ] || { printf 'fleet-sandbox-acceptance: %s is required\n' "$1" >&2; exit 1; }
}
require_input AURORA_PIPELINE_IMAGE "$release_image"
require_input AURORA_SANDBOX_IMAGE "$probe_image"
require_input AURORA_PROXY_IMAGE "$proxy_image"
require_input AURORA_FLEET_FIXTURE_REF "$fleet_fixture"
require_input AURORA_SECCOMP_PROFILE "$seccomp_profile"
[ -f "$seccomp_profile" ] || { printf 'fleet-sandbox-acceptance: seccomp profile %s is missing\n' "$seccomp_profile" >&2; exit 1; }
case "$count" in
  ''|*[!0-9]*) printf 'fleet-sandbox-acceptance: AURORA_DOCKER_SECURITY_COUNT must be a positive integer\n' >&2; exit 1 ;;
esac
[ "$count" -ge 1 ] || { printf 'fleet-sandbox-acceptance: AURORA_DOCKER_SECURITY_COUNT must be a positive integer\n' >&2; exit 1; }

fixtures_dir="$repo_root/deploy/aurora-sandbox/fixtures"
harness="$fixtures_dir/smoke/aurora-fake-pipelines.mjs"

work="$(mktemp -d)"
pass_count=0
fail_count=0
skip_count=0
FAILURES=""
TEST_REASON=""

cleanup() { rm -rf "$work"; }
trap cleanup EXIT

emit_pass() { printf -- '--- PASS: %s (%s.00s)\n' "$1" "$2"; pass_count=$((pass_count + 1)); }
emit_fail() { printf -- '--- FAIL: %s (%s.00s)\n' "$1" "$2"; printf '    %s\n' "$3"; fail_count=$((fail_count + 1)); }
emit_skip() { printf -- '--- SKIP: %s (0.00s)\n' "$1"; printf '    %s\n' "$2"; skip_count=$((skip_count + 1)); }

start_test() { FAILURES=""; }
note_fail() {
  if [ -z "$FAILURES" ]; then
    FAILURES="$1"
  else
    FAILURES="$FAILURES; $1"
  fi
}
finish_test() { TEST_REASON="$FAILURES"; [ -z "$FAILURES" ]; }

run_test() {
  # run_test <public-name> <function>
  local name="$1" fn="$2" start end rc
  start="$(date +%s)"
  rc=0
  "$fn" || rc=$?
  end="$(date +%s)"
  if [ "$rc" -eq 0 ]; then
    emit_pass "$name" "$((end - start))"
  else
    emit_fail "$name" "$((end - start))" "${TEST_REASON:-failed}"
  fi
}

# probe executes the fixed probe binary inside the sandbox container. It records
# the JSON result and docker's exit code without aborting on a seccomp kill.
probe() {
  PROBE_JSON="$(docker exec "$sandbox_name" /opt/aurora/bin/aurora-sandbox-probe "$@" 2>/dev/null)"
  PROBE_RC=$?
}
require_allowed() {
  local label="$1"; shift
  probe "$@"
  if [ -n "$PROBE_JSON" ] && [ "$(printf '%s' "$PROBE_JSON" | jq -r '.allowed' 2>/dev/null)" = "true" ]; then return 0; fi
  note_fail "$label: expected the operation to be allowed (rc=$PROBE_RC out=$PROBE_JSON)"
  return 1
}
require_blocked() {
  local label="$1"; shift
  probe "$@"
  if [ -z "$PROBE_JSON" ]; then
    if [ "$PROBE_RC" -ne 0 ]; then return 0; fi
    note_fail "$label: expected a denial but the probe exited 0 without a result"
    return 1
  fi
  if [ "$(printf '%s' "$PROBE_JSON" | jq -r '.allowed' 2>/dev/null)" = "false" ]; then return 0; fi
  note_fail "$label: expected the operation to be blocked, got $PROBE_JSON"
  return 1
}
# A seccomp SCMP_ACT_KILL_PROCESS terminates the process with SIGSYS (31); the
# Docker Engine API reports a signal-terminated process as 128+signal, so the
# probe dies with rc 159 and prints no JSON. Only that named rc is a kill: a Go
# crash exits 2, a missing binary 126/127, SIGSEGV 139, and so on. Accepting any
# non-zero exit would record a crash as a sandbox kill, so the set is explicit
# and only overridable deliberately by a Linux host that documents another rc.
seccomp_kill_rcs="${AURORA_SECCOMP_KILL_RCS:-159}"

require_killed() {
  local label="$1" rc
  shift
  probe "$@"
  if [ -n "$PROBE_JSON" ]; then
    note_fail "$label: expected the seccomp SIGSYS kill, got a result: $PROBE_JSON"
    return 1
  fi
  for rc in $seccomp_kill_rcs; do
    if [ "$PROBE_RC" = "$rc" ]; then return 0; fi
  done
  note_fail "$label: expected the seccomp SIGSYS kill (rc in '$seccomp_kill_rcs'), got rc=$PROBE_RC"
  return 1
}
wait_allowed() {
  # wait_allowed <label> <seconds> <args...>
  local label="$1" timeout="$2"; shift 2
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    probe "$@"
    if [ -n "$PROBE_JSON" ] && [ "$(printf '%s' "$PROBE_JSON" | jq -r '.allowed' 2>/dev/null)" = "true" ]; then return 0; fi
    sleep 1
  done
  note_fail "$label: never became allowed within ${timeout}s (last rc=$PROBE_RC out=$PROBE_JSON)"
  return 1
}

# ---------------------------------------------------------------------------
# The documented Fleet node contract (read this before trusting a green run).
#
# server/internal/fleet/docker/provider.go is the authority for the real node:
# Ensure() mounts the node data volume at /data and the read-only secrets volume
# at /secrets, runs the node as 10001:10001 with HOME=/data/home, and
# NodeHostConfig() sets the CPU, memory and PID limits, CapDrop ALL,
# no-new-privileges:true and a disabled restart policy. The Go test
# TestNodeHostConfigIsRestricted (server/internal/fleet/docker/provider_test.go),
# extended by Task 5, is the authority for provider drift. This acceptance
# cannot observe a host the Go provider never built.
#
# Every container below is labelled by what it proves:
#   * "release image contract"      - metadata/layout the published image ships.
#   * "probe fixture configuration" - flags and mounts this script passes to its
#     own fixture; they prove the fixture matches the documented shape, not that
#     the real node applies them.
#   * "isolation behaviour"         - denied operations attempted under the
#     fixture's AppArmor + seccomp profiles; the real security evidence for the
#     profiles. AppArmor, seccomp and a read-only rootfs are fixture-only
#     hardening: NodeHostConfig does not set them yet, so a green matrix proves
#     the profiles behave, not that the Fleet has wired them.
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# TestDockerSandboxImageContract: release image metadata and the fixed
# /data + /secrets layout the published image must ship. No probe flag is
# asserted here, so nothing in this test is tautological.
# ---------------------------------------------------------------------------
test_image_contract() {
  start_test
  local entry user health volumes data_vol out rc

  entry="$(docker image inspect --format '{{json .Config.Entrypoint}}' "$release_image" 2>/dev/null)"
  [ "$entry" = '["/usr/local/bin/fleet-node","run"]' ] || note_fail "release image entrypoint is '$entry', want the fixed fleet-node run"
  user="$(docker image inspect --format '{{.Config.User}}' "$release_image" 2>/dev/null)"
  [ "$user" = "10001:10001" ] || note_fail "release image Config.User is '$user', want 10001:10001"
  health="$(docker image inspect --format '{{json .Config.Healthcheck.Test}}' "$release_image" 2>/dev/null)"
  printf '%s' "$health" | grep -q 'fleet-node' || note_fail "release image health check is $health, want fleet-node"
  printf '%s' "$health" | grep -q 'health' || note_fail "release image health check is $health, want the health subcommand"
  volumes="$(docker image inspect --format '{{json .Config.Volumes}}' "$release_image" 2>/dev/null)"
  case "$volumes" in
    ""|"null"|"{}") ;;
    *) note_fail "release image declares volumes: $volumes" ;;
  esac

  # release image contract: a fresh named volume at /data is seeded from the
  # image layout and writable by the non-root node; the image ships no package
  # managers or download tools and no baked enrollment secret. Docker, not this
  # script, populates the volume from /data, so this is a real behavioural probe.
  data_vol="aurora-acc-data-$$"
  docker volume rm "$data_vol" >/dev/null 2>&1 || true
  if docker volume create "$data_vol" >/dev/null 2>&1; then
    cat >"$work/layout.sh" <<'LAYOUT'
set -eu
test -x /usr/local/bin/fleet-node
test -x /usr/local/bin/multica
for d in /data /data/home /data/workspaces /secrets; do
  test -d "$d" || { echo "missing layout directory $d"; exit 1; }
  owner="$(stat -c '%u:%g' "$d")"
  [ "$owner" = "10001:10001" ] || { echo "layout directory $d is owned by $owner"; exit 1; }
done
[ ! -e /secrets/aurora-enrollment ] || { echo "image bakes an enrollment secret"; exit 1; }
touch /data/workspaces/.acceptance-probe
rm -f /data/workspaces/.acceptance-probe
for b in npm npx pnpm pnpx corepack yarn git curl wget ssh scp sftp apt apt-get dpkg; do
  if command -v "$b" >/dev/null 2>&1; then echo "forbidden binary present: $b"; exit 1; fi
done
echo LAYOUT_OK
LAYOUT
    out="$(docker run --rm -i --network none --read-only --user 10001:10001 --tmpfs /tmp \
      -v "$data_vol:/data" --entrypoint /bin/sh "$release_image" - <"$work/layout.sh" 2>&1)"
    printf '%s' "$out" | grep -q 'LAYOUT_OK' || note_fail "release image layout probe failed: $out"
  else
    note_fail "could not create the acceptance data volume"
  fi
  docker volume rm "$data_vol" >/dev/null 2>&1 || true

  # release image contract: the real node binary fails closed on an empty
  # command, so a misconfigured node never runs as a long-lived no-op.
  out="$(docker run --rm --network none --read-only --user 10001:10001 --tmpfs /tmp \
    --entrypoint /usr/local/bin/fleet-node "$release_image" 2>&1)"
  rc=$?
  [ "$rc" -ne 0 ] || note_fail "fleet-node accepted an empty command"
  printf '%s' "$out" | grep -q 'fleet-node command failed' || note_fail "fleet-node did not fail closed: $out"

  finish_test
}

# ---------------------------------------------------------------------------
# TestDockerSandboxProbeFixtureConfig: the probe fixture container is built to
# the documented Fleet node contract above (entrypoint, user, read-only root,
# capability drop, limits, env, and the fixed /data + /secrets mounts). These
# are flag-derived assertions against a container this script creates itself, so
# they are reported as "probe fixture configuration": they prove the fixture
# matches the documented shape, not that the real node applies it. The Go
# provider test is the authority for the real boundary.
# ---------------------------------------------------------------------------
test_probe_fixture_config() {
  start_test
  local cid data_vol secret_target data_target secret_ro

  printf '# probe fixture configuration: the flags and mounts below mirror server/internal/fleet/docker/provider.go; the Go provider test is the authority for real provider drift\n'

  data_vol="aurora-acc-fleet-data-$$"
  docker volume rm "$data_vol" >/dev/null 2>&1 || true
  docker volume create "$data_vol" >/dev/null 2>&1 || note_fail "probe fixture configuration: could not create the /data fixture volume"

  cid="aurora-acc-fixture-$$"
  docker rm -f "$cid" >/dev/null 2>&1 || true
  if docker create --name "$cid" --user 10001:10001 --read-only \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --pids-limit 256 --memory 4g --cpus 2 \
    -v "$data_vol:/data" \
    -v "$secret_dir:/secrets:ro" \
    -e HOME=/data/home -e FLEET_NODE_MAX_RUNS=1 \
    -e MULTICA_MANAGED=1 -e MULTICA_SERVER_URL=http://127.0.0.1:8090 \
    -e MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=/secrets/aurora-enrollment \
    --entrypoint /usr/local/bin/fleet-node "$release_image" run >/dev/null 2>&1; then
    [ "$(docker inspect --format '{{json .Config.Entrypoint}}' "$cid")" = '["/usr/local/bin/fleet-node"]' ] || note_fail "probe fixture configuration: entrypoint override drifted"
    [ "$(docker inspect --format '{{json .Config.Cmd}}' "$cid")" = '["run"]' ] || note_fail "probe fixture configuration: Cmd is not [run]"
    [ "$(docker inspect --format '{{.Config.User}}' "$cid")" = "10001:10001" ] || note_fail "probe fixture configuration: Config.User drifted"
    [ "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$cid")" = "true" ] || note_fail "probe fixture configuration: fixture is not read-only"
    docker inspect --format '{{json .HostConfig.CapDrop}}' "$cid" | grep -q '"ALL"' || note_fail "probe fixture configuration: fixture does not drop ALL capabilities"
    docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$cid" | grep -q 'no-new-privileges' || note_fail "probe fixture configuration: fixture does not set no-new-privileges"
    [ "$(docker inspect --format '{{.HostConfig.Memory}}' "$cid")" = "4294967296" ] || note_fail "probe fixture configuration: fixture memory limit drifted"
    [ "$(docker inspect --format '{{.HostConfig.NanoCpus}}' "$cid")" = "2000000000" ] || note_fail "probe fixture configuration: fixture CPU limit drifted"
    [ "$(docker inspect --format '{{.HostConfig.PidsLimit}}' "$cid")" = "256" ] || note_fail "probe fixture configuration: fixture PIDs limit drifted"
    docker inspect --format '{{json .Config.Env}}' "$cid" | grep -q 'HOME=/data/home' || note_fail "probe fixture configuration: fixture HOME is not /data/home"
    secret_target="$(docker inspect --format '{{json .HostConfig.Mounts}}' "$cid" | jq -r '.[] | select(.Target=="/secrets") | .Target' 2>/dev/null)"
    [ "$secret_target" = "/secrets" ] || note_fail "probe fixture configuration: the secrets volume is not mounted at the fixed /secrets target"
    secret_ro="$(docker inspect --format '{{json .HostConfig.Mounts}}' "$cid" | jq -r '.[] | select(.Target=="/secrets") | .ReadOnly' 2>/dev/null)"
    [ "$secret_ro" = "true" ] || note_fail "probe fixture configuration: /secrets is not mounted read-only"
    data_target="$(docker inspect --format '{{json .HostConfig.Mounts}}' "$cid" | jq -r '.[] | select(.Target=="/data") | .Target' 2>/dev/null)"
    [ "$data_target" = "/data" ] || note_fail "probe fixture configuration: the data volume is not mounted at the fixed /data target"
  else
    note_fail "probe fixture configuration: could not create the Fleet node fixture container"
  fi
  docker rm -f "$cid" >/dev/null 2>&1 || true
  docker volume rm "$data_vol" >/dev/null 2>&1 || true

  finish_test
}

# ---------------------------------------------------------------------------
# TestDockerSandboxFixtureContract: the fake-CLI Fleet fixture is a working
# contract double, so the acceptance can validate the node shape without the
# real Claude install.
# ---------------------------------------------------------------------------
test_fixture_contract() {
  start_test
  local entry user health out

  entry="$(docker image inspect --format '{{json .Config.Entrypoint}}' "$fleet_fixture" 2>/dev/null)"
  [ "$entry" = '["/usr/local/bin/fleet-node","run"]' ] || note_fail "fixture entrypoint is '$entry', want the fixed fleet-node run"
  user="$(docker image inspect --format '{{.Config.User}}' "$fleet_fixture" 2>/dev/null)"
  [ "$user" = "10001:10001" ] || note_fail "fixture Config.User is '$user', want 10001:10001"
  health="$(docker image inspect --format '{{json .Config.Healthcheck.Test}}' "$fleet_fixture" 2>/dev/null)"
  printf '%s' "$health" | grep -q 'fleet-node' || note_fail "fixture health check is $health"

  cat >"$work/fixture.sh" <<'FIXTURE'
set -eu
test -x /usr/local/bin/fleet-node || { echo "missing fleet-node"; exit 1; }
test -x /usr/local/bin/multica || { echo "missing multica"; exit 1; }
test -x /usr/local/bin/claude || { echo "missing fake claude"; exit 1; }
version="$(/usr/local/bin/claude --version)"
case "$version" in *fixture*) ;; *) echo "unexpected fake claude version: $version"; exit 1 ;; esac
/usr/local/bin/fleet-node health | grep -q '"ready":true' || { echo "fake fleet-node health did not report ready"; exit 1; }
for d in /data /data/home /data/workspaces /secrets; do
  test -d "$d" || { echo "missing layout directory $d"; exit 1; }
  [ "$(stat -c '%u:%g' "$d")" = "10001:10001" ] || { echo "layout directory $d has the wrong owner"; exit 1; }
done
echo FIXTURE_OK
FIXTURE
  out="$(docker run --rm -i --network none --read-only --user 10001:10001 --tmpfs /tmp \
    --entrypoint /bin/sh "$fleet_fixture" - <"$work/fixture.sh" 2>&1)"
  printf '%s' "$out" | grep -q 'FIXTURE_OK' || note_fail "fixture contract probe failed: $out"

  finish_test
}

# ---------------------------------------------------------------------------
# TestDockerSandboxLinuxSecurityBoundary: the Fleet host hardening plus the
# AppArmor and seccomp profiles over the fixed probe binary, with the egress
# sidecar in front of every outbound path.
# ---------------------------------------------------------------------------
sandbox_name=""
proxy_name=""
origin_name=""
peer_name=""
net_name=""
other_net_name=""
data_vol=""
# The single-use enrollment secret the Fleet writes into its read-only secrets
# volume at the fixed /secrets/aurora-enrollment path (model.AuroraEnrollmentFile).
secret_dir="$(mktemp -d "$work/secret.XXXXXX")"
printf 'mse_%040d' 0 >"$secret_dir/aurora-enrollment"
chmod 0400 "$secret_dir/aurora-enrollment"

boundary_cleanup() {
  if [ -n "$sandbox_name" ]; then docker rm -f "$sandbox_name" >/dev/null 2>&1 || true; fi
  if [ -n "$proxy_name" ]; then docker rm -f "$proxy_name" >/dev/null 2>&1 || true; fi
  if [ -n "$origin_name" ]; then docker rm -f "$origin_name" >/dev/null 2>&1 || true; fi
  if [ -n "$peer_name" ]; then docker rm -f "$peer_name" >/dev/null 2>&1 || true; fi
  if [ -n "$net_name" ]; then docker network rm "$net_name" >/dev/null 2>&1 || true; fi
  if [ -n "$other_net_name" ]; then docker network rm "$other_net_name" >/dev/null 2>&1 || true; fi
  if [ -n "$data_vol" ]; then docker volume rm "$data_vol" >/dev/null 2>&1 || true; fi
  sandbox_name=""; proxy_name=""; origin_name=""; peer_name=""; net_name=""; other_net_name=""; data_vol=""
}
# Keep the leak-free guarantee even if a probe aborts the run.
trap 'boundary_cleanup; cleanup' EXIT

test_isolation_boundary() {
  start_test
  local id="$$-$RANDOM" gateway peer_ip

  printf '# isolation behaviour: the AppArmor and seccomp profiles below are probe fixture configuration; NodeHostConfig does not set them yet, so the Go provider test is the authority for provider drift\n'

  data_vol="aurora-acc-net-data-$id"
  docker volume rm "$data_vol" >/dev/null 2>&1 || true
  docker volume create "$data_vol" >/dev/null 2>&1 || note_fail "could not create the isolation /data volume"

  net_name="aurora-acc-net-$id"
  other_net_name="aurora-acc-other-$id"
  sandbox_name="aurora-acc-sandbox-$id"
  proxy_name="aurora-acc-proxy-$id"
  origin_name="aurora-acc-origin-$id"
  peer_name="aurora-acc-peer-$id"

  docker network create --internal "$net_name" >/dev/null 2>&1 || note_fail "could not create the workspace-internal network"
  docker network create --internal "$other_net_name" >/dev/null 2>&1 || note_fail "could not create the peer network"

  # A plain-HTTP origin the egress policy may reach, served by the release
  # image's Node runtime on the workspace-internal network.
  docker run -d --name "$origin_name" --network "$net_name" --network-alias origin --user 10001:10001 --read-only \
    --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,size=64m,mode=1777 \
    --entrypoint /usr/local/bin/node "$release_image" \
    -e 'require("http").createServer(function(q,s){s.end("ok")}).listen(8080,"0.0.0.0")' >/dev/null 2>&1 \
    || note_fail "could not start the plain-HTTP origin"

  # The egress sidecar is attached to the workspace network under the fixed
  # "egress" alias the sandbox resolves.
  docker run -d --name "$proxy_name" --network "$net_name" --network-alias egress \
    --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges \
    --tmpfs /tmp:rw,nosuid,nodev,noexec,size=33554432,uid=10001,gid=10001,mode=0700 \
    -e MULTICA_EGRESS_SERVER_ORIGIN="http://origin:8080" \
    "$proxy_image" >/dev/null 2>&1 || note_fail "could not start the egress sidecar"

  # The sandbox runs the fixed probe under probe fixture configuration: the
  # documented Fleet flags (non-root, capability drop, no-new-privileges,
  # resource limits, the fixed /data + /secrets mounts) plus the AppArmor and
  # seccomp profiles. A read-only rootfs, AppArmor and seccomp are fixture-only
  # hardening today; the Go provider test is the authority for whether the Fleet
  # sets them.
  docker run -d --name "$sandbox_name" --network "$net_name" --user 10001:10001 --read-only \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --security-opt "seccomp=$seccomp_profile" --security-opt "apparmor=$apparmor_profile" \
    --pids-limit 256 --memory 4g --memory-swap 4g --cpus 2 --ulimit nofile=1024:1024 \
    --tmpfs /workspace:rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700 \
    --tmpfs /tmp:rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700 \
    --tmpfs /run:rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755 \
    -v "$data_vol:/data" \
    -v "$secret_dir:/secrets:ro" \
    -e MULTICA_SERVER_URL="http://origin:8080" \
    -e MULTICA_MANAGED=1 \
    -e MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=/secrets/aurora-enrollment \
    -e HTTP_PROXY=http://egress:3128 \
    -e HTTPS_PROXY=http://egress:3128 \
    -e NO_PROXY=egress,127.0.0.1,localhost \
    "$probe_image" >/dev/null 2>&1 || note_fail "could not start the probe sandbox"

  if [ "$(docker inspect --format '{{.State.Running}}' "$sandbox_name" 2>/dev/null)" != "true" ]; then
    note_fail "the probe sandbox is not running; the AppArmor or seccomp profile may be unenforceable"
    finish_test
    boundary_cleanup
    return 1
  fi

  # Inspected container boundary.
  [ "$(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$sandbox_name")" = "true" ] || note_fail "sandbox is not read-only"
  docker inspect --format '{{json .HostConfig.CapDrop}}' "$sandbox_name" | grep -q '"ALL"' || note_fail "sandbox does not drop ALL capabilities"
  docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$sandbox_name" | grep -q 'no-new-privileges' || note_fail "sandbox does not set no-new-privileges"

  require_allowed "fs-write-workspace" fs-write-workspace
  require_blocked "fs-write-root" fs-write-root
  require_blocked "privilege-escalate" privilege-escalate
  require_blocked "raw-socket" raw-socket
  require_killed "mount" mount
  require_killed "unshare" unshare
  require_killed "ptrace" ptrace
  require_blocked "fork-pressure" fork-pressure 256
  require_blocked "tmpfs-quota /workspace" tmpfs-quota /workspace/quota 2147483648
  require_blocked "tmpfs-quota /tmp" tmpfs-quota /tmp/quota 268435456
  require_blocked "tmpfs-quota /run" tmpfs-quota /run/quota 16777216

  gateway="$(docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}' "$net_name" 2>/dev/null)"
  for target in 1.1.1.1:443 10.0.0.1:443 192.168.0.1:443 169.254.169.254:80 example.com:443; do
    require_blocked "dial $target" dial "$target"
  done
  if [ -n "$gateway" ]; then require_blocked "dial gateway:9" dial "$gateway:9"; fi

  wait_allowed "proxy-get allowed origin" 20 proxy-get http://origin:8080/aurora-acceptance
  require_blocked "proxy-get wrong port" proxy-get http://origin:9999/
  require_blocked "proxy-get unknown host" proxy-get http://unknown.invalid/
  require_blocked "proxy-connect unknown host" proxy-connect https://unknown.invalid/
  require_blocked "proxy-connect private target" proxy-connect https://10.0.0.1:443/

  # A second workspace network and listener must be invisible.
  docker run -d --name "$peer_name" --network "$other_net_name" "$probe_image" listen 9000 >/dev/null 2>&1 || note_fail "could not start the peer listener"
  peer_ip="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$peer_name" 2>/dev/null)"
  if [ -n "$peer_ip" ]; then
    require_blocked "network-visibility" network-visibility "$peer_name" "$peer_ip" 9000
  else
    note_fail "the peer listener has no address on the second network"
  fi

  boundary_cleanup
  finish_test
}

# ---------------------------------------------------------------------------
# TestDockerSandboxFleetLayoutBoundary: the fixed Fleet layout is readable and
# writable through the loaded AppArmor profile. This is the behavioural proof
# for the profile's fixed-layout rules and is reported as isolation behaviour.
# It runs the release image's own shell under the seccomp/AppArmor profiles the
# isolation fixture uses, because the scratch probe image has no shell and the
# profile is what is under test.
# ---------------------------------------------------------------------------
test_fleet_layout_boundary() {
  start_test
  local id="$$-$RANDOM" layout_data layout_secrets token out rc

  token="$(printf 'mse_%040d' 0)"
  layout_data="aurora-acc-layout-data-$id"
  layout_secrets="aurora-acc-layout-secrets-$id"
  docker volume rm "$layout_data" "$layout_secrets" >/dev/null 2>&1 || true
  docker volume create "$layout_data" >/dev/null 2>&1 || note_fail "could not create the layout /data volume"
  docker volume create "$layout_secrets" >/dev/null 2>&1 || note_fail "could not create the layout /secrets volume"

  # Seed both volumes the way the Fleet installer does: the layout manifest and
  # the single-use enrollment secret owned by 10001 with fixed modes. A second
  # secret the profile does NOT name proves the rules do not widen /secrets.
  if ! docker run --rm --network none --user 0:0 \
    -e ACCEPTANCE_ENROLLMENT="$token" \
    -v "$layout_data:/data" -v "$layout_secrets:/secrets" \
    --entrypoint /bin/sh "$release_image" -c '
      set -eu
      install -d -o 10001 -g 10001 -m 0700 /data/home /data/workspaces /secrets
      printf "%s" "{}" > /data/fleet-layout.json
      printf "%s" "$ACCEPTANCE_ENROLLMENT" > /secrets/aurora-enrollment
      printf "%s" "$ACCEPTANCE_ENROLLMENT" > /secrets/aurora-provider-x
      chmod 0600 /data/fleet-layout.json
      chmod 0400 /secrets/aurora-enrollment /secrets/aurora-provider-x
      chown -R 10001:10001 /data /secrets
    ' >/dev/null 2>&1; then
    note_fail "could not seed the fixed Fleet layout volumes"
    docker volume rm "$layout_data" "$layout_secrets" >/dev/null 2>&1 || true
    finish_test
    return 1
  fi

  cat >"$work/fleet-layout.sh" <<'LAYOUT'
set -u
fail=0
mark() { echo "LAYOUT_BOUNDARY_$1 $2"; }
# positive: the fixed enrollment secret is readable and holds the issued token.
enroll="$(cat /secrets/aurora-enrollment 2>/dev/null || true)"
[ "$enroll" = "$ACCEPTANCE_ENROLLMENT" ] || { mark DENIED "read /secrets/aurora-enrollment"; fail=1; }
# positive: the /secrets directory the node resolves the file through is listable.
ls /secrets >/dev/null 2>&1 || { mark DENIED "list /secrets"; fail=1; }
# positive: the node writes its home, workspaces and layout manifest under /data.
for path in /data/home/.acceptance /data/workspaces/.acceptance /data/fleet-layout.probe; do
  printf 'x' >"$path" 2>/dev/null || { mark DENIED "write $path"; fail=1; }
done
# negative: the read-only /secrets volume accepts no write.
if printf 'x' >/secrets/evil 2>/dev/null; then mark UNEXPECTED "write /secrets/evil"; fail=1; fi
# negative: the profile does not widen /secrets to a secret it does not name.
if cat /secrets/aurora-provider-x >/dev/null 2>&1; then mark UNEXPECTED "read /secrets/aurora-provider-x"; fail=1; fi
# negative: paths outside the fixed layout stay default-denied.
if printf 'x' >/aurora-probe-root-write 2>/dev/null; then mark UNEXPECTED "write /aurora-probe-root-write"; fail=1; fi
[ "$fail" -eq 0 ] && mark OK "fixed Fleet layout permitted under AppArmor"
exit "$fail"
LAYOUT

  out="$(docker run --rm -i --network none --read-only --user 10001:10001 \
    --cap-drop ALL --security-opt no-new-privileges:true \
    --security-opt "apparmor=$apparmor_profile" --security-opt "seccomp=$seccomp_profile" \
    --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0700 \
    -e ACCEPTANCE_ENROLLMENT="$token" \
    -v "$layout_data:/data" -v "$layout_secrets:/secrets:ro" \
    --entrypoint /bin/sh "$release_image" - <"$work/fleet-layout.sh" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | grep -q 'LAYOUT_BOUNDARY_OK' || note_fail "the fixed Fleet layout was not permitted under AppArmor (rc=$rc): $out"
  [ "$rc" -eq 0 ] || note_fail "fixed Fleet layout probe exited $rc: $out"

  docker volume rm "$layout_data" "$layout_secrets" >/dev/null 2>&1 || true
  finish_test
}

# ---------------------------------------------------------------------------
# TestDockerSandboxFakeAuroraPipelines: the containerized fake pipeline smoke
# inside the release image, off the network, with no provider or agent call.
# ---------------------------------------------------------------------------
test_fake_pipelines() {
  start_test
  local log="$work/pipelines-$$-$RANDOM.log" result_line ok want

  if [ ! -f "$harness" ]; then
    note_fail "pipeline harness $harness is missing"
    finish_test
    return 1
  fi

  if docker run --rm --pull never --network none --user 10001:10001 --read-only \
    --tmpfs /tmp:rw,size=4g,mode=1777 \
    --tmpfs /workspace:rw,size=4g,uid=10001,gid=10001,mode=0700 \
    --shm-size 512m --cap-drop ALL --security-opt no-new-privileges \
    --pids-limit 256 --memory 4g --cpus 2 -e HOME=/tmp \
    -v "$fixtures_dir:/opt/aurora/smoke:ro" \
    --entrypoint /usr/local/bin/node "$release_image" \
    /opt/aurora/smoke/smoke/aurora-fake-pipelines.mjs >"$log" 2>&1; then
    if ! grep -q 'AURORA_SMOKE_RESULT' "$log"; then
      note_fail "fake pipeline printed no result marker: $(tail -n 3 "$log")"
    else
      result_line="$(grep 'AURORA_SMOKE_RESULT' "$log" | tail -n 1 | sed 's/^AURORA_SMOKE_RESULT //')"
      ok="$(printf '%s' "$result_line" | jq -r '.ok' 2>/dev/null)"
      [ "$ok" = "true" ] || note_fail "fake pipeline result is not ok: $result_line"
      for want in xhs-image text-video video-captions resume provider-failure-refund-no-fallback; do
        printf '%s' "$result_line" | jq -e --arg w "$want" '.pipelines | index($w) != null' >/dev/null 2>&1 \
          || note_fail "fake pipeline did not report $want"
      done
    fi
  else
    note_fail "the fake pipeline smoke failed: $(tail -n 3 "$log")"
  fi

  finish_test
}

for pass in $(seq 1 "$count"); do
  printf '=== RUN   Fleet sandbox acceptance pass %s/%s\n' "$pass" "$count" >&2
  run_test TestDockerSandboxImageContract test_image_contract
  run_test TestDockerSandboxProbeFixtureConfig test_probe_fixture_config
  run_test TestDockerSandboxFixtureContract test_fixture_contract
  run_test TestDockerSandboxLinuxSecurityBoundary test_isolation_boundary
  run_test TestDockerSandboxFleetLayoutBoundary test_fleet_layout_boundary
  run_test TestDockerSandboxFakeAuroraPipelines test_fake_pipelines
  [ "$fail_count" -eq 0 ] || break
done

if [ "$fail_count" -gt 0 ]; then
  exit 1
fi
if [ "$pass_count" -eq 0 ]; then
  exit 3
fi
exit 0
