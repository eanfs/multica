#!/usr/bin/env bash
# Exercises the node entrypoint without Docker: secrets live in a temporary
# directory and a fake multica binary records the argv and environment it got.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
entrypoint="$root/docker/runtime/entrypoint.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The image contract uses the fixed /run/secrets/runtime path. The override
# lets this test mount the same contract somewhere writable.
export MULTICA_RUNTIME_SECRETS_DIR="$tmp/secrets"
secrets="$MULTICA_RUNTIME_SECRETS_DIR"
home="$tmp/home"
mkdir -p "$secrets" "$home"
export HOME="$home"

bin="$tmp/bin"
mkdir -p "$bin"
cat > "$bin/multica" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" > "$HOME/ran"
env > "$HOME/node-env"
EOF
chmod +x "$bin/multica"
export PATH="$bin:$PATH"

fail() { echo "FAIL: $*" >&2; exit 1; }

file_mode() {
  # GNU stat first (Linux), then BSD stat (macOS).
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

# 1. No secrets at all: refuse to start and say why.
if "$entrypoint" >"$tmp/out" 2>"$tmp/err"; then
  fail "entrypoint without secrets must exit non-zero"
fi
grep -q 'missing' "$tmp/err" || fail "expected a missing-secret diagnostic"

# 2. Only one of the two secrets files is present: still refuse.
printf '{"server_url":"http://api:8080","token":"mcn_test"}' > "$secrets/config.json"
if "$entrypoint" >/dev/null 2>"$tmp/err-partial"; then
  fail "entrypoint without env must exit non-zero"
fi
grep -q 'missing' "$tmp/err-partial" || fail "expected a missing-secret diagnostic for env"
rm -f "$secrets/config.json"

# 3. Complete secrets: install the CLI config, export only model credentials,
#    then exec the daemon. The '=' inside ANTHROPIC_BASE_URL proves values keep
#    everything after the first separator.
printf '{"server_url":"http://api:8080","token":"mcn_test","workspace_id":"w1","workspaces_root":"/home/multica/workspaces"}' > "$secrets/config.json"
printf 'ANTHROPIC_API_KEY=sk-test\nANTHROPIC_BASE_URL=http://gw.example/v1?a=b\nMULTICA_TOKEN=must-not-export\n' > "$secrets/env"
chmod 600 "$secrets/config.json" "$secrets/env"

"$entrypoint" >/dev/null 2>"$tmp/err-ok"

[ "$(cat "$home/.multica/config.json")" = "$(cat "$secrets/config.json")" ] || fail "config not installed"
[ "$(file_mode "$home/.multica/config.json")" = "600" ] || fail "config mode must be 600, got $(file_mode "$home/.multica/config.json")"
[ "$(cat "$home/ran")" = "daemon start --foreground" ] || fail "daemon not started: $(cat "$home/ran" 2>/dev/null || echo missing)"
grep -Fxq 'ANTHROPIC_API_KEY=sk-test' "$home/node-env" || fail "model key must be exported"
grep -Fxq 'ANTHROPIC_BASE_URL=http://gw.example/v1?a=b' "$home/node-env" || fail "value after '=' must be preserved"
if grep -q '^MULTICA_TOKEN=' "$home/node-env"; then
  fail "forbidden env must not be exported"
fi
if grep -q 'MULTICA_TOKEN' "$tmp/err-ok"; then
  fail "ignored variables must not be logged"
fi
if grep -q -e 'sk-test' -e 'mcn_test' "$tmp/err-ok"; then
  fail "secret values must not be logged"
fi

echo "ok"
