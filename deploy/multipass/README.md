# Multipass Docker backend

Run the local Docker daemon (and therefore Fleet plus its node containers) in a
Multipass VM instead of Docker Desktop or Colima.

## Architecture

```
macOS host                         Multipass VM (Ubuntu 24.04)
  API :<port>  <── 192.168.64.1 ──  Docker daemon
  docker CLI ── SSH context ──────>  ├─ fleet container (socket mounted)
                                     ├─ node containers (daemon + Claude Code)
                                     └─ PostgreSQL / shared network
```

- The VM only runs Docker. The API stays on the host, so `make up` keeps
  managing it as usual and Fleet containers call it at
  `http://192.168.64.1:<port>`.
- `192.168.64.0/24` is Multipass' default network; the host side (DHCP/DNS) is
  `192.168.64.1`. Override with `MULTICA_FLEET_HOST_GATEWAY` for a custom
  network.
- Docker reaches the VM daemon through an SSH Docker context. Fleet's node
  containers are created inside the VM's Docker, so the mounted
  `/var/run/docker.sock` is the VM socket (never the host's).

## Prerequisites

- Multipass (macOS: `brew install --cask multipass`; the installer needs
  administrator privileges).
- Docker CLI on the host (already required by the repository).

## One-time setup

```bash
bash deploy/multipass/setup.sh
```

The script launches a 4 CPU / 8 GB / 60 GB VM named `multica-docker`, waits for
cloud-init (Docker CE + buildx + compose plugin), installs the host SSH key for
the Docker context, and creates/activates the `multica-multipass` context.

## Daily use

```bash
docker context use multica-multipass
make worktree-env
make up C=api,web,fleet
```

Fleet start derives the API address from the active Docker context; no extra
variables are needed. Switch back to Colima with `docker context use colima`.

## Operations

- Inspect: `multipass info multica-docker`, `docker context inspect multica-multipass`.
- Stop the VM: `multipass stop multica-docker` (Fleet and nodes stop with it;
  PostgreSQL data stays on the VM disk).
- Remove the VM: `multipass delete multica-docker && multipass purge`
  (run `make destroy` first while Docker is reachable so Fleet-owned node
  resources are cleaned through the product path).
- Custom network: `MULTICA_FLEET_HOST_GATEWAY=<host-reachable-ip>`.
- VPN software that hijacks the `192.168.64.0/24` route will break both the
  Docker context and the node callback; the Multipass networking guide lists
  the known offenders.

## Security

The cloud-init recipe contains no secrets. API keys and node tokens are written
into the node containers' secrets volume by Fleet, exactly as with Colima and
Docker Desktop. The Multipass VM is a trusted control plane because its Docker
socket is mounted into the Fleet container.
