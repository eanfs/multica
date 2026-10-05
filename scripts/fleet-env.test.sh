#!/usr/bin/env bash
# Hermetic environment regressions. No live Docker, SQL, HTTP or process access.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
# Retain fixtures when requested so the controller can audit exact inputs/logs.
trap '[ "${MULTICA_KEEP_TEST_FIXTURES:-0}" = 1 ] || rm -rf "$tmp_dir"' EXIT
export HOME="$tmp_dir/home" MULTICA_DEV_HOME="$tmp_dir/registry" MULTICA_DEV_TMPDIR="$tmp_dir/tmp"
export MULTICA_DEV_PROFILES_HOME="$tmp_dir/profiles" MULTICA_DEV_WORKSPACES_PARENT="$tmp_dir/workspaces"
export MULTICA_DEV_DESKTOP_APP_DATA="$tmp_dir/appdata" FIXTURE_MISSES="$tmp_dir/misses"
mkdir -p "$HOME" "$tmp_dir/bin"
for tool in docker curl psql go make pnpm lsof ps; do
  printf '#!/usr/bin/env bash
echo %s >> "$FIXTURE_MISSES"
exit 97
' "$tool" > "$tmp_dir/bin/$tool"
  chmod +x "$tmp_dir/bin/$tool"
done
export PATH="$tmp_dir/bin:$PATH"
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
echo "Owned fixture tree: $tmp_dir"
# Break: validating each component only after stopping earlier components shuts
# down the API even when a later requested component is invalid.
status=0
(
  source "$root_dir/scripts/dev-env.sh"
  resolve_env_for_read() { NAME=fixture; BACKEND_PORT=18080; FRONTEND_PORT=13000; DATABASE_URL=fixture; DB_NAME=fixture; }
  stop_component() { printf '%s\n' "$1" >> "$tmp_dir/stops"; }
  cmd_down --components api,nope
) > "$tmp_dir/unknown-down.log" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail 'unknown component accepted'
[ ! -s "$tmp_dir/stops" ] || fail 'TestUnknownComponentDeniedBeforeAPIStop: API stopped before validation'
pass TestUnknownComponentDeniedBeforeAPIStop
[ ! -s "$FIXTURE_MISSES" ] || fail 'unmatched external fixture'
