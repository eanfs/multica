# Self-Hosting Setup (for AI Agents)

This document is designed for AI agents to execute. Follow these steps exactly to deploy a local Multica instance and connect to it.

## Prerequisites

- Docker and Docker Compose installed
- Homebrew installed (for CLI)
- At least one AI agent CLI on PATH: `claude` or `codex`

## Install

```bash
# Install CLI + provision self-host server
curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --with-server

# Configure CLI for localhost, authenticate, and start daemon
multica setup self-host
```

Wait for the server output `✓ Multica server is running and CLI is ready!` before running `multica setup self-host`.

**Expected result:**
- Frontend at http://localhost:3000
- Backend at http://localhost:8080
- `multica` CLI installed and configured for localhost

## Alternative: Manual Setup

```bash
git clone https://github.com/multica-ai/multica.git
cd multica
make selfhost
brew install multica-ai/tap/multica
multica setup self-host
```

The `multica setup self-host` command will:
1. Configure CLI to connect to localhost:8080 / localhost:3000
2. Open a browser for login — use the emailed code, or the generated code printed in backend logs when Resend is unset
3. Discover workspaces automatically
4. Start the daemon in the background

## Verification

```bash
multica daemon status
```

Should show `running` with detected agents.

## Stopping

```bash
# Stop the daemon
multica daemon stop

# Stop all Docker services
cd multica
make selfhost-stop
```

## Custom Ports

If the default ports (8080/3000) are in use:

1. Edit `.env` and change `PORT` and `FRONTEND_PORT`. These are host ports; the
   containers keep listening on 8080/3000 internally, so no rebuild is needed.
2. Run `make selfhost`
3. Run `multica setup self-host --port <PORT> --frontend-port <FRONTEND_PORT>`

Edit the file rather than relying on environment variables: `make` `include`s
`.env`, so a value in the file outranks the same variable from your shell
(`PORT=9100 make selfhost` is ignored when `.env` sets `PORT`). A command-line
assignment does take effect (`make selfhost PORT=9100`), and Docker Compose
invoked directly is the reverse again — there the environment outranks `.env`.

`BACKEND_PORT` is an optional alias that overrides `PORT` for the backend;
`API_PORT` and `SERVER_PORT` follow in that order. Whichever route you use, the
startup output is read back from Docker Compose, so the address it prints is the
one the stack is actually published on.

## Optional: Local Docker Cloud Runtime

The self-host stack does not include the Fleet control plane. Use it only when
you want Multica to provision Docker node containers that already run the
daemon and Claude Code:

The self-host Compose file deliberately ships no `fleet` service. A Compose
profile cannot hand this checkout's `MULTICA_LOCAL_FLEET_URL`,
`MULTICA_LOCAL_FLEET_SECRET_FILE` and `MULTICA_LOCAL_FLEET_NAMESPACE` to the
self-host backend, and node containers cannot resolve the Compose service name
`backend`; the removed profile could only start a control plane the API ignored.

1. Start it from a repository checkout, not a published image:

   ```bash
   make up C=api,web,fleet
   ```

   The first run builds `multica/fleet:dev` and
   `multica/runtime-claude:dev` and can take several minutes. Fleet publishes
   its port on loopback and joins this checkout's `multica_default` network.

2. Map at least one user to a credential profile in
   `.dev/fleet-credentials.json` (0600, git-ignored). The file is generated
   as an empty template; add the owner UUID, a profile name and that profile's
   model environment. The accepted variables and the exact shape are in
   [CLI_AND_DAEMON.md](CLI_AND_DAEMON.md#local-docker-cloud-runtime). No real
   key belongs in this document, `.env`, a Compose file, or Git.

3. Sign in to the workspace UI, open **Runtimes**, choose **Add runtime** and
   then **Cloud Runtime**. Only the provider's allowed spec is offered.

4. Stop the control plane with `make down` (keeps volumes and leaves node
   containers running) or clean this namespace up with `make destroy`
   (removes the namespace's containers and volumes before the database).

Requirements and limits:

- Docker Desktop or a Linux Docker daemon must be running before
  `make up C=fleet`.
- The node containers have no Docker socket, no privileged mode, no host
  network/PID and no published ports. They are not a production sandbox.
- On a plain Linux daemon, make sure `MULTICA_FLEET_API_URL` is reachable from
  the node network (the Docker network gateway usually works; Docker Desktop
  provides `host.docker.internal`).
- The E2E and Docker integration suites are opt-in and skip without their
  environment variables. They are not part of a normal self-host install.

## Troubleshooting

- **Backend not ready:** `docker compose -f docker-compose.selfhost.yml logs backend`
- **Frontend not ready:** `docker compose -f docker-compose.selfhost.yml logs frontend`
- **Daemon issues:** `multica daemon logs`
- **Health checks:** `curl http://localhost:8080/health` for liveness, `curl http://localhost:8080/readyz` for dependency-aware readiness
