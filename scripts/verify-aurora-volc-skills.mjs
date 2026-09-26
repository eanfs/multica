#!/usr/bin/env node
// Deterministic verifier for the vendored Volcengine AgentPlan skills.
//
// It recomputes a whole-tree SHA-256 with the same sorted path/content approach
// as server/pkg/skillbundle/hash.go (length-prefixed parts, sorted by path) and
// rejects any drift between the committed trees and their locks.
//
// The Seedance license gate is enforced here: the upstream package ships no
// LICENSE file, so the exact MIT text/copyright must be obtained from the
// Volcengine source owner, committed as byted-ark-seedance-skill/LICENSE.upstream,
// and its digest recorded in vendor-lock.json before the tree may be vendored.
// While that evidence is missing the verifier fails with "missing Seedance
// license text" and the Seedance tree stays out of the repository/image.
//
// No network calls are made.

import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(SCRIPT_DIR, "..");
export const REPO_ROOT_PATH = REPO_ROOT;
export const VENDOR_DIR = path.resolve(
  process.env.AURORA_VOLC_VENDOR_DIR || path.join(REPO_ROOT, "deploy/aurora-sandbox/vendor/volcengine"),
);
const SKILLS_LOCK_PATH = path.join(VENDOR_DIR, "skills-lock.json");
const VENDOR_LOCK_PATH = path.join(VENDOR_DIR, "vendor-lock.json");

export const SOURCE_URL = "https://skills.volces.com/skills/volcengine/agentplan";
export const SKILLS_CLI_VERSION = "1.7.0";
export const SECURITY_POLICY_VERSION = "aurora-sandbox-skill-runtime/v1";
export const TREE_HASH_ALGORITHM =
  "sha256 over sorted files of writeHashPart(path), writeHashPart('sha256:'+fileSha256), writeHashPart(content); writeHashPart is len(bytes)+':'+value+'\\n' (server/pkg/skillbundle/hash.go)";

// The audited source lock from the plan. The verifier treats this table as the
// authority and never rewrites it from the tree it is checking.
export const AUDITED = {
  "byted-ark-seedance-skill": {
    packageJsonName: "byted-ark-seedance-skill",
    declaredVersion: "5.0.0",
    spdxLicense: "MIT",
    computedHash: "9aed265f32003e867089212d831c1982e369fd766c02d8459770370377ca04eb",
    wellKnownDigest: "sha256:97bfafe21dfdd127cb7a040413ac2be5a0a816f75db1d049506dac222b42d6dd",
    licensePath: "byted-ark-seedance-skill/LICENSE.upstream",
    upstreamShipsLicense: false,
  },
  "byted-ark-seedream-skill": {
    packageJsonName: "ark-agentplan-seedream-skill",
    declaredVersion: "4.0.0",
    spdxLicense: "MIT",
    computedHash: "e11bbd33031f6c2e975291d08cb7ab09a9468ec7442496744070fc54f12c4a94",
    wellKnownDigest: "sha256:ecd6b80fc5b2b10c6dc944fd8940126a289e9ffb7f6890672baaa9ece4410a2c",
    licensePath: "byted-ark-seedream-skill/references/LICENSE",
    upstreamShipsLicense: true,
  },
};

const SKILL_IDS = Object.keys(AUDITED);
const errors = [];

function fail(message) {
  errors.push(message);
}

export function sha256Hex(value) {
  return "sha256:" + createHash("sha256").update(value).digest("hex");
}

// Byte-for-byte mirror of server/pkg/skillbundle/hash.go writeHashPart:
// fmt.Fprintf(h, "%d:%s\n", len(value), value)
function writeHashPart(hash, value) {
  const bytes = Buffer.isBuffer(value) ? value : Buffer.from(value, "utf8");
  hash.update(String(bytes.length) + ":");
  hash.update(bytes);
  hash.update("\n");
}

// Mirrors the file loop of skillbundle.BuildManifest: sorted paths, then
// path, "sha256:<file>", and the raw content as length-prefixed parts.
export function treeHash(files) {
  const sorted = [...files].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  const hash = createHash("sha256");
  for (const file of sorted) {
    const content = Buffer.isBuffer(file.content) ? file.content : Buffer.from(file.content, "utf8");
    writeHashPart(hash, file.path);
    writeHashPart(hash, sha256Hex(content));
    writeHashPart(hash, content);
  }
  return "sha256:" + hash.digest("hex");
}

function walk(root, prefix = "") {
  const out = [];
  for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
    const rel = prefix ? prefix + "/" + entry.name : entry.name;
    const abs = path.join(root, entry.name);
    if (entry.isDirectory()) {
      out.push(...walk(abs, rel));
    } else if (entry.isFile()) {
      out.push(rel);
    } else {
      fail("unsupported file type in vendor tree: " + rel);
    }
  }
  return out;
}

// Deterministic inventory of one skill directory. Used by the verifier and by
// scripts/update-aurora-volc-skills.sh so both derive identical digests.
export function inventoryTree(vendorDir, skillId) {
  const root = path.join(vendorDir, skillId);
  if (!fs.existsSync(root)) return { missing: true, files: [], whole_tree_sha256: null };
  const files = [];
  for (const rel of walk(root).sort()) {
    const abs = path.join(root, rel);
    const stat = fs.statSync(abs);
    const content = fs.readFileSync(abs);
    files.push({
      path: skillId + "/" + rel,
      bytes: stat.size,
      mode: (stat.mode & 0o777).toString(8).padStart(4, "0"),
      sha256: sha256Hex(content),
      content,
    });
  }
  return { missing: false, files, whole_tree_sha256: treeHash(files) };
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (error) {
    return { __error: error.message };
  }
}

function isNonEmptyString(value) {
  return typeof value === "string" && value.length > 0;
}

function isDigest(value) {
  return typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value);
}

export function patchApplies(vendorDir, patchFile) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aurora-volc-patch-"));
  try {
    for (const entry of fs.readdirSync(vendorDir, { withFileTypes: true })) {
      if (entry.isDirectory() && entry.name !== "patches") {
        fs.cpSync(path.join(vendorDir, entry.name), path.join(tmp, entry.name), { recursive: true });
      }
    }
    execFileSync("git", ["init", "-q"], { cwd: tmp, stdio: "pipe" });
    execFileSync("git", ["apply", "--check", "--whitespace=nowarn", patchFile], { cwd: tmp, stdio: "pipe" });
    return { ok: true };
  } catch (error) {
    const detail = error.stderr ? error.stderr.toString().trim() : error.message;
    return { ok: false, detail };
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

function verifyPatches(skillId, patches) {
  if (!Array.isArray(patches) || patches.length === 0) {
    fail("vendor-lock " + skillId + " has no hardening patches");
    return;
  }
  const seen = new Set();
  for (const patch of patches) {
    if (!isNonEmptyString(patch.path)) {
      fail("vendor-lock " + skillId + " patch entry is missing a path");
      continue;
    }
    if (seen.has(patch.path)) fail("duplicate patch entry for " + skillId + ": " + patch.path);
    seen.add(patch.path);
    const abs = path.join(VENDOR_DIR, patch.path);
    if (!fs.existsSync(abs)) {
      fail("missing patch file for " + skillId + ": " + patch.path);
      continue;
    }
    const actual = sha256Hex(fs.readFileSync(abs));
    if (typeof patch.sha256 !== "string" || !isDigest(patch.sha256)) {
      fail("missing patch digest for " + skillId + ": " + patch.path);
    } else if (patch.sha256 !== actual) {
      fail("patch digest mismatch for " + patch.path + ": lock=" + patch.sha256 + " actual=" + actual);
    }
    const applied = patchApplies(VENDOR_DIR, abs);
    if (!applied.ok) {
      fail("non-applying patch " + patch.path + ": " + applied.detail.split("\n")[0]);
    }
  }
}

function verifyField(label, actual, expected) {
  if (actual !== expected) {
    fail("wrong " + label + ": lock=" + JSON.stringify(actual) + " audited=" + JSON.stringify(expected));
  }
}

function main() {
  if (!fs.existsSync(VENDOR_LOCK_PATH)) {
    console.error("missing vendor lock: " + path.relative(REPO_ROOT, VENDOR_LOCK_PATH));
    process.exit(1);
  }
  if (!fs.existsSync(SKILLS_LOCK_PATH)) {
    console.error("missing skills lock: " + path.relative(REPO_ROOT, SKILLS_LOCK_PATH));
    process.exit(1);
  }

  const skillsLock = readJson(SKILLS_LOCK_PATH);
  const vendorLock = readJson(VENDOR_LOCK_PATH);
  if (vendorLock.__error) {
    console.error("vendor-lock.json is not valid JSON: " + vendorLock.__error);
    process.exit(1);
  }
  if (skillsLock.__error) fail("skills-lock.json is not valid JSON: " + skillsLock.__error);

  if (skillsLock.schema !== "com.multica.aurora.volcengine-skills-lock") {
    fail("unexpected skills-lock schema: " + JSON.stringify(skillsLock.schema));
  }
  if (skillsLock.version !== 1) fail("unexpected skills-lock version: " + JSON.stringify(skillsLock.version));
  if (skillsLock.skills_cli_version !== SKILLS_CLI_VERSION) {
    fail("skills-lock skills_cli_version must be " + SKILLS_CLI_VERSION);
  }
  if (!("upstream_revision" in skillsLock) || skillsLock.upstream_revision !== null) {
    fail("skills-lock upstream_revision must be null (the audited source has no revision)");
  }
  if (
    skillsLock.source?.source !== "skills.volces.com" ||
    skillsLock.source?.source_url !== SOURCE_URL ||
    skillsLock.source?.source_type !== "well-known"
  ) {
    fail("skills-lock source fields do not match the audited source");
  }

  if (vendorLock.schema !== "com.multica.aurora.volcengine-vendor-lock") {
    fail("unexpected vendor-lock schema: " + JSON.stringify(vendorLock.schema));
  }
  if (vendorLock.version !== 1) fail("unexpected vendor-lock version: " + JSON.stringify(vendorLock.version));
  if (vendorLock.security_policy_version !== SECURITY_POLICY_VERSION) {
    fail("vendor-lock security_policy_version must be " + SECURITY_POLICY_VERSION);
  }
  if (vendorLock.tree_hash_algorithm !== TREE_HASH_ALGORITHM) {
    fail("vendor-lock tree_hash_algorithm must describe the skillbundle hash");
  }
  if (!isNonEmptyString(vendorLock.reviewer)) fail("vendor-lock reviewer is required");

  for (const skillId of SKILL_IDS) {
    const audited = AUDITED[skillId];
    const lock = skillsLock.skills?.[skillId];
    if (!lock) {
      fail("skills-lock is missing entry for " + skillId);
    } else {
      verifyField("skill id for " + skillId, lock.skill_id, skillId);
      verifyField("package name for " + skillId, lock.package_name, audited.packageJsonName);
      verifyField("declared version for " + skillId, lock.declared_version, audited.declaredVersion);
      verifyField("license for " + skillId, lock.spdx_license, audited.spdxLicense);
      verifyField("computedHash for " + skillId, lock.computed_hash, audited.computedHash);
      verifyField("wellKnownDigest for " + skillId, lock.well_known_digest, audited.wellKnownDigest);
    }

    const vendor = vendorLock.skills?.[skillId];
    if (!vendor) {
      fail("vendor-lock is missing entry for " + skillId);
      continue;
    }
    verifyField("vendor-lock skill id for " + skillId, vendor.skill_id, skillId);
    verifyField("vendor-lock package name for " + skillId, vendor.package_name, audited.packageJsonName);
    verifyField("vendor-lock declared version for " + skillId, vendor.declared_version, audited.declaredVersion);
    verifyField("vendor-lock license for " + skillId, vendor.spdx_license, audited.spdxLicense);

    verifyPatches(skillId, vendor.patches);

    // A skill marked as not vendored is a hard distribution blocker. The only
    // sanctioned state is the audited Seedance license gate.
    if (vendor.vendored !== true) {
      if (vendor.vendored === false) {
        if (skillId !== "byted-ark-seedance-skill") {
          fail("vendor tree is not vendored for " + skillId);
        }
        const license = vendor.license || {};
        if (license.status !== "blocked" || !isNonEmptyString(license.reason)) {
          fail("vendor-lock " + skillId + " must record a blocked license with a reason while the tree is not vendored");
        }
        if (isDigest(license.sha256)) {
          fail("vendor-lock " + skillId + " records a license digest but the tree is not vendored");
        }
        fail(
          "missing Seedance license text: " +
            audited.licensePath +
            " (upstream ships no LICENSE; the exact MIT text/copyright must be obtained from the Volcengine source owner before this tree is distributed)",
        );
      } else {
        fail("vendor-lock " + skillId + " must declare vendored: true|false");
      }
      continue;
    }

    const tree = inventoryTree(VENDOR_DIR, skillId);
    if (tree.missing) {
      fail("missing vendor tree for " + skillId + " at " + path.relative(REPO_ROOT, path.join(VENDOR_DIR, skillId)));
      continue;
    }

    const recorded = Array.isArray(vendor.files) ? vendor.files : [];
    const recordedPaths = recorded.map((file) => file.path);
    const sortedPaths = [...recordedPaths].sort();
    if (recordedPaths.some((p, i) => p !== sortedPaths[i])) {
      fail("inventory for " + skillId + " is not sorted by path");
    }
    const actualByPath = new Map(tree.files.map((file) => [file.path, file]));
    const recordedByPath = new Map(recorded.map((file) => [file.path, file]));

    for (const file of tree.files) {
      const entry = recordedByPath.get(file.path);
      if (!entry) {
        fail("extra file not present in vendor-lock for " + skillId + ": " + file.path);
        continue;
      }
      if (entry.bytes !== file.bytes) {
        fail("changed byte size for " + file.path + ": lock=" + entry.bytes + " actual=" + file.bytes);
      }
      if (entry.mode !== file.mode) {
        fail("changed mode for " + file.path + ": lock=" + entry.mode + " actual=" + file.mode);
      }
      if (entry.sha256 !== file.sha256) {
        fail("changed bytes for " + file.path + ": lock=" + entry.sha256 + " actual=" + file.sha256);
      }
      if (!isDigest(entry.sha256)) fail("invalid file digest in vendor-lock for " + file.path);
    }
    for (const file of recorded) {
      if (!actualByPath.has(file.path)) fail("missing vendor file for " + skillId + ": " + file.path);
    }

    if (vendor.whole_tree_sha256 !== tree.whole_tree_sha256) {
      fail(
        "whole-tree hash mismatch for " +
          skillId +
          ": lock=" +
          JSON.stringify(vendor.whole_tree_sha256) +
          " actual=" +
          tree.whole_tree_sha256,
      );
    }

    const license = vendor.license || {};
    verifyField("license path for " + skillId, license.path, audited.licensePath);
    const licenseAbs = path.join(VENDOR_DIR, audited.licensePath);
    if (!fs.existsSync(licenseAbs)) {
      fail("missing license file for " + skillId + ": " + audited.licensePath);
    } else if (!isDigest(license.sha256)) {
      fail("vendor-lock " + skillId + ".license.sha256 is required");
    } else {
      const actual = sha256Hex(fs.readFileSync(licenseAbs));
      if (actual !== license.sha256) {
        fail("license hash mismatch for " + skillId + ": lock=" + license.sha256 + " actual=" + actual);
      }
    }
  }

  if (errors.length > 0) {
    for (const error of errors) console.error("ERROR: " + error);
    console.error("\n" + errors.length + " verification error(s)");
    process.exit(1);
  }
  console.log("verified " + SKILL_IDS.length + " vendored Volcengine skills against the audited source lock");
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
