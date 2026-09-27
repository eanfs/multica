#!/usr/bin/env bash
# Aurora sandbox Linux Docker security acceptance entry point.
#
# This script builds the fixture sandbox and egress images, loads the AppArmor
# profile, and runs the auroradocker-tagged acceptance tests on a Linux Docker
# Engine host: the isolation/egress boundary plus Task 3's containerized fake
# pipeline smoke (fake providers, real HyperFrames/FFmpeg/Chromium). Docker
# Desktop on macOS cannot satisfy the AppArmor, cgroup, or kernel gates, so this
# script fails loudly there.
#
# Usage:
#   deploy/aurora-sandbox/docker-security-test.sh
#   AURORA_DOCKER_SECURITY_COUNT=2 deploy/aurora-sandbox/docker-security-test.sh
#
# Set both AURORA_FIXTURE_SANDBOX_REF and AURORA_FIXTURE_PROXY_REF to reuse
# prebuilt digest-pinned fixture images instead of building them.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
server_dir="$repo_root/server"

fail() {
  echo "docker-security-test: $*" >&2
  exit 1
}

[ "$(uname -s)" = "Linux" ] || fail "this acceptance runs only on Linux Docker Engine; on $(uname -s) use the functional smoke instead"

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "the Docker daemon is not reachable"

seccomp_profile="$repo_root/deploy/aurora-sandbox/seccomp.json"
apparmor_profile="$repo_root/deploy/aurora-sandbox/multica-aurora-sandbox.apparmor"
[ -f "$seccomp_profile" ] || fail "missing seccomp profile: $seccomp_profile"
[ -f "$apparmor_profile" ] || fail "missing AppArmor profile: $apparmor_profile"

# AppArmor is a mandatory part of the Linux acceptance boundary.
[ -e /sys/module/apparmor ] || fail "the kernel does not expose AppArmor; the acceptance requires it"
if [ -r /sys/module/apparmor/parameters/enabled ] && [ "$(cat /sys/module/apparmor/parameters/enabled)" != "Y" ]; then
  fail "AppArmor is present but disabled in the kernel"
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

fixture_sandbox_ref="${AURORA_FIXTURE_SANDBOX_REF:-}"
fixture_proxy_ref="${AURORA_FIXTURE_PROXY_REF:-}"
if [ -n "$fixture_sandbox_ref" ] || [ -n "$fixture_proxy_ref" ]; then
  [ -n "$fixture_sandbox_ref" ] && [ -n "$fixture_proxy_ref" ] || fail "set both AURORA_FIXTURE_SANDBOX_REF and AURORA_FIXTURE_PROXY_REF, or neither"
fi

staging="$(mktemp -d)"
cleanup() { rm -rf "$staging"; }
trap cleanup EXIT

# image_ref resolves a locally tagged image to an immutable reference the
# local image store can actually resolve. It prefers the registry digest Docker
# records in RepoDigests; when the store records none (an image built locally on
# the classic image store), it derives one from the image ID. That derived
# <repo>@<image-id> form is not resolvable on the classic store, so passing it
# to docker run starts a network pull instead; the bare image ID is
# content-addressed and always resolves while the image is present. Both forms
# keep the hardened policy's immutability boundary; a mutable tag never passes.
image_ref() {
  local tag="$1" ref id candidate
  id="$(docker inspect --format '{{.Id}}' "$tag")" || fail "cannot inspect image $tag"
  ref="$(docker inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$tag" 2>/dev/null || true)"
  if [ -z "$ref" ]; then
    ref="${tag%:*}@$id"
  fi
  case "$ref" in
    *@sha256:*) ;;
    *) fail "resolved image reference $ref is not digest-pinned" ;;
  esac
  # Prove the reference resolves before a docker invocation can turn it into a
  # network pull. docker image inspect never pulls.
  for candidate in "$ref" "$id"; do
    if docker image inspect "$candidate" >/dev/null 2>&1; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  fail "image $tag is not resolvable locally as $ref or $id; refusing to pull"
}

if [ -z "$fixture_sandbox_ref" ]; then
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$go_bin" build -trimpath -o "$staging/aurora-sandbox-probe" ./cmd/aurora-sandbox-probe ) || fail "failed to build the probe binary"
  ( cd "$server_dir" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$go_bin" build -trimpath -o "$staging/aurora-egress-proxy" ./cmd/aurora-egress-proxy ) || fail "failed to build the egress proxy binary"
  docker build -f "$repo_root/deploy/aurora-sandbox/fixture/Dockerfile.sandbox" -t "${AURORA_FIXTURE_SANDBOX_TAG:-multica-aurora-sandbox-fixture:local}" "$staging" || fail "failed to build the fixture sandbox image"
  docker build -f "$repo_root/deploy/aurora-sandbox/fixture/Dockerfile.egress" -t "${AURORA_FIXTURE_PROXY_TAG:-multica-aurora-egress-fixture:local}" "$staging" || fail "failed to build the fixture egress image"
  fixture_sandbox_ref="$(image_ref "${AURORA_FIXTURE_SANDBOX_TAG:-multica-aurora-sandbox-fixture:local}")"
  fixture_proxy_ref="$(image_ref "${AURORA_FIXTURE_PROXY_TAG:-multica-aurora-egress-fixture:local}")"
fi

printf 'AURORA_SANDBOX_IMAGE=%s\n' "$fixture_sandbox_ref"
printf 'AURORA_PROXY_IMAGE=%s\n' "$fixture_proxy_ref"
printf 'AURORA_SECCOMP_PROFILE=%s\n' "$seccomp_profile"

export AURORA_RUN_DOCKER_SECURITY_TEST=1
export AURORA_SANDBOX_IMAGE="$fixture_sandbox_ref"
export AURORA_PROXY_IMAGE="$fixture_proxy_ref"
export AURORA_SECCOMP_PROFILE="$seccomp_profile"
export AURORA_APPARMOR_PROFILE="multica-aurora-sandbox"

count="${AURORA_DOCKER_SECURITY_COUNT:-1}"
cd "$server_dir"
# Both auroradocker tests run against the same immutable image pair: the
# Linux isolation/egress boundary and Task 3's containerized fake pipelines
# (fake Multica/Ark/OpenAI/ASR endpoints, real HyperFrames/FFmpeg/Chromium).
"$go_bin" test -tags=auroradocker ./internal/aurorafleet \
  -run '^(TestDockerSandboxLinuxSecurityBoundary|TestDockerSandboxFakeAuroraPipelines)$' \
  -count="$count" -v "$@"
