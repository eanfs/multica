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
printf 'ANTHROPIC_API_KEY=sk-test\nANTHROPIC_AUTH_TOKEN=auth-test\nANTHROPIC_BASE_URL=http://gw.example/v1?a=b\nANTHROPIC_MODEL=claude-sonnet-4-5\nCLAUDE_CODE_USE_BEDROCK=0\nAPI_TIMEOUT_MS=60000\nMULTICA_TOKEN=must-not-export\n' > "$secrets/env"
# Keep the source config world-readable: install must force 0600, not copy the
# mode of a hand-edited secrets file.
chmod 644 "$secrets/config.json"
chmod 600 "$secrets/env"

"$entrypoint" >/dev/null 2>"$tmp/err-ok"

[ "$(cat "$home/.multica/config.json")" = "$(cat "$secrets/config.json")" ] || fail "config not installed"
[ "$(file_mode "$home/.multica/config.json")" = "600" ] || fail "config mode must be 600, got $(file_mode "$home/.multica/config.json")"
[ "$(cat "$home/ran")" = "daemon start --foreground" ] || fail "daemon not started: $(cat "$home/ran" 2>/dev/null || echo missing)"
for kv in \
  'ANTHROPIC_API_KEY=sk-test' \
  'ANTHROPIC_AUTH_TOKEN=auth-test' \
  'ANTHROPIC_BASE_URL=http://gw.example/v1?a=b' \
  'ANTHROPIC_MODEL=claude-sonnet-4-5' \
  'CLAUDE_CODE_USE_BEDROCK=0' \
  'API_TIMEOUT_MS=60000'; do
  grep -Fxq "$kv" "$home/node-env" || fail "allowlisted env must be exported: $kv"
done
if grep -q '^MULTICA_TOKEN=' "$home/node-env"; then
  fail "forbidden env must not be exported"
fi
if grep -q 'must-not-export' "$home/node-env"; then
  fail "forbidden value must not be exported"
fi
if grep -q -e 'MULTICA_TOKEN' -e 'must-not-export' "$tmp/err-ok"; then
  fail "ignored variables and their values must not be logged"
fi
if grep -q -e 'sk-test' -e 'auth-test' -e 'mcn_test' "$tmp/err-ok"; then
  fail "secret values must not be logged"
fi

# 4. Image contract: node:22-bookworm-slim already provides uid/gid 1000, so a
#    second groupadd/useradd would fail at build time. No model credential may
#    be baked into an image ENV layer either; the entrypoint receives them from
#    the secrets volume only.
dockerfile="$root/docker/runtime/Dockerfile"
if grep -qE '^[[:space:]]*(RUN[[:space:]]+)?(groupadd|useradd)([[:space:]]|$)' "$dockerfile"; then
  fail "image must reuse the 1000:1000 identity from the base image"
fi
grep -qx 'USER 1000:1000' "$dockerfile" || fail "image must run as 1000:1000"
grep -q 'chown -R 1000:1000 /home/multica' "$dockerfile" || fail "data volume paths must belong to 1000:1000"
grep -qx 'ENV HOME=/home/multica' "$dockerfile" || fail "HOME must point at the data volume"
if grep -q 'ANTHROPIC_' "$dockerfile"; then
  fail "model credentials must not be baked into the image"
fi

echo "ok"
