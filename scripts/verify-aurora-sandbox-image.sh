#!/usr/bin/env bash
# Verify the built Aurora sandbox and egress images against the locked inputs
# and the image contract in
# docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md (Task 3).
#
# Usage:
#   scripts/verify-aurora-sandbox-image.sh <sandbox-image> [<egress-image>]
#
# Each argument is any local image reference (tag or digest). The script never
# pulls: every image must already be present in the local Docker store. It
# inspects the configured user, entrypoint, health check and architecture, the
# per-architecture size, the required binaries and locked versions, the patched
# vendor tree, the absence of forbidden package-manager/download/SSH/Git
# binaries, root-owned writable directories, and any token/key pattern in
# files, config, labels, environment, or docker history --no-trunc. It runs an
# in-image self-test and exits non-zero naming the first unmet invariant.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"

if [ "$#" -lt 1 ]; then
  printf 'usage: %s <sandbox-image> [<egress-image>]\n' "$(basename "$0")" >&2
  exit 2
fi
sandbox_image="$1"
egress_image=""
if [ "$#" -ge 2 ]; then egress_image="$2"; fi

fail() { printf 'verify-aurora-sandbox-image: FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'verify-aurora-sandbox-image: ok: %s\n' "$*"; }

for tool in docker node tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool '$tool' is not on PATH"
done

max_image_bytes=$((4 * 1024 * 1024 * 1024))
sandbox_user="10001:10001"

versions_json="$repo_root/deploy/aurora-sandbox/versions.json"
apt_lock="$repo_root/deploy/aurora-sandbox/apt-packages.lock"
vendor_lock="$repo_root/deploy/aurora-sandbox/vendor/volcengine/vendor-lock.json"
vendor_source="$repo_root/deploy/aurora-sandbox/vendor/volcengine"
for f in "$versions_json" "$apt_lock" "$vendor_lock"; do
  [ -f "$f" ] || fail "missing locked input $f"
done

work="$(mktemp -d)"
# docker export/tar preserves the image's read-only modes, so make the tree
# writable before removing it; the EXIT trap must not emit permission errors.
cleanup() { chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Token/key patterns. These are matched against image files, Config.Env,
# Config.Labels, the full inspect JSON, and docker history --no-trunc.
# ---------------------------------------------------------------------------
cat >"$work/secret-patterns" <<'PATTERNS'
sk-ant-[A-Za-z0-9_-]{16,}
sk-proj-[A-Za-z0-9_-]{16,}
sk-[A-Za-z0-9]{40,}
AKIA[0-9A-Z]{16}
-----BEGIN [A-Z ]*PRIVATE KEY-----
ghp_[A-Za-z0-9]{30,}
github_pat_[A-Za-z0-9_]{20,}
xox[baprs]-[A-Za-z0-9-]{10,}
mse_[0-9a-f]{16,}
volc-[A-Za-z0-9]{20,}
PATTERNS

# File-content patterns. The config scan above stays deliberately broad, but
# third-party sources shipped in the runtime legitimately contain key-shaped
# bytes: a base64 blob can embed an AKIA run, and libraries like jose contain
# the literal PEM header string. Require a standalone AKIA token and a PEM
# header that fills its own line, so real credentials still match while those
# library false positives do not.
cat >"$work/secret-file-patterns" <<'PATTERNS'
sk-ant-[A-Za-z0-9_-]{16,}
sk-proj-[A-Za-z0-9_-]{16,}
sk-[A-Za-z0-9]{40,}
(^|[^A-Za-z0-9])AKIA[0-9A-Z]{16}([^A-Za-z0-9]|$)
^-----BEGIN [A-Z ]*PRIVATE KEY-----[[:space:]]*$
ghp_[A-Za-z0-9]{30,}
github_pat_[A-Za-z0-9_]{20,}
xox[baprs]-[A-Za-z0-9-]{10,}
mse_[0-9a-f]{16,}
volc-[A-Za-z0-9]{20,}
PATTERNS

scan_secrets() {
  # scan_secrets <label> <text-file>
  local label="$1" file="$2" hits
  hits="$(grep -E -n -f "$work/secret-patterns" "$file" 2>/dev/null || true)"
  [ -z "$hits" ] || fail "$label contains a token/key pattern:
$hits"
}

# resolve_image pins a local reference to its immutable image ID.
resolve_image() {
  local ref="$1"
  docker image inspect "$ref" >/dev/null 2>&1 || fail "image '$ref' is not present locally (the verifier never pulls)"
  docker image inspect --format '{{.Id}}' "$ref"
}

check_image_identity() {
  # check_image_identity <name> <ref> <expected-entrypoint-json>
  local name="$1" ref="$2" want_entrypoint="$3"
  local os arch size entrypoint user
  os="$(docker image inspect --format '{{.Os}}' "$ref")"
  arch="$(docker image inspect --format '{{.Architecture}}' "$ref")"
  [ "$os" = "linux" ] || fail "$name image OS is '$os', want linux"
  case "$arch" in
    amd64 | arm64) ;;
    *) fail "$name image architecture is '$arch', want amd64 or arm64" ;;
  esac
  pass "$name image runs linux/$arch"
  size="$(docker image inspect --format '{{.Size}}' "$ref")"
  if [ "$size" -ge "$max_image_bytes" ]; then
    fail "$name image size $size bytes is not under 4 GiB ($max_image_bytes)"
  fi
  pass "$name image size $size bytes is under 4 GiB"
  user="$(docker image inspect --format '{{.Config.User}}' "$ref")"
  [ "$user" = "$sandbox_user" ] || fail "$name image Config.User is '$user', want '$sandbox_user'"
  pass "$name image runs as UID/GID $sandbox_user"
  entrypoint="$(docker image inspect --format '{{json .Config.Entrypoint}}' "$ref")"
  [ "$entrypoint" = "$want_entrypoint" ] || fail "$name image Entrypoint is $entrypoint, want $want_entrypoint"
  pass "$name image entrypoint is fixed"
}

check_no_exported_secrets() {
  # check_no_exported_secrets <name> <ref>
  local name="$1" ref="$2"
  docker image inspect --format '{{json .Config.Env}}' "$ref" >"$work/$name-env.json"
  docker image inspect --format '{{json .Config.Labels}}' "$ref" >"$work/$name-labels.json"
  docker image inspect --format '{{json .Config.Entrypoint}}' "$ref" >"$work/$name-entrypoint.json"
  docker image inspect --format '{{json .Config.Cmd}}' "$ref" >"$work/$name-cmd.json"
  scan_secrets "$name Config.Env" "$work/$name-env.json"
  scan_secrets "$name Config.Labels" "$work/$name-labels.json"
  scan_secrets "$name Entrypoint/Cmd" "$work/$name-entrypoint.json"
  scan_secrets "$name Entrypoint/Cmd" "$work/$name-cmd.json"
  docker history --no-trunc --format '{{.CreatedBy}}' "$ref" >"$work/$name-history.txt"
  scan_secrets "$name image history" "$work/$name-history.txt"
  pass "$name config, labels, environment and history carry no token/key pattern"
}

# ===========================================================================
# Sandbox image
# ===========================================================================
sandbox_id="$(resolve_image "$sandbox_image")"
sandbox="multica-aurora-sandbox-verify"
docker tag "$sandbox_id" "$sandbox" >/dev/null
check_image_identity "sandbox" "$sandbox" '["/usr/local/bin/fleet-node","run"]'

healthcheck="$(docker image inspect --format '{{json .Config.Healthcheck}}' "$sandbox")"
case "$healthcheck" in
  *fleet-node*health*) ;;
  *) fail "sandbox image health check is $healthcheck, want the fixed non-shell fleet-node health command" ;;
esac
volumes="$(docker image inspect --format '{{json .Config.Volumes}}' "$sandbox")"
case "$volumes" in
  ""|"null"|"{}") ;;
  *) fail "sandbox image declares volumes: $volumes" ;;
esac
pass "sandbox image health check is fixed and declares no volume"

check_no_exported_secrets "sandbox" "$sandbox"

# --- locked versions table, derived from the lock files ---------------------
export VERIFY_ARCH="$(docker image inspect --format '{{.Architecture}}' "$sandbox")"
node - "$versions_json" "$apt_lock" >"$work/version-expectations.tsv" <<'NODE'
const fs = require('node:fs');
const [versionsPath, aptPath] = process.argv.slice(2);
const versions = JSON.parse(fs.readFileSync(versionsPath, 'utf8'));
const apt = JSON.parse(fs.readFileSync(aptPath, 'utf8'));
const arch = process.env.VERIFY_ARCH || 'arm64';
function aptVersion(pkg) {
  const entry = apt.packages[pkg];
  if (!entry) throw new Error('missing apt package ' + pkg);
  const perArch = entry[arch] || entry.amd64;
  return perArch.version;
}
function upstream(version) {
  const noEpoch = version.includes(':') ? version.slice(version.indexOf(':') + 1) : version;
  return noEpoch.split('-')[0].split('+')[0];
}
const nodeMajor = versions.node.runtime_image.match(/node:(\d+)/);
const rows = [
  ['node', 'node --version', nodeMajor ? 'v' + nodeMajor[1] + '.' : 'v22.'],
  ['ffmpeg', 'ffmpeg -version', upstream(aptVersion('ffmpeg'))],
  ['chromium', 'chromium --version', upstream(aptVersion('chromium'))],
  ['pdfinfo', 'pdfinfo -v', upstream(aptVersion('poppler-utils'))],
  ['convert', 'convert -version', upstream(aptVersion('imagemagick')).split('.').slice(0, 3).join('.')],
  ['unzip', 'unzip -v', upstream(aptVersion('unzip'))],
  ['tini', 'tini --version', upstream(aptVersion('tini'))],
];
for (const [name, probe, expected] of rows) {
  process.stdout.write([name, probe, expected].join('\t') + '\n');
}
for (const [pkg, version] of Object.entries(versions.runtime_packages)) {
  process.stdout.write(['pkg:' + pkg, version, version].join('\t') + '\n');
}
NODE

# --- in-container content inspection ---------------------------------------
cat >"$work/sandbox-inspect.sh" <<'INSPECT'
set -eu
fail() { echo "INSPECT_FAIL: $*"; exit 1; }

missing=""
for b in /usr/local/bin/multica /usr/local/bin/fleet-node /usr/local/bin/node /usr/bin/ffmpeg /usr/bin/ffprobe \
         /usr/bin/chromium /usr/bin/convert /usr/bin/pdftoppm /usr/bin/pdfinfo \
         /usr/bin/unzip /usr/bin/tini; do
  [ -x "$b" ] || missing="$missing $b"
done
[ -z "$missing" ] || fail "missing required binaries:$missing"
echo "BINARIES ok"

hyperframes="$(find /opt/aurora/runtime/node_modules -maxdepth 3 -path '*/.bin/hyperframes' -print -quit 2>/dev/null || true)"
[ -n "$hyperframes" ] || fail "missing hyperframes CLI under node_modules/.bin"
# @anthropic-ai/claude-code 2.1.282 no longer ships a cli.* launcher. Its
# bin/claude.exe is a text stub that the package postinstall replaces with the
# platform-native binary from @anthropic-ai/claude-code-linux-{arm64,x64}; the
# image runs that postinstall in the nodedeps stage. Require the real binary,
# the pnpm shim that the runtime puts on PATH, and a runnable --version probe.
claude_pkg=/opt/aurora/runtime/node_modules/@anthropic-ai/claude-code
claude_bin="$claude_pkg/bin/claude.exe"
[ -x "$claude_bin" ] || fail "missing Claude Code CLI launcher $claude_bin"
claude_bytes="$(wc -c <"$claude_bin" 2>/dev/null || echo 0)"
[ "$claude_bytes" -gt 1048576 ] || fail "Claude Code CLI is the ${claude_bytes}-byte install stub, want the platform-native binary"
claude_shim=/opt/aurora/runtime/node_modules/.bin/claude
[ -x "$claude_shim" ] || fail "missing executable claude shim $claude_shim"
claude_version="$(HOME=/tmp timeout 30 "$claude_shim" --version 2>&1)" || fail "Claude Code CLI is not runnable: $claude_version"
claude_version="$(printf '%s\n' "$claude_version" | head -n1)"
[ -n "$claude_version" ] || fail "Claude Code CLI did not report a version"
echo "CLAUDE $claude_version"
[ -x /usr/local/bin/multica ] || fail "missing multica daemon"
echo "CLIS ok"

# The fixed Fleet node layout: /data, /data/home, /data/workspaces and /secrets
# exist and are owned by the non-root node, so a fresh named volume mounted there
# is writable by UID 10001 and the read-only secrets volume is readable by it.
for d in /data /data/home /data/workspaces /secrets; do
  [ -d "$d" ] || fail "missing fixed Fleet layout directory $d"
  owner="$(stat -c '%u:%g' "$d")"
  [ "$owner" = "10001:10001" ] || fail "Fleet layout directory $d is owned by $owner, want 10001:10001"
done
echo "LAYOUT ok"

# fleet-node is runnable and fails closed on an empty command; a missing or
# mistyped binary must not be able to masquerade as a healthy node.
set +e
fleet_out="$(/usr/local/bin/fleet-node 2>&1)"
fleet_rc=$?
set -e
[ "$fleet_rc" -ne 0 ] || fail "fleet-node accepted an empty command"
case "$fleet_out" in
  *"fleet-node command failed"*) ;;
  *) fail "fleet-node did not report its fixed rejection: $fleet_out" ;;
esac
echo "FLEET_NODE ok"

# No fixed enrollment secret is baked in: the Fleet installs the one-time mse_
# secret into the mounted secrets volume at runtime.
baked="$(find / -xdev -name 'aurora-enrollment' -print -quit 2>/dev/null || true)"
[ -z "$baked" ] || fail "image bakes a fixed enrollment secret: $baked"
echo "NO_BAKED_ENROLLMENT ok"

forbidden=""
for b in npm npx pnpm pnpx corepack yarn git curl wget ssh scp sftp \
         apt apt-get dpkg python python3 gcc cc make go; do
  p="$(command -v "$b" 2>/dev/null || true)"
  [ -z "$p" ] || forbidden="$forbidden $b=$p"
done
[ -z "$forbidden" ] || fail "forbidden binaries present:$forbidden"
echo "FORBIDDEN ok"

writable="$(find / -xdev \( -path /proc -o -path /sys -o -path /dev -o -path /tmp -o -path /run -o -path /var/tmp -o -path /workspace \) -prune -o -type d -user 0 -perm /002 -print 2>/dev/null || true)"
[ -z "$writable" ] || fail "other-writable root-owned directories:
$writable"
writable_group="$(find / -xdev \( -path /proc -o -path /sys -o -path /dev -o -path /tmp -o -path /run -o -path /var/tmp -o -path /workspace \) -prune -o -type d -user 0 -group 10001 -perm /020 -print 2>/dev/null || true)"
[ -z "$writable_group" ] || fail "group-writable root-owned directories for gid 10001:
$writable_group"
echo "WRITABLE ok"

residue=""
for p in /root/.npm /root/.cache /root/.pnpm-store /root/.local/share/pnpm \
         /opt/aurora/runtime/.npmrc \
         /opt/aurora/vendor/volcengine/patches \
         /opt/aurora/runtime/deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedance-skill; do
  [ -e "$p" ] && residue="$residue $p" || true
done
gitdirs="$(find / -xdev -type d -name .git -print 2>/dev/null || true)"
[ -z "$gitdirs" ] || residue="$residue $gitdirs"
[ -z "$residue" ] || fail "forbidden build residue:$residue"
echo "RESIDUE ok"

generated="$(find /workspace /opt/aurora -xdev -name 'aurora-artifacts.v1.json' -print 2>/dev/null || true)"
[ -z "$generated" ] || fail "generated artifact manifest present:$generated"
if [ -e /run/secrets ]; then fail "image contains /run/secrets"; fi
echo "GENERATED ok"

for pkg in @anthropic-ai/claude-code @modelcontextprotocol/sdk hyperframes openai; do
  v="$(node -e "process.stdout.write(require('/opt/aurora/runtime/node_modules/$pkg/package.json').version)" 2>/dev/null || echo MISSING)"
  echo "PKG $pkg $v"
done

echo "PROBE node $(node --version)"
echo "PROBE ffmpeg $(ffmpeg -version 2>&1 | head -n1)"
echo "PROBE chromium $(chromium --version 2>&1 | head -n1)"
echo "PROBE pdfinfo $(pdfinfo -v 2>&1 | head -n1)"
echo "PROBE convert $(convert -version 2>&1 | head -n1)"
echo "PROBE unzip $(unzip -v 2>&1 | head -n1)"
echo "PROBE tini $(tini --version 2>&1 | head -n1)"

stored="$(cat /opt/aurora/vendor-tree.sha256 2>/dev/null || echo MISSING)"
recomputed="$(find /opt/aurora/vendor -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}')"
[ "$stored" = "$recomputed" ] || fail "vendor-tree.sha256 stored=$stored recomputed=$recomputed"
echo "VENDOR_TREE_HASH $stored"

node -e "const {createRequire}=require('node:module');const r=createRequire('/opt/aurora/vendor/volcengine/');const m=r('/opt/aurora/vendor/volcengine/byted-ark-seedream-skill/scripts/seedream-broker.js');if(typeof m.generate!=='function'){console.error('seedream-broker generate missing');process.exit(3);}console.log('SELFTEST seedream-broker ok');"
node -e "const {createRequire}=require('node:module');const r=createRequire('/opt/aurora/vendor/volcengine/');const m=r('/opt/aurora/vendor/volcengine/byted-ark-seedance-skill/scripts/seedance-broker.js');if(typeof m.createTask!=='function'||typeof m.pollTask!=='function'){console.error('seedance-broker createTask/pollTask missing');process.exit(3);}console.log('SELFTEST seedance-broker ok');"
node --input-type=module -e "const m=await import('/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src/server.mjs');if(!Array.isArray(m.TOOL_NAMES)||m.TOOL_NAMES.length!==9){console.error('broker tool count',m.TOOL_NAMES&&m.TOOL_NAMES.length);process.exit(4);}console.log('SELFTEST broker tools='+m.TOOL_NAMES.length);"
echo "SELFTEST ok"
INSPECT

inspect_out="$(docker run --rm -i --user 0:0 --entrypoint /bin/sh "$sandbox" - <"$work/sandbox-inspect.sh")" || fail "sandbox in-image inspection failed:
$inspect_out"
printf '%s\n' "$inspect_out" >"$work/sandbox-inspect.out"
printf '%s\n' "$inspect_out" | grep -q '^SELFTEST ok$' || fail "sandbox in-image self-test did not complete"
pass "sandbox required binaries, forbidden-binary boundary, writable-directory boundary and build residue are clean"
pass "sandbox in-image self-test passed"

# Prove the daemon can actually resolve the agent launcher from its own
# environment: the in-image check above proves the shim exists and runs as root,
# but the daemon resolves exec.LookPath(envOrDefault("MULTICA_CLAUDE_PATH",
# "claude")) as the sandbox user. That gap is why a container could exit 1 with
# "managed mode requires a claude executable" while this verifier stayed green.
managed_out="$("$script_dir/verify-aurora-sandbox-managed-agent.sh" --user "$sandbox_user" "$sandbox")" \
  || fail "sandbox managed-agent resolution: $(printf '%s' "$managed_out" | tail -n1)"
printf '%s\n' "$managed_out"
pass "sandbox daemon resolves and runs its managed agent executable"

# Token/key patterns over every regular, non-binary file in the image rootfs.
sandbox_cid="$(docker create "$sandbox")"
docker export "$sandbox_cid" >"$work/sandbox-rootfs.tar"
docker rm "$sandbox_cid" >/dev/null
mkdir -p "$work/sandbox-rootfs"
tar -xf "$work/sandbox-rootfs.tar" -C "$work/sandbox-rootfs"
secret_files="$(grep -r -I -E -l -f "$work/secret-file-patterns" "$work/sandbox-rootfs" 2>/dev/null || true)"
if [ -n "$secret_files" ]; then
  fail "sandbox rootfs files contain a token/key pattern:
$secret_files"
fi
pass "sandbox rootfs files carry no token/key pattern"

# Locked binary and node package versions.
while IFS="$(printf '\t')" read -r name probe expected; do
  [ -n "$name" ] || continue
  case "$name" in
    pkg:*)
      pkg="$(printf '%s' "$name" | sed 's/^pkg://')"
      actual="$(printf '%s\n' "$inspect_out" | awk -v p="$pkg" '$1=="PKG" && $2==p {print $3}')"
      [ -n "$actual" ] || fail "in-image self-test did not report package $pkg"
      [ "$actual" = "$expected" ] || fail "package $pkg version is $actual, want locked $expected"
      ;;
    *)
      actual="$(printf '%s\n' "$inspect_out" | awk -v n="$name" '$1=="PROBE" && $2==n { $1=""; $2=""; sub(/^  /,""); print }')"
      [ -n "$actual" ] || fail "in-image self-test did not report binary $name"
      case "$actual" in
        *"$expected"*) ;;
        *) fail "binary $name reports '$actual', want locked version '$expected'" ;;
      esac
      ;;
  esac
done <"$work/version-expectations.tsv"
pass "sandbox binaries and node packages match the locked versions"

# --- vendor patched tree matches the lock ----------------------------------
# The runtime tree is root-owned with read-only (0555) directories, so a
# docker cp of it cannot be untarred by an unprivileged host. Hash the in-image
# tree inside a container and compare it to the host recomputation.
cat >"$work/image-vendor-hash.sh" <<'IMAGE_HASH'
set -eu
cd /opt/aurora/vendor/volcengine
find byted-ark-seedance-skill byted-ark-seedream-skill -type f -print0 | sort -z | while IFS= read -r -d '' f; do
  printf '%s\0' "$f"
  sha256sum "$f" | awk '{print $1}'
done | sha256sum | awk '{print $1}'
IMAGE_HASH
mkdir -p "$work/patched"
cp -a "$vendor_source/byted-ark-seedance-skill" "$work/patched/"
cp -a "$vendor_source/byted-ark-seedream-skill" "$work/patched/"
(
  cd "$work/patched"
  git init -q
  git apply "$vendor_source/patches/0001-seedance-broker-adapter.patch"
  git apply "$vendor_source/patches/0002-seedream-broker-adapter.patch"
  git apply "$vendor_source/patches/0003-seedream-fail-closed-cli.patch"
  git apply "$vendor_source/patches/0004-seedance-fail-closed-cli.patch"
  rm -rf .git
) || fail "could not apply the locked vendor patch series on the host"

manifest_hash() {
  # manifest_hash <root> <relative-directory>...
  local root="$1"; shift
  ( cd "$root" && find "$@" -type f -print0 | sort -z | while IFS= read -r -d '' f; do
      printf '%s\0' "$f"
      sha256sum "$f" | awk '{print $1}'
    done | sha256sum | awk '{print $1}' )
}
image_vendor_hash="$(docker run --rm -i --user 0:0 --entrypoint /bin/bash "$sandbox" - <"$work/image-vendor-hash.sh")" || fail "image has no /opt/aurora/vendor tree"
locked_vendor_hash="$(manifest_hash "$work/patched" "byted-ark-seedance-skill" "byted-ark-seedream-skill")"
[ "$image_vendor_hash" = "$locked_vendor_hash" ] || fail "vendor patched-tree hash $image_vendor_hash does not match the locked source+patches $locked_vendor_hash"

node - "$vendor_lock" "$vendor_source" <<'NODE' || fail "vendored source files or patch hashes do not match vendor-lock.json"
const fs = require('node:fs');
const crypto = require('node:crypto');
const [lockPath, source] = process.argv.slice(2);
const lock = JSON.parse(fs.readFileSync(lockPath, 'utf8'));
function sha256(file) {
  return 'sha256:' + crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}
for (const skill of Object.values(lock.skills)) {
  if (!skill.vendored) continue;
  for (const entry of skill.files) {
    const actual = sha256(source + '/' + entry.path);
    if (actual !== entry.sha256) {
      console.error('locked file mismatch: ' + entry.path + ' actual=' + actual);
      process.exit(1);
    }
  }
  for (const patch of skill.patches) {
    const actual = sha256(source + '/' + patch.path);
    if (actual !== patch.sha256) {
      console.error('locked patch mismatch: ' + patch.path);
      process.exit(1);
    }
  }
}
NODE
pass "sandbox vendor patched tree matches the locked source files and patch series"

# ===========================================================================
# Egress image
# ===========================================================================
if [ -n "$egress_image" ]; then
  egress_id="$(resolve_image "$egress_image")"
  egress="multica-aurora-egress-verify"
  docker tag "$egress_id" "$egress" >/dev/null
  check_image_identity "egress" "$egress" '["/usr/local/bin/aurora-egress-proxy"]'
  healthcheck="$(docker image inspect --format '{{json .Config.Healthcheck}}' "$egress")"
  [ "$healthcheck" = "null" ] || fail "egress image declares a health check: $healthcheck"
  check_no_exported_secrets "egress" "$egress"

  # Inspect the saved image layers rather than a container export: docker export
  # injects runtime files (.dockerenv, /etc/hosts, /proc, /sys) that are not part
  # of the image. Non-tar blobs (config, index, attestation) are skipped.
  docker save "$egress" >"$work/egress-image.tar"
  mkdir -p "$work/egress-save" "$work/egress-rootfs"
  tar -xf "$work/egress-image.tar" -C "$work/egress-save"
  : >"$work/egress-paths.txt"
  while IFS= read -r layer; do
    [ -n "$layer" ] || continue
    if tar -tf "$layer" >/dev/null 2>&1; then
      tar -tf "$layer" >>"$work/egress-paths.txt"
      tar -xf "$layer" -C "$work/egress-rootfs" 2>/dev/null || true
    fi
  done < <(find "$work/egress-save" \( -name 'layer.tar' -o -path '*/blobs/sha256/*' \) -type f)
  sort -u "$work/egress-paths.txt" -o "$work/egress-paths.txt"
  grep -qx 'usr/local/bin/aurora-egress-proxy' "$work/egress-paths.txt" || fail "egress image is missing /usr/local/bin/aurora-egress-proxy"
  grep -qx 'etc/ssl/certs/ca-certificates.crt' "$work/egress-paths.txt" || fail "egress image is missing the CA bundle"
  for forbidden_path in bin/ usr/bin/ usr/sbin/ sbin/ lib/ usr/lib/ etc/passwd etc/shadow lib/ld-linux-aarch64.so.1 lib64/ld-linux-x86-64.so.2 usr/bin/apt usr/bin/git usr/bin/curl usr/bin/wget usr/bin/ssh; do
    if grep -qx "$forbidden_path" "$work/egress-paths.txt"; then
      fail "egress image contains forbidden path /$forbidden_path"
    fi
  done
  file_count="$(grep -cv '/$' "$work/egress-paths.txt" || true)"
  if [ "$file_count" -gt 3 ]; then
    fail "egress image carries $file_count regular files, want only the proxy and CA bundle"
  fi
  secret_files="$(grep -r -I -E -l -f "$work/secret-file-patterns" "$work/egress-rootfs" 2>/dev/null || true)"
  [ -z "$secret_files" ] || fail "egress rootfs files contain a token/key pattern:
$secret_files"
  pass "egress image content, entrypoint, size and secret boundary are clean"
fi

pass "all sandbox image content invariants passed"
