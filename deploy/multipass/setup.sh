#!/usr/bin/env bash
# Create (or reuse) a Multipass VM that hosts the Docker daemon used by the
# local Fleet, then point the Docker CLI at it. Run once per machine:
#
#   bash deploy/multipass/setup.sh
#
# Requires Multipass (macOS: brew install --cask multipass) and its admin
# installer. The VM needs network access for the Docker apt repository.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
vm_name="${MULTICA_MULTIPASS_VM:-multica-docker}"
context_name="${MULTICA_MULTIPASS_CONTEXT:-multica-multipass}"
cloud_init="$repo_root/deploy/multipass/cloud-init-docker.yaml"

command -v multipass >/dev/null 2>&1 || {
  echo "Multipass is not installed. Install it first: brew install --cask multipass" >&2
  exit 1
}
command -v docker >/dev/null 2>&1 || { echo "docker CLI is required" >&2; exit 1; }

if ! multipass info "$vm_name" >/dev/null 2>&1; then
  multipass launch 24.04 --name "$vm_name" --cpus 4 --memory 8G --disk 60G --cloud-init "$cloud_init"
fi

echo "waiting for cloud-init in $vm_name ..."
multipass exec "$vm_name" -- cloud-init status --wait

ip="$(multipass info "$vm_name" --format json | python3 -c '
import json, sys
data = json.load(sys.stdin)["info"]
name = next(iter(data))
print(data[name]["ipv4"][0])
')"
echo "Multipass VM $vm_name at $ip"

# Docker context over SSH needs the host public key in the VM. Multipass gives
# us passwordless exec, so install the key here instead of relying on a shared
# SSH configuration.
key="$HOME/.ssh/id_ed25519"
if [ ! -f "$key" ]; then
  ssh-keygen -t ed25519 -N "" -f "$key" >/dev/null
fi
multipass transfer "$key.pub" "$vm_name:/tmp/multica-docker-key.pub"
multipass exec "$vm_name" -- bash -c '
  mkdir -p "$HOME/.ssh"
  chmod 700 "$HOME/.ssh"
  cat /tmp/multica-docker-key.pub >> "$HOME/.ssh/authorized_keys"
  chmod 600 "$HOME/.ssh/authorized_keys"
  rm -f /tmp/multica-docker-key.pub
'
ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes "ubuntu@$ip" true

if docker context inspect "$context_name" >/dev/null 2>&1; then
  docker context rm -f "$context_name" >/dev/null
fi
docker context create "$context_name" --docker "host=ssh://ubuntu@$ip"
docker context use "$context_name"
docker info >/dev/null

cat <<EOF
Docker context "$context_name" is active (daemon in Multipass VM "$vm_name").

Next:
  make worktree-env
  make up C=api,web,fleet

Fleet derives the container-to-host API address from the Multipass default
gateway (192.168.64.1). Override with MULTICA_FLEET_HOST_GATEWAY if your
Multipass network differs. Switch back with: docker context use colima
EOF
