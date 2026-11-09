#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${1:-.env.worktree}"

if [ -f "$ENV_FILE" ] && [ "${FORCE:-0}" != "1" ]; then
  echo "Refusing to overwrite existing $ENV_FILE. Re-run with FORCE=1 if you want to regenerate it."
  exit 1
fi

worktree_name="${WORKTREE_NAME:-$(basename "$PWD")}"
slug="$(printf '%s' "$worktree_name" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/_/g; s/__*/_/g; s/^_//; s/_$//')"
if [ -z "$slug" ]; then
  slug="multica"
fi

hash_value="$(printf '%s' "$PWD" | cksum | awk '{print $1}')"
offset=$((hash_value % 1000))

postgres_db="multica_${slug}_${offset}"
postgres_port=5432
backend_port=$((18080 + offset))
frontend_port=$((13000 + offset))
frontend_origin="http://localhost:${frontend_port}"

# Fleet is an optional component (`make up C=fleet`). Its service secret and
# owner/profile map live next to the env file under .dev/, which .gitignore
# excludes, and are created with owner-only permissions. No key is written into
# the env file itself.
env_file_dir="$(cd "$(dirname "$ENV_FILE")" && pwd)"
fleet_secret_dir="$env_file_dir/.dev"
fleet_port=$((backend_port + 20))

cat > "$ENV_FILE" <<EOF
POSTGRES_DB=${postgres_db}
POSTGRES_USER=multica
POSTGRES_PASSWORD=multica
POSTGRES_PORT=${postgres_port}
DATABASE_URL=postgres://multica:multica@localhost:${postgres_port}/${postgres_db}?sslmode=disable

PORT=${backend_port}
JWT_SECRET=change-me-in-production
MULTICA_DEV_VERIFICATION_CODE=888888
MULTICA_SERVER_URL=ws://localhost:${backend_port}/ws
MULTICA_PUBLIC_URL=http://localhost:${backend_port}
MULTICA_APP_URL=${frontend_origin}

GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
GOOGLE_REDIRECT_URI=${frontend_origin}/auth/callback

FRONTEND_PORT=${frontend_port}
FRONTEND_ORIGIN=${frontend_origin}
NEXT_PUBLIC_API_URL=http://localhost:${backend_port}
NEXT_PUBLIC_WS_URL=ws://localhost:${backend_port}/ws
EOF

mkdir -p "$fleet_secret_dir"
if [ ! -f "$fleet_secret_dir/fleet-secret" ]; then
  ( umask 077; head -c 32 /dev/urandom | base64 > "$fleet_secret_dir/fleet-secret" )
fi
if [ ! -f "$fleet_secret_dir/fleet-credentials.json" ]; then
  ( umask 077; printf '{"users":{},"profiles":{}}\n' > "$fleet_secret_dir/fleet-credentials.json" )
fi
chmod 600 "$fleet_secret_dir/fleet-secret" "$fleet_secret_dir/fleet-credentials.json"

cat >> "$ENV_FILE" <<EOF

# Local Fleet control plane (optional: make up C=fleet). The files below are
# private: the service secret and the owner/profile map, both 0600.
# The listen address is deliberately absent: the compose service binds
# 0.0.0.0:8091 inside the container and nothing on the host consumes it.
MULTICA_FLEET_PORT=${fleet_port}
MULTICA_LOCAL_FLEET_URL=http://127.0.0.1:${fleet_port}
MULTICA_LOCAL_FLEET_SECRET_FILE=${fleet_secret_dir}/fleet-secret
MULTICA_LOCAL_FLEET_NAMESPACE=${slug}
MULTICA_FLEET_RUNTIME_IMAGE=multica/runtime-claude:dev
MULTICA_FLEET_CREDENTIALS_FILE=${fleet_secret_dir}/fleet-credentials.json
MULTICA_FLEET_API_URL=http://host.docker.internal:${backend_port}
EOF

echo "Generated $ENV_FILE for worktree '$worktree_name'"
echo "  Shared Postgres: localhost:${postgres_port}"
echo "  Database: ${postgres_db}"
echo "  Backend:  http://localhost:${backend_port}"
echo "  Frontend: ${frontend_origin}"
echo "  Fleet:    http://127.0.0.1:${fleet_port} (optional: make up C=fleet)"
echo "  Fleet secrets: ${fleet_secret_dir} (0600, git-ignored)"
echo ""
echo "Next steps:"
echo "  make setup-worktree"
echo "  make start-worktree"
