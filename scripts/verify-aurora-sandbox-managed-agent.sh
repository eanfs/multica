#!/usr/bin/env bash
# Verify the managed daemon can resolve and run its one agent executable,
# exactly as server/internal/daemon/config.go managedClaudeAgent does:
#
#   exec.LookPath(envOrDefault("MULTICA_CLAUDE_PATH", "claude"))
#
# The neutral sandbox image bakes no MULTICA_CLAUDE_PATH; the Fleet provider
# supplies the fixed path at container start. Pass that path with --path so the
# probe runs the same resolution as the sandbox user. When --path is omitted the
# probe falls back to the image's own MULTICA_CLAUDE_PATH (legacy fixtures) and
# then to a PATH lookup for "claude".
#
# A launcher that exists inside the image but is not reachable from the daemon's
# PATH is a real shipped defect: the sandbox container exited 1 with
#   managed mode requires a claude executable: exec: "claude": executable file
#   not found in $PATH
# while the content verifier stayed green because it only proved the shim
# existed and ran as root. This check runs the same resolution as the sandbox
# user and requires the resolved executable to report a version.
#
# Usage:
#   scripts/verify-aurora-sandbox-managed-agent.sh [--user USER] [--path PATH] <image>
#
# The image must already be present locally; this script never pulls.

set -euo pipefail

user="10001:10001"
path=""
image=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --user)
      [ "$#" -ge 2 ] || { printf 'verify-aurora-sandbox-managed-agent: --user needs a value\n' >&2; exit 2; }
      user="$2"
      shift 2
      ;;
    --user=*)
      user="${1#--user=}"
      shift
      ;;
    --path)
      [ "$#" -ge 2 ] || { printf 'verify-aurora-sandbox-managed-agent: --path needs a value\n' >&2; exit 2; }
      path="$2"
      shift 2
      ;;
    --path=*)
      path="${1#--path=}"
      shift
      ;;
    -h|--help)
      printf 'usage: %s [--user USER] [--path PATH] <image>\n' "$(basename "$0")"
      exit 0
      ;;
    --)
      shift
      while [ "$#" -gt 0 ]; do image="$1"; shift; done
      ;;
    -*)
      printf 'verify-aurora-sandbox-managed-agent: unknown option %s\n' "$1" >&2
      exit 2
      ;;
    *)
      image="$1"
      shift
      ;;
  esac
done

fail() { printf 'verify-aurora-sandbox-managed-agent: FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'verify-aurora-sandbox-managed-agent: ok: %s\n' "$*"; }

[ -n "$image" ] || fail "usage: $(basename "$0") [--user USER] [--path PATH] <image>"
command -v docker >/dev/null 2>&1 || fail "docker is not on PATH"
docker image inspect "$image" >/dev/null 2>&1 || fail "image '$image' is not present locally (this check never pulls)"

# Resolve the agent command the Fleet supplies. --path is authoritative; when it
# is omitted, fall back to the image's own MULTICA_CLAUDE_PATH (legacy fixtures)
# and finally to a PATH lookup for "claude".
if [ -n "$path" ]; then
  agent_cmd="$path"
else
  agent_cmd="$(docker run --rm --entrypoint /bin/sh "$image" -c 'printf "%s" "${MULTICA_CLAUDE_PATH:-claude}"')"
fi

# Run the same resolution as the sandbox user: an absolute path is executed
# directly, a bare command is looked up on the daemon's PATH.
probe='
set -u
cmd="${MULTICA_AGENT_PATH:-claude}"
case "$cmd" in
  */*) path="$cmd" ;;
  *) path="$(command -v "$cmd" 2>/dev/null || true)" ;;
esac
if [ -z "$path" ]; then
  printf "UNRESOLVED cmd=%s\n" "$cmd"
  exit 1
fi
if [ ! -x "$path" ]; then
  printf "NOT_EXECUTABLE cmd=%s path=%s\n" "$cmd" "$path"
  exit 1
fi
if command -v timeout >/dev/null 2>&1; then
  out="$(HOME=/tmp timeout 60 "$path" --version 2>&1)"; rc=$?
else
  out="$(HOME=/tmp "$path" --version 2>&1)"; rc=$?
fi
if [ "$rc" -ne 0 ]; then
  printf "NOT_RUNNABLE cmd=%s path=%s rc=%s out=%s\n" "$cmd" "$path" "$rc" "$(printf "%s" "$out" | head -n1)"
  exit 1
fi
version="$(printf "%s\n" "$out" | head -n1)"
if [ -z "$version" ]; then
  printf "NO_VERSION cmd=%s path=%s\n" "$cmd" "$path"
  exit 1
fi
printf "MANAGED_AGENT %s -> %s [%s]\n" "$cmd" "$path" "$version"
'

out="$(docker run --rm --user "$user" -e MULTICA_AGENT_PATH="$agent_cmd" --entrypoint /bin/sh "$image" -c "$probe" 2>&1)" || \
  fail "the managed daemon cannot resolve and run its agent executable as $user: $(printf '%s' "$out" | tail -n1)"
printf '%s\n' "$out" | grep -q '^MANAGED_AGENT ' || fail "managed-agent probe produced no resolution line: $out"
pass "$(printf '%s' "$out" | grep '^MANAGED_AGENT ' | head -n1)"
