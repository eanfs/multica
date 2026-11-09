#!/usr/bin/env bash
# Node entrypoint: install the Multica CLI config from the read-only secrets
# volume, export only the model credential allowlist, then replace this shell
# with the foreground daemon so tini forwards SIGTERM to a single process.
set -euo pipefail

# The image contract is the fixed /run/secrets/runtime path. The override exists
# so the entrypoint can be exercised on a host without that mount.
secrets_dir="${MULTICA_RUNTIME_SECRETS_DIR:-/run/secrets/runtime}"
config_src="$secrets_dir/config.json"
env_src="$secrets_dir/env"

if [ -z "${HOME:-}" ]; then
  echo "HOME must be set for the node CLI config" >&2
  exit 1
fi

# Fleet writes both files before the node starts, so a missing or empty file
# means this container was launched without its bootstrap.
if [ ! -s "$config_src" ] || [ ! -s "$env_src" ]; then
  echo "missing or empty node secrets under $secrets_dir" >&2
  exit 1
fi

install -d -m 0700 "$HOME/.multica"
install -m 0600 "$config_src" "$HOME/.multica/config.json"

# Only model credentials may reach the model process. Fleet validates credential
# profiles, but this allowlist also keeps a hand-edited secrets file from
# smuggling platform values (node token, HOME, PATH) into the daemon.
allowed_model_env() {
  case "$1" in
    ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_BASE_URL|ANTHROPIC_MODEL|CLAUDE_CODE_USE_BEDROCK|API_TIMEOUT_MS) return 0 ;;
    *) return 1 ;;
  esac
}

ignored=0
malformed=0
# Tolerate a final line without a newline: read fails at EOF but still assigns.
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    ""|"#"*) continue ;;
  esac
  key="${line%%=*}"
  if [ -z "$key" ] || [ "$key" = "$line" ]; then
    malformed=$((malformed + 1))
    continue
  fi
  value="${line#*=}"
  if allowed_model_env "$key"; then
    export "$key=$value"
  else
    ignored=$((ignored + 1))
  fi
done < "$env_src"

# Names and values stay out of logs on purpose: the secrets volume is private.
if [ "$ignored" -gt 0 ]; then
  echo "ignored $ignored non-model env entries" >&2
fi
if [ "$malformed" -gt 0 ]; then
  echo "ignored $malformed malformed env entries" >&2
fi

if ! command -v multica >/dev/null 2>&1; then
  echo "multica CLI not found on PATH" >&2
  exit 1
fi

exec multica daemon start --foreground
