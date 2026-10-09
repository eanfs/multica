#!/usr/bin/env bash
# Runtime node image contract.
#
# Checks the single published node image carries exactly the entrypoints, the
# fixed layout and the credential-free environment the daemon contract needs.
# It inspects and runs the image; it never builds one, and it never pulls.
#
# Usage: scripts/check-runtime-image.sh [<image-ref>]   (default multica-runtime-node:dev)
#
# Exit codes, matching the repository's other acceptance scripts:
#   0  every check passed
#   1  at least one check failed
#   3  the environment cannot evaluate the image (no Docker daemon, or the
#      image is not present locally). Callers record this as SKIP, never PASS.
set -euo pipefail

image="${1:-multica-runtime-node:dev}"
failures=0

pass() { printf -- '--- PASS: %s\n' "$1"; }
fail() {
  printf -- '--- FAIL: %s (%s)\n' "$1" "$2"
  failures=$((failures + 1))
}

check_eq() { # check_eq <name> <expected> <actual>
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "want '$2', got '$3'"; fi
}

if ! docker info >/dev/null 2>&1; then
  printf -- '--- SKIP: docker daemon unreachable; cannot evaluate %s\n' "$image" >&2
  exit 3
fi

if ! docker image inspect "$image" >/dev/null 2>&1; then
  printf -- '--- SKIP: image %s is not present locally; build it first\n' "$image" >&2
  exit 3
fi

inspect() { docker image inspect "$image" --format "$1"; }

# The image runs as the fixed non-root node user, starts through the fleet-node
# entrypoint only, and targets an architecture the fleet publishes.
check_eq user 10001:10001 "$(inspect '{{.Config.User}}')"
check_eq entrypoint '[/usr/local/bin/fleet-node run]' "$(inspect '{{.Config.Entrypoint}}')"
if inspect '{{.Config.Healthcheck.Test}}' | grep -q 'fleet-node'; then
  pass healthcheck_uses_fleet_node
else
  fail healthcheck_uses_fleet_node "$(inspect '{{.Config.Healthcheck.Test}}')"
fi

# Config.Env carries neither a credential value nor a credential name. The
# provider keys arrive from the environment at run time, injected by the caller;
# baking a name here would suggest the image expects one of its own.
env_dump="$(inspect '{{range .Config.Env}}{{println .}}{{end}}')"
if printf '%s\n' "$env_dump" | grep -qE '(API_KEY|TOKEN|SECRET|PASSWORD)='; then
  fail env_has_no_credentials "$(printf '%s' "$env_dump" | tr '\n' ' ')"
else
  pass env_has_no_credentials
fi

# Everything below runs inside the image.
probe() { docker run --rm --entrypoint /bin/sh "$image" -c "$1"; }

for bin in /usr/local/bin/multica /usr/local/bin/fleet-node /usr/local/bin/claude; do
  if probe "[ -x $bin ]"; then pass "entrypoint_executable $bin"; else fail "entrypoint_executable $bin" "missing or not executable"; fi
done

# The shell and the media toolchain the skill documents call.
for tool in bash curl jq unzip chromium ffmpeg convert pdftoppm pdfinfo; do
  if probe "command -v $tool >/dev/null"; then pass "tool_present $tool"; else fail "tool_present $tool" "not on PATH"; fi
done

# The node's persistent layout belongs to the node user, so a fresh volume
# mounted at either path is initialised writable for it.
check_eq layout_owner /data:10001:10001:/secrets:10001:10001 \
  "$(probe 'stat -c "%n:%u:%g" /data /secrets' | tr '\n' ' ' | sed 's/ $//' | sed 's/ /:/g')"

# The single Claude install answers. Its path is the image's own entry, not a
# second copy: the Aurora profile used to pin a vendored one under /opt/aurora.
claude_version="$(probe '/usr/local/bin/claude --version 2>/dev/null | head -1')"
if [ -n "$claude_version" ]; then pass "claude_runs ($claude_version)"; else fail claude_runs "no version output"; fi

if [ "$failures" -ne 0 ]; then
  printf 'runtime image contract: %d check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'runtime image contract: ok (%s)\n' "$image"
