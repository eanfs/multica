#!/usr/bin/env bash
# Isolated, pinned updater for the vendored Volcengine AgentPlan skills.
#
# It always runs the two requested installs in a throwaway HOME/project with the
# pinned skills CLI, verifies the audited computedHash and wellKnownDigest values
# BEFORE replacing any vendor tree, copies the installed trees byte-for-byte, and
# rewrites skills-lock.json and vendor-lock.json from what was actually retrieved.
#
# The temporary HOME/project lives under ${TMPDIR:-/tmp}, is never committed, and
# is removed on exit unless --keep-temp is given.
#
# Seedance license gate (plan Step 4): the upstream Seedance package ships no
# LICENSE file, so the tree is NOT vendored until the exact MIT text/copyright is
# supplied with --seedance-license (or AURORA_SEEDANCE_LICENSE). In that state the
# script records the skill as blocked and exits 2.
#
# Usage:
#   scripts/update-aurora-volc-skills.sh [--reviewer NAME]
#     [--seedance-license PATH] [--reuse-project DIR] [--keep-temp]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR_DIR="${AURORA_VOLC_VENDOR_DIR:-$REPO_ROOT/deploy/aurora-sandbox/vendor/volcengine}"
VERIFIER="$REPO_ROOT/scripts/verify-aurora-volc-skills.mjs"
SKILLS_CLI_VERSION="1.7.0"
SOURCE_URL="https://skills.volces.com/skills/volcengine/agentplan"
SECURITY_POLICY_VERSION="aurora-sandbox-skill-runtime/v1"
REVIEWER="${AURORA_VOLC_REVIEWER:-$(git -C "$REPO_ROOT" config user.name 2>/dev/null || true)}"
SEEDANCE_LICENSE="${AURORA_SEEDANCE_LICENSE:-}"
REUSE_PROJECT=""
KEEP_TEMP="0"
RETRIEVED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --reviewer) REVIEWER="${2:?--reviewer needs a value}"; shift 2 ;;
    --seedance-license) SEEDANCE_LICENSE="${2:?--seedance-license needs a value}"; shift 2 ;;
    --vendor-dir) VENDOR_DIR="${2:?--vendor-dir needs a value}"; shift 2 ;;
    --reuse-project) REUSE_PROJECT="${2:?--reuse-project needs a value}"; shift 2 ;;
    --keep-temp) KEEP_TEMP="1"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 1 ;;
  esac
done

[[ -n "$REVIEWER" ]] || REVIEWER="unknown"

TEMP="$(mktemp -d "${TMPDIR:-/tmp}/aurora-volc-vendor.XXXXXX")"
cleanup() {
  if [[ "$KEEP_TEMP" != "1" ]]; then
    rm -rf "$TEMP"
  else
    echo "kept temporary project at $TEMP" >&2
  fi
}
trap cleanup EXIT

if [[ -n "$REUSE_PROJECT" ]]; then
  PROJECT_DIR="$(cd "$REUSE_PROJECT" && pwd)"
else
  PROJECT_DIR="$TEMP/project"
  mkdir -p "$PROJECT_DIR" "$TEMP/home"
  export HOME="$TEMP/home"
  export npm_config_cache="$TEMP/home/.npm"
  echo "==> retrieving skills with skills@$SKILLS_CLI_VERSION into an isolated HOME" >&2
  ( cd "$PROJECT_DIR" && npx --yes "skills@$SKILLS_CLI_VERSION" add "$SOURCE_URL" \
      -s byted-ark-seedance-skill --agent claude-code --copy --yes )
  ( cd "$PROJECT_DIR" && npx --yes "skills@$SKILLS_CLI_VERSION" add "$SOURCE_URL" \
      -s byted-ark-seedream-skill --agent claude-code --copy --yes )
fi

mkdir -p "$VENDOR_DIR/patches"

if [[ -n "$SEEDANCE_LICENSE" ]]; then
  [[ -f "$SEEDANCE_LICENSE" ]] || { echo "seedance license not found: $SEEDANCE_LICENSE" >&2; exit 1; }
  SEEDANCE_LICENSE="$(cd "$(dirname "$SEEDANCE_LICENSE")" && pwd)/$(basename "$SEEDANCE_LICENSE")"
  export AURORA_SEEDANCE_LICENSE="$SEEDANCE_LICENSE"
else
  export AURORA_SEEDANCE_LICENSE=""
fi

export AURORA_VERIFIER="$VERIFIER"
export AURORA_VENDOR_DIR="$VENDOR_DIR"
export AURORA_PROJECT_DIR="$PROJECT_DIR"
export AURORA_REVIEWER="$REVIEWER"
export AURORA_RETRIEVED_AT="$RETRIEVED_AT"
export AURORA_SKILLS_CLI_VERSION="$SKILLS_CLI_VERSION"
export AURORA_SECURITY_POLICY_VERSION="$SECURITY_POLICY_VERSION"

set +e
node --input-type=module <<'NODE'
import fs from "node:fs";
import path from "node:path";

const {
  AUDITED,
  SOURCE_URL,
  SKILLS_CLI_VERSION,
  SECURITY_POLICY_VERSION,
  TREE_HASH_ALGORITHM,
  sha256Hex,
  inventoryTree,
  patchApplies,
} = await import(process.env.AURORA_VERIFIER);

const vendorDir = process.env.AURORA_VENDOR_DIR;
const projectDir = process.env.AURORA_PROJECT_DIR;
const seedanceLicense = process.env.AURORA_SEEDANCE_LICENSE || "";

const cliLock = JSON.parse(fs.readFileSync(path.join(projectDir, "skills-lock.json"), "utf8"));

// 1. Verify the audited digests BEFORE touching any vendor tree.
for (const [skillId, audited] of Object.entries(AUDITED)) {
  const entry = cliLock.skills?.[skillId];
  if (!entry) throw new Error("retrieved lock is missing " + skillId);
  if (entry.computedHash !== audited.computedHash) {
    throw new Error("computedHash mismatch for " + skillId + ": " + entry.computedHash);
  }
  if (entry.wellKnownDigest !== audited.wellKnownDigest) {
    throw new Error("wellKnownDigest mismatch for " + skillId + ": " + entry.wellKnownDigest);
  }
}

function readSkillMetadata(skillId, installDir) {
  const pkg = JSON.parse(fs.readFileSync(path.join(installDir, "package.json"), "utf8"));
  const skillMd = fs.readFileSync(path.join(installDir, "SKILL.md"), "utf8");
  const license = /^license:\s*["']?([^"'\n]+)["']?\s*$/m.exec(skillMd);
  return { packageName: pkg.name, declaredVersion: pkg.version, spdxLicense: license ? license[1].trim() : "UNKNOWN" };
}

function replaceTree(sourceDir, targetDir) {
  fs.rmSync(targetDir, { recursive: true, force: true });
  fs.mkdirSync(path.dirname(targetDir), { recursive: true });
  fs.cpSync(sourceDir, targetDir, { recursive: true, preserveTimestamps: true });
}

const skills = {};
const gateBlocked = [];

for (const skillId of Object.keys(AUDITED)) {
  const audited = AUDITED[skillId];
  const installDir = path.join(projectDir, ".claude/skills", skillId);
  const meta = readSkillMetadata(skillId, installDir);
  if (meta.packageName !== audited.packageJsonName) {
    throw new Error("package name mismatch for " + skillId + ": " + meta.packageName + " != " + audited.packageJsonName);
  }
  if (meta.declaredVersion !== audited.declaredVersion) {
    throw new Error("declared version mismatch for " + skillId + ": " + meta.declaredVersion);
  }
  if (meta.spdxLicense !== audited.spdxLicense) {
    throw new Error("declared license mismatch for " + skillId + ": " + meta.spdxLicense);
  }

  const targetDir = path.join(vendorDir, skillId);
  let vendored = true;
  let license;

  if (skillId === "byted-ark-seedance-skill" && !seedanceLicense) {
    if (fs.existsSync(path.join(installDir, "LICENSE"))) {
      throw new Error("upstream now ships a Seedance LICENSE; re-audit before vendoring");
    }
    vendored = false;
    gateBlocked.push(skillId);
    license = {
      path: audited.licensePath,
      sha256: null,
      status: "blocked",
      reason:
        "The upstream Seedance package ships no LICENSE file; only the SKILL.md frontmatter declares MIT. " +
        "The exact MIT text/copyright for Seedance 5.0.0 must be obtained from the Volcengine source owner and " +
        "passed with --seedance-license before this tree may be vendored or embedded in an image.",
    };
  } else if (skillId === "byted-ark-seedance-skill") {
    replaceTree(installDir, targetDir);
    const licenseTarget = path.join(targetDir, "LICENSE.upstream");
    fs.copyFileSync(seedanceLicense, licenseTarget);
    license = { path: audited.licensePath, sha256: sha256Hex(fs.readFileSync(licenseTarget)), status: "verified" };
  } else {
    replaceTree(installDir, targetDir);
    license = {
      path: audited.licensePath,
      sha256: sha256Hex(fs.readFileSync(path.join(targetDir, "references/LICENSE"))),
      status: "verified",
    };
  }

  const skillToken = skillId.replace("byted-ark-", "").replace("-skill", "");
  const patchFiles = fs
    .readdirSync(path.join(vendorDir, "patches"))
    .filter((name) => name.endsWith(".patch"))
    .filter((name) => name.includes(skillToken))
    .sort();
  const recordedPatches = patchFiles.map((name) => {
    const abs = path.join(vendorDir, "patches", name);
    return { path: "patches/" + name, sha256: sha256Hex(fs.readFileSync(abs)), applies: patchApplies(vendorDir, abs).ok };
  });

  const entry = {
    skill_id: skillId,
    package_name: meta.packageName,
    declared_version: meta.declaredVersion,
    spdx_license: meta.spdxLicense,
    vendored,
    license,
  };
  if (vendored) {
    const inventory = inventoryTree(vendorDir, skillId);
    entry.whole_tree_sha256 = inventory.whole_tree_sha256;
    entry.files = inventory.files.map(({ path: filePath, bytes, mode, sha256 }) => ({ path: filePath, bytes, mode, sha256 }));
  }
  entry.patches = recordedPatches;
  skills[skillId] = entry;
}

const skillsLock = {
  schema: "com.multica.aurora.volcengine-skills-lock",
  version: 1,
  retrieved_at: process.env.AURORA_RETRIEVED_AT,
  skills_cli_version: SKILLS_CLI_VERSION,
  upstream_revision: null,
  source: {
    source: "skills.volces.com",
    source_url: SOURCE_URL,
    source_type: "well-known",
    well_known_index_url: SOURCE_URL + "/.well-known/skills/index.json",
  },
  skills: Object.fromEntries(
    Object.keys(AUDITED).map((skillId) => [
      skillId,
      {
        skill_id: skillId,
        package_name: AUDITED[skillId].packageJsonName,
        declared_version: AUDITED[skillId].declaredVersion,
        spdx_license: AUDITED[skillId].spdxLicense,
        computed_hash: cliLock.skills[skillId].computedHash,
        well_known_digest: cliLock.skills[skillId].wellKnownDigest,
      },
    ]),
  ),
};

const vendorLock = {
  schema: "com.multica.aurora.volcengine-vendor-lock",
  version: 1,
  reviewer: process.env.AURORA_REVIEWER,
  security_policy_version: SECURITY_POLICY_VERSION,
  tree_hash_algorithm: TREE_HASH_ALGORITHM,
  skills,
};

fs.writeFileSync(path.join(vendorDir, "skills-lock.json"), JSON.stringify(skillsLock, null, 2) + "\n");
fs.writeFileSync(path.join(vendorDir, "vendor-lock.json"), JSON.stringify(vendorLock, null, 2) + "\n");

if (gateBlocked.length > 0) {
  console.error(
    "GATE: " + gateBlocked.join(", ") + " not vendored: upstream ships no LICENSE; obtain the exact MIT text/copyright " +
      "from the Volcengine source owner and re-run with --seedance-license.",
  );
  process.exit(2);
}
console.log("vendored " + Object.keys(skills).length + " skills into " + vendorDir);
NODE
STATUS=$?
set -e

echo "==> wrote $VENDOR_DIR/skills-lock.json and $VENDOR_DIR/vendor-lock.json" >&2
exit "$STATUS"
