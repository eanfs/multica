#!/usr/bin/env bash
# Resolve the Aurora runtime Debian package set from the dated snapshot.
#
# This script is the only writer of deploy/aurora-sandbox/apt-packages.lock.
# It runs a disposable container per locked architecture, points apt at the
# snapshot timestamp recorded in versions.json, and records the exact
# package=version plus repository/filename/SHA-256 that apt selected. It never
# asks apt for "latest" and never invents a version: if the snapshot or an
# architecture cannot be resolved it exits non-zero without touching the lock.
#
# The disposable image has no ca-certificates (that is one of the packages it
# resolves), so the resolver disables TLS peer/host verification for the apt
# transport and relies on apt's GPG verification of the snapshot InRelease and
# on the SHA-256 from the authenticated Packages index. No value is committed
# from an unverified transport.
#
# Usage:
#   scripts/update-aurora-sandbox-apt-lock.sh
# Environment overrides:
#   AURORA_SANDBOX_VERSIONS  path to versions.json
#   AURORA_APT_LOCK          path to the lock to rewrite
#   AURORA_APT_ARCHES        space-separated architectures (default from lock)
set -eo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSIONS="$REPO_ROOT/deploy/aurora-sandbox/versions.json"
LOCK="$REPO_ROOT/deploy/aurora-sandbox/apt-packages.lock"

if [ -n "$AURORA_SANDBOX_VERSIONS" ]; then VERSIONS="$AURORA_SANDBOX_VERSIONS"; fi
if [ -n "$AURORA_APT_LOCK" ]; then LOCK="$AURORA_APT_LOCK"; fi

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
command -v node >/dev/null 2>&1 || { echo "node is required" >&2; exit 1; }
[ -f "$VERSIONS" ] || { echo "missing versions.json: $VERSIONS" >&2; exit 1; }

NODE_IMAGE="$(node -e 'process.stdout.write(require(process.argv[1]).node.runtime_image)' "$VERSIONS")"
ARCHES="$(node -e 'process.stdout.write(require(process.argv[1]).debian_snapshot.architectures.join(" "))' "$VERSIONS")"
if [ -n "$AURORA_APT_ARCHES" ]; then ARCHES="$AURORA_APT_ARCHES"; fi

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

cp "$VERSIONS" "$TMP/versions.json"
cat > "$TMP/resolve.mjs" <<'NODE'
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const versions = JSON.parse(fs.readFileSync("/out/versions.json", "utf8"));
const snapshot = versions.debian_snapshot.timestamp;
const requested = versions.debian_snapshot.packages;
const arch = process.env.ARCH;

for (const entry of fs.readdirSync("/etc/apt/sources.list.d")) {
  fs.rmSync(path.join("/etc/apt/sources.list.d", entry), { force: true, recursive: true });
}
fs.rmSync("/etc/apt/sources.list", { force: true });
fs.writeFileSync(
  "/etc/apt/sources.list",
  "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/" + snapshot + " bookworm main\n" +
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/" + snapshot + " bookworm-security main\n",
);

const TLS = ["-o", "Acquire::https::Verify-Peer=false", "-o", "Acquire::https::Verify-Host=false"];
execFileSync("apt-get", TLS.concat(["update"]), { stdio: "inherit" });

function apt(args) {
  return execFileSync("apt-get", TLS.concat(args), { encoding: "utf8" });
}
function aptCache(args) {
  return execFileSync("apt-cache", args, { encoding: "utf8" });
}

const packages = {};
for (const pkg of requested) {
  const listing = apt(["install", "--print-uris", "--reinstall", "-y", "--no-install-recommends", pkg]);
  let chosen = null;
  for (const line of listing.split(/\r?\n/)) {
    const match = /^'([^']+)'\s+(\S+)\s/.exec(line);
    if (match && match[2].startsWith(pkg + "_")) {
      chosen = { uri: match[1], basename: match[2] };
      break;
    }
  }
  if (!chosen) throw new Error("no candidate for " + pkg + " on " + arch);
  const rest = chosen.basename.replace(/\.deb$/, "").slice(pkg.length + 1);
  const cut = rest.lastIndexOf("_");
  if (cut < 0) throw new Error("cannot parse version from " + chosen.basename);
  const version = decodeURIComponent(rest.slice(0, cut));
  const archToken = rest.slice(cut + 1);
  const show = aptCache(["show", pkg + "=" + version]);
  const sha = /^SHA256:\s*(\S+)\s*$/m.exec(show);
  const filename = /^Filename:\s*(\S+)\s*$/m.exec(show);
  if (!sha || !filename) throw new Error("missing SHA256/Filename for " + pkg + "=" + version);
  const poolIndex = chosen.uri.indexOf("/pool/");
  if (poolIndex < 0) throw new Error("unexpected snapshot URI: " + chosen.uri);
  packages[pkg] = {
    version,
    architecture: archToken,
    repository: chosen.uri.slice(0, poolIndex),
    filename: filename[1],
    sha256: "sha256:" + sha[1],
  };
}

fs.writeFileSync("/out/" + arch + ".json", JSON.stringify({ architecture: arch, packages }, null, 2) + "\n");
NODE

cat > "$TMP/merge.mjs" <<'NODE'
import fs from "node:fs";

const dir = process.argv[2];
const versionsPath = process.argv[3];
const lockPath = process.argv[4];
const versions = JSON.parse(fs.readFileSync(versionsPath, "utf8"));
const snapshot = versions.debian_snapshot.timestamp;
const arches = versions.debian_snapshot.architectures;
const requested = versions.debian_snapshot.packages;

const byArch = {};
for (const arch of arches) {
  const file = dir + "/" + arch + ".json";
  if (!fs.existsSync(file)) throw new Error("missing resolution for " + arch);
  byArch[arch] = JSON.parse(fs.readFileSync(file, "utf8"));
}

const expectedSet = requested.slice().sort().join(",");
for (const arch of arches) {
  const actualSet = Object.keys(byArch[arch].packages).sort().join(",");
  if (actualSet !== expectedSet) {
    throw new Error("architecture " + arch + " package set differs: " + actualSet);
  }
  for (const pkg of requested) {
    const entry = byArch[arch].packages[pkg];
    if (!entry || !entry.version || !/^sha256:[0-9a-f]{64}$/.test(entry.sha256)) {
      throw new Error("unresolved package " + pkg + " on " + arch);
    }
  }
}

const packages = {};
for (const pkg of requested) {
  packages[pkg] = {};
  for (const arch of arches) packages[pkg][arch] = byArch[arch].packages[pkg];
}

const lock = {
  schema: "com.multica.aurora.apt-packages-lock",
  version: 1,
  snapshot_timestamp: snapshot,
  sources: [
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/" + snapshot + " bookworm main",
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/" + snapshot + " bookworm-security main",
  ],
  architectures: arches,
  packages,
};
fs.writeFileSync(lockPath, JSON.stringify(lock, null, 2) + "\n");
console.log("wrote " + lockPath + " for " + arches.join(","));
NODE

for ARCH in $ARCHES; do
  echo "==> resolving $ARCH from snapshot $(node -e 'process.stdout.write(require(process.argv[1]).debian_snapshot.timestamp)' "$VERSIONS")" >&2
  docker run --rm --platform "linux/$ARCH" -v "$TMP:/out" -e "ARCH=$ARCH" "$NODE_IMAGE" node /out/resolve.mjs
done

node "$TMP/merge.mjs" "$TMP" "$VERSIONS" "$LOCK"
