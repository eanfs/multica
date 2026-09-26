#!/usr/bin/env node
// Deterministic cross-lock verifier for the Aurora managed sandbox image.
//
// It treats the plan's Locked Inputs table as the authority and never rewrites
// the lock from the artifacts it is checking. It cross-checks:
//   - deploy/aurora-sandbox/versions.json against the audited table;
//   - server/go.mod against the locked Go toolchain;
//   - deploy/aurora-sandbox/runtime/package.json and pnpm-lock.yaml;
//   - the Debian snapshot apt-packages.lock (both architectures);
//   - the child plan C vendor locks (vendor-lock.json and skills-lock.json);
//   - deploy/aurora-sandbox/Dockerfile and Dockerfile.egress when present
//     (Task 2 creates them; this verifier still rejects a mutable FROM tag,
//     a wrong digest, an unpinned apt install, or a network fetch of the
//     skills/HyperFrames source).
//
// No network calls are made. The runtime package/lock checks read files only.

import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const SCRIPT_DIR = path.dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = path.resolve(SCRIPT_DIR, "..");

export const REL = {
  versions: "deploy/aurora-sandbox/versions.json",
  runtimePackage: "deploy/aurora-sandbox/runtime/package.json",
  pnpmLock: "pnpm-lock.yaml",
  pnpmWorkspace: "pnpm-workspace.yaml",
  goMod: "server/go.mod",
  aptLock: "deploy/aurora-sandbox/apt-packages.lock",
  vendorLock: "deploy/aurora-sandbox/vendor/volcengine/vendor-lock.json",
  skillsLock: "deploy/aurora-sandbox/vendor/volcengine/skills-lock.json",
  dockerfile: "deploy/aurora-sandbox/Dockerfile",
  dockerfileEgress: "deploy/aurora-sandbox/Dockerfile.egress",
};

// The audited lock. Mirrors the plan's Locked Inputs table and the child plan C
// vendor lock; the verifier never derives these from the tree under test.
export const LOCKED = {
  schema: "com.multica.aurora.sandbox-image-versions",
  version: 1,
  retrieved_at: "2026-09-25",
  go: {
    toolchain: "1.26.6",
    builder_image:
      "golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36",
  },
  node: {
    runtime_image:
      "node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c",
  },
  runtime_packages: {
    "@anthropic-ai/claude-code": "2.1.282",
    "@modelcontextprotocol/sdk": "1.30.1",
    hyperframes: "0.8.75",
    openai: "7.23.0",
  },
  runtime_workspace_dependencies: { zod: "catalog:" },
  hyperframes_source: {
    tag: "v0.8.75",
    commit: "a95cb96a5dd3c1f7b31266a4b470590c86ad231f",
    license: "Apache-2.0",
    node_engine: ">=22",
  },
  skills_cli: "1.7.0",
  debian_snapshot: "20260925T000000Z",
  apt_architectures: ["amd64", "arm64"],
  apt_packages: [
    "ca-certificates",
    "chromium",
    "ffmpeg",
    "fonts-noto-cjk",
    "fonts-noto-color-emoji",
    "imagemagick",
    "libnss3",
    "poppler-utils",
    "tini",
    "unzip",
  ],
  vendor_trees: {
    "byted-ark-seedance-skill": {
      declared_version: "5.0.0",
      vendored: false,
      whole_tree_sha256: null,
      license_sha256: null,
    },
    "byted-ark-seedream-skill": {
      declared_version: "4.0.0",
      vendored: true,
      whole_tree_sha256: "sha256:aac297142496fe2f07f3c4d9a8d792110c5bad871a515f72b1374bbde3a1fc0d",
      license_sha256: "sha256:8a32047e9ee5270ab9f49e5d293b650bc0253e05bff2d81787b3276ea0d75935",
    },
  },
};

const EXACT_SEMVER = /^[0-9]+\.[0-9]+\.[0-9]+$/;
const DIGEST = /^sha256:[0-9a-f]{64}$/;
const IMAGE_REF = /^[a-zA-Z0-9][a-zA-Z0-9._/:-]*@sha256:[0-9a-f]{64}$/;
const CREDENTIAL_KEY = /"(api[_-]?key|secret|password|token)"/i;
const RUNTIME_IMPORTER = "deploy/aurora-sandbox/runtime";

function parseJson(text) {
  if (text === null || text === undefined) {
    return { value: null, error: "file is missing" };
  }
  try {
    return { value: JSON.parse(text), error: null };
  } catch (error) {
    return { value: null, error: error.message };
  }
}

function readLockText(root, rel) {
  const primary = path.join(root, rel);
  if (fs.existsSync(primary)) return fs.readFileSync(primary, "utf8");
  if (path.resolve(root) !== REPO_ROOT) {
    const fallback = path.join(REPO_ROOT, rel);
    if (fs.existsSync(fallback)) return fs.readFileSync(fallback, "utf8");
  }
  return null;
}

export function loadLockState(root = REPO_ROOT) {
  const resolved = path.resolve(root);
  return {
    root: resolved,
    versions: parseJson(readLockText(resolved, REL.versions)),
    runtimePackage: parseJson(readLockText(resolved, REL.runtimePackage)),
    aptLock: parseJson(readLockText(resolved, REL.aptLock)),
    vendorLock: parseJson(readLockText(resolved, REL.vendorLock)),
    skillsLock: parseJson(readLockText(resolved, REL.skillsLock)),
    pnpmLockText: readLockText(resolved, REL.pnpmLock),
    pnpmWorkspaceText: readLockText(resolved, REL.pnpmWorkspace),
    goModText: readLockText(resolved, REL.goMod),
    dockerfile: readLockText(resolved, REL.dockerfile),
    dockerfileEgress: readLockText(resolved, REL.dockerfileEgress),
  };
}

function applyFlatOverrides(state, overrides) {
  const versions = state.versions.value;
  if (versions) {
    if (overrides.nodeBase !== undefined) versions.node.runtime_image = overrides.nodeBase;
    if (overrides.goBuilder !== undefined) versions.go.builder_image = overrides.goBuilder;
    if (overrides.goVersion !== undefined) versions.go.toolchain = overrides.goVersion;
    if (overrides.hyperframesCommit !== undefined) versions.hyperframes_source.commit = overrides.hyperframesCommit;
    if (overrides.hyperframesTag !== undefined) versions.hyperframes_source.tag = overrides.hyperframesTag;
    if (overrides.snapshotTimestamp !== undefined) versions.debian_snapshot.timestamp = overrides.snapshotTimestamp;
    if (overrides.aptPackages !== undefined) versions.debian_snapshot.packages = overrides.aptPackages;
  }
  if (state.runtimePackage.value && overrides.claudeCode !== undefined) {
    state.runtimePackage.value.dependencies["@anthropic-ai/claude-code"] = overrides.claudeCode;
  }
  if (state.runtimePackage.value && overrides.runtimeDependencies !== undefined) {
    state.runtimePackage.value.dependencies = Object.assign(
      {},
      state.runtimePackage.value.dependencies,
      overrides.runtimeDependencies,
    );
  }
  if (overrides.aptLock !== undefined) state.aptLock.value = overrides.aptLock;
  if (overrides.vendorLock !== undefined) state.vendorLock.value = overrides.vendorLock;
  if (overrides.dockerfile !== undefined) state.dockerfile = overrides.dockerfile;
  if (overrides.dockerfileEgress !== undefined) state.dockerfileEgress = overrides.dockerfileEgress;
  if (overrides.pnpmLockText !== undefined) state.pnpmLockText = overrides.pnpmLockText;
  if (overrides.goMod !== undefined) state.goModText = overrides.goMod;
}

function deepAssign(target, source) {
  for (const [key, value] of Object.entries(source)) {
    if (value && typeof value === "object" && !Array.isArray(value) && target[key] && typeof target[key] === "object") {
      deepAssign(target[key], value);
    } else {
      target[key] = value;
    }
  }
  return target;
}

function expectedSources(timestamp) {
  return [
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/" + timestamp + " bookworm main",
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/" + timestamp + " bookworm-security main",
  ];
}

function checkExact(label, actual, expected, errors) {
  if (actual !== expected) {
    errors.push(label + " mismatch: lock=" + JSON.stringify(actual) + " audited=" + JSON.stringify(expected));
  }
}

function checkImageRef(label, ref, expectedRef, errors) {
  if (typeof ref !== "string" || !IMAGE_REF.test(ref)) {
    errors.push(label + " base image must be pinned by @sha256 digest: " + JSON.stringify(ref));
    return;
  }
  if (ref !== expectedRef) {
    errors.push(label + " base image does not match the locked digest: lock=" + ref + " audited=" + expectedRef);
  }
}

function isPlainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function parsePnpmRuntimeImporter(text) {
  if (typeof text !== "string") return null;
  const lines = text.split(/\r?\n/);
  const start = lines.findIndex(function (line) {
    return line === "  " + RUNTIME_IMPORTER + ":";
  });
  if (start < 0) return null;
  const deps = {};
  let inDependencies = false;
  let current = null;
  for (let i = start + 1; i < lines.length; i += 1) {
    const line = lines[i];
    if (/^  \S/.test(line)) break;
    if (line === "    dependencies:") {
      inDependencies = true;
      continue;
    }
    if (/^    [A-Za-z]+Dependencies:$/.test(line)) {
      inDependencies = false;
      current = null;
      continue;
    }
    if (!inDependencies) continue;
    const keyMatch = /^      '?([^':]+)'?:$/.exec(line);
    if (keyMatch) {
      current = keyMatch[1];
      deps[current] = { specifier: null, version: null };
      continue;
    }
    if (current === null) continue;
    const specMatch = /^        specifier:\s*(.*)$/.exec(line);
    if (specMatch) deps[current].specifier = specMatch[1].replace(/^'|'$/g, "");
    const verMatch = /^        version:\s*(.*)$/.exec(line);
    if (verMatch) deps[current].version = verMatch[1].replace(/^'|'$/g, "");
  }
  return deps;
}

function findPackageBlock(text, key) {
  if (typeof text !== "string") return null;
  const lines = text.split(/\r?\n/);
  const needles = ["  '" + key + "':", "  " + key + ":"];
  const idx = lines.findIndex(function (line) {
    return needles.indexOf(line) >= 0;
  });
  if (idx < 0) return null;
  const block = [];
  for (let i = idx + 1; i < lines.length; i += 1) {
    if (/^  \S/.test(lines[i])) break;
    block.push(lines[i]);
  }
  return block.join("\n");
}

function baseVersion(value) {
  if (typeof value !== "string") return null;
  const cut = value.indexOf("(");
  return cut >= 0 ? value.slice(0, cut) : value;
}

function collectImageRefs(value, out) {
  if (typeof value === "string") {
    if (IMAGE_REF.test(value)) out.add(value);
    return;
  }
  if (Array.isArray(value)) {
    for (const entry of value) collectImageRefs(entry, out);
    return;
  }
  if (isPlainObject(value)) {
    for (const entry of Object.values(value)) collectImageRefs(entry, out);
  }
}

function logicalLines(text) {
  const out = [];
  let buffer = "";
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.replace(/\s+$/, "");
    if (line.endsWith("\\")) {
      buffer += line.slice(0, -1) + " ";
      continue;
    }
    out.push(buffer + line);
    buffer = "";
  }
  if (buffer.length > 0) out.push(buffer);
  return out;
}

function aptLockVersion(state, name) {
  const apt = state.aptLock.value;
  if (!apt || !apt.packages || !apt.packages[name]) return null;
  const arch = LOCKED.apt_architectures[0];
  const record = apt.packages[name][arch];
  return record && record.version ? record.version : null;
}

function checkAptInstall(label, line, state, errors) {
  const match = /\bapt(-get)?\s+install\b/.exec(line);
  if (!match) return;
  const rest = line.slice(match.index + match[0].length);
  const tokens = rest.split(/\s+/).filter(Boolean);
  for (let token of tokens) {
    if (token === "&&" || token === "||" || token === ";" || token === "|" || token === ">" || token === "<") break;
    if (token.startsWith("-")) continue;
    if (token.startsWith("$")) continue;
    token = token.replace(/^["']/, "").replace(/["']$/, "").replace(/\\$/, "");
    if (token.length === 0) continue;
    if (token.indexOf("=") >= 0) {
      const parts = token.split("=");
      const name = parts[0];
      const version = parts.slice(1).join("=");
      if (LOCKED.apt_packages.indexOf(name) < 0) {
        errors.push("Dockerfile " + label + " installs apt package outside the runtime package set: " + name);
        continue;
      }
      if (version.length === 0) {
        errors.push("unpinned apt package: " + name + " (Dockerfile " + label + ")");
        continue;
      }
      const locked = aptLockVersion(state, name);
      if (locked !== null && locked !== version) {
        errors.push(
          "apt package version drift for " + name + " (Dockerfile " + label + "): docker=" + version + " lock=" + locked,
        );
      }
    } else {
      if (LOCKED.apt_packages.indexOf(token) >= 0) {
        errors.push("unpinned apt package: " + token + " (Dockerfile " + label + " must pin =<version>)");
      } else {
        errors.push("Dockerfile " + label + " installs apt package outside the runtime package set: " + token);
      }
    }
  }
}

function checkDockerfile(label, text, versions, state, errors) {
  const declaredRefs = new Set();
  collectImageRefs(versions, declaredRefs);
  const stageNames = new Set();
  const lines = logicalLines(text);
  const fetchPattern = /\b(curl|wget|git\s+(clone|fetch)|npx|npm\s+(i|install|exec)|pnpm\s+(add|install|fetch)|ADD\s+https?:\/\/)/i;
  const sourcePattern = /(hyperframes|skills\.volces\.com|skills@|github\.com\/[^\s]*hyperframes)/i;

  for (const line of lines) {
    if (fetchPattern.test(line) && sourcePattern.test(line)) {
      errors.push("Dockerfile " + label + " network fetch for skills/HyperFrames source: " + line.trim().slice(0, 200));
    }
    const fromMatch = /^\s*FROM\s+(\S+)(?:\s+[Aa][Ss]\s+(\S+))?/.exec(line);
    if (fromMatch) {
      const ref = fromMatch[1];
      const alias = fromMatch[2];
      if (alias) stageNames.add(alias.toLowerCase());
      if (ref.toLowerCase() === "scratch") continue;
      if (stageNames.has(ref.toLowerCase())) continue;
      if (!IMAGE_REF.test(ref)) {
        errors.push("Dockerfile " + label + " FROM must be pinned by @sha256 digest: " + ref);
        continue;
      }
      if (!declaredRefs.has(ref)) {
        const refName = ref.split("@")[0];
        let sameName = null;
        for (const candidate of declaredRefs) {
          if (candidate.split("@")[0] === refName) {
            sameName = candidate;
            break;
          }
        }
        if (sameName) {
          errors.push("Dockerfile " + label + " FROM " + ref + " does not match the locked digest " + sameName);
        } else {
          errors.push("Dockerfile " + label + " FROM " + ref + " is not declared in versions.json");
        }
      }
      continue;
    }
    checkAptInstall(label, line, state, errors);
  }
}

function checkVendorTrees(state, versions, errors) {
  const vendorLock = state.vendorLock.value;
  const skillsLock = state.skillsLock.value;
  if (!isPlainObject(vendorLock) || !isPlainObject(vendorLock.skills)) {
    errors.push("missing or invalid " + REL.vendorLock + ": " + (state.vendorLock.error || "no skills"));
    return;
  }
  checkExact("vendor-lock schema", vendorLock.schema, "com.multica.aurora.volcengine-vendor-lock", errors);
  if (vendorLock.version !== 1) errors.push("vendor-lock version must be 1: " + JSON.stringify(vendorLock.version));
  if (!isPlainObject(skillsLock) || !isPlainObject(skillsLock.skills)) {
    errors.push("missing or invalid " + REL.skillsLock + ": " + (state.skillsLock.error || "no skills"));
  }

  const trees = isPlainObject(versions.vendor_trees) ? versions.vendor_trees : {};
  for (const skillId of Object.keys(vendorLock.skills)) {
    if (!(skillId in trees)) errors.push("versions.json is missing vendor tree " + skillId);
  }

  for (const [skillId, tree] of Object.entries(trees)) {
    const skill = vendorLock.skills[skillId];
    const audited = LOCKED.vendor_trees[skillId];
    if (!audited) {
      errors.push("versions.json records an unaudited vendor tree: " + skillId);
      continue;
    }
    checkExact("vendor tree version for " + skillId, tree.declared_version, audited.declared_version, errors);
    if (tree.vendored !== audited.vendored) {
      errors.push("vendor tree vendored flag drift for " + skillId + ": lock=" + JSON.stringify(tree.vendored));
    }
    if (!isPlainObject(skill)) {
      errors.push("vendor-lock is missing entry for " + skillId);
      continue;
    }
    checkExact("vendor-lock declared version for " + skillId, skill.declared_version, audited.declared_version, errors);
    if (skill.vendored !== tree.vendored) {
      errors.push(
        "vendor tree vendored flag drift for " + skillId + ": versions=" + JSON.stringify(tree.vendored) + " vendor-lock=" + JSON.stringify(skill.vendored),
      );
    }

    const skillsEntry = isPlainObject(skillsLock) && isPlainObject(skillsLock.skills) ? skillsLock.skills[skillId] : null;
    if (!skillsEntry) {
      errors.push("skills-lock is missing entry for " + skillId);
    } else {
      checkExact("skills-lock declared version for " + skillId, skillsEntry.declared_version, skill.declared_version, errors);
      if (typeof skillsEntry.computed_hash !== "string" || skillsEntry.computed_hash.length === 0) {
        errors.push("skills-lock is missing computed_hash for " + skillId);
      }
      if (typeof skillsEntry.well_known_digest !== "string" || !DIGEST.test(skillsEntry.well_known_digest)) {
        errors.push("skills-lock is missing well_known_digest for " + skillId);
      }
    }

    if (skill.vendored === true) {
      if (!DIGEST.test(tree.whole_tree_sha256) || !DIGEST.test(skill.whole_tree_sha256)) {
        errors.push(
          "missing vendor tree hash for " + skillId + ": versions=" + JSON.stringify(tree.whole_tree_sha256) + " vendor-lock=" + JSON.stringify(skill.whole_tree_sha256),
        );
      } else if (tree.whole_tree_sha256 !== skill.whole_tree_sha256) {
        errors.push(
          "vendor tree hash mismatch for " + skillId + ": versions=" + tree.whole_tree_sha256 + " vendor-lock=" + skill.whole_tree_sha256,
        );
      }
      const licenseHash = isPlainObject(skill.license) ? skill.license.sha256 : null;
      if (isExecutableVendorTree(skill)) {
        if (!DIGEST.test(tree.license_sha256) || !DIGEST.test(licenseHash)) {
          errors.push(
            "executable vendor tree " + skillId + " is missing a license hash: versions=" + JSON.stringify(tree.license_sha256) + " vendor-lock=" + JSON.stringify(licenseHash),
          );
        }
      }
      if (DIGEST.test(tree.license_sha256) && DIGEST.test(licenseHash) && tree.license_sha256 !== licenseHash) {
        errors.push("vendor license hash mismatch for " + skillId);
      }
    } else {
      if (tree.whole_tree_sha256 !== null && tree.whole_tree_sha256 !== undefined) {
        errors.push("versions.json records a tree hash for the non-vendored tree " + skillId);
      }
    }
  }
}

function isExecutableVendorTree(skill) {
  const files = Array.isArray(skill.files) ? skill.files : [];
  return files.some(function (file) {
    if (typeof file.path !== "string") return false;
    if (file.path.indexOf("/scripts/") >= 0) return true;
    if (/\.(sh|bash)$/.test(file.path)) return true;
    const mode = typeof file.mode === "string" ? parseInt(file.mode, 8) : 0;
    return (mode & 0o111) !== 0;
  });
}

export function validate(state) {
  const errors = [];
  const versions = state.versions.value;

  if (!isPlainObject(versions)) {
    errors.push("missing or invalid " + REL.versions + ": " + (state.versions.error || "not an object"));
    return errors;
  }
  if (CREDENTIAL_KEY.test(state.versions.value ? JSON.stringify(versions) : "")) {
    errors.push("versions.json must not contain credentials or deployment values");
  }

  checkExact("versions schema", versions.schema, LOCKED.schema, errors);
  if (versions.version !== LOCKED.version) errors.push("versions.json version must be " + LOCKED.version);
  checkExact("retrieval date", versions.retrieved_at, LOCKED.retrieved_at, errors);

  if (!isPlainObject(versions.go)) {
    errors.push("versions.json is missing the go lock");
  } else {
    checkExact("Go toolchain", versions.go.toolchain, LOCKED.go.toolchain, errors);
    checkImageRef("Go builder", versions.go.builder_image, LOCKED.go.builder_image, errors);
  }
  if (!isPlainObject(versions.node)) {
    errors.push("versions.json is missing the node lock");
  } else {
    checkImageRef("Node runtime", versions.node.runtime_image, LOCKED.node.runtime_image, errors);
  }

  if (!isPlainObject(versions.hyperframes_source)) {
    errors.push("versions.json is missing the HyperFrames source lock");
  } else {
    checkExact("HyperFrames tag", versions.hyperframes_source.tag, LOCKED.hyperframes_source.tag, errors);
    checkExact("HyperFrames commit", versions.hyperframes_source.commit, LOCKED.hyperframes_source.commit, errors);
    checkExact("HyperFrames license", versions.hyperframes_source.license, LOCKED.hyperframes_source.license, errors);
    checkExact("HyperFrames node engine", versions.hyperframes_source.node_engine, LOCKED.hyperframes_source.node_engine, errors);
  }
  checkExact("skills CLI version", versions.skills_cli, LOCKED.skills_cli, errors);

  if (!isPlainObject(versions.debian_snapshot)) {
    errors.push("versions.json is missing the Debian snapshot lock");
  } else {
    if (versions.debian_snapshot.timestamp !== LOCKED.debian_snapshot) {
      errors.push(
        "Debian snapshot date mismatch: lock=" + JSON.stringify(versions.debian_snapshot.timestamp) + " audited=" + JSON.stringify(LOCKED.debian_snapshot),
      );
    }
    if (JSON.stringify(versions.debian_snapshot.architectures) !== JSON.stringify(LOCKED.apt_architectures)) {
      errors.push("Debian snapshot architectures mismatch: " + JSON.stringify(versions.debian_snapshot.architectures));
    }
    if (JSON.stringify(versions.debian_snapshot.packages) !== JSON.stringify(LOCKED.apt_packages)) {
      errors.push("Debian snapshot package set mismatch: " + JSON.stringify(versions.debian_snapshot.packages));
    }
  }

  // versions.json runtime package set.
  const lockedRuntime = {};
  for (const [name, version] of Object.entries(LOCKED.runtime_packages)) lockedRuntime[name] = version;
  for (const [name, linkage] of Object.entries(LOCKED.runtime_workspace_dependencies)) lockedRuntime[name] = linkage;
  const declaredRuntime = isPlainObject(versions.runtime_packages) ? versions.runtime_packages : {};
  for (const name of Object.keys(LOCKED.runtime_packages)) {
    checkExact("runtime package " + name, declaredRuntime[name], LOCKED.runtime_packages[name], errors);
  }
  const declaredWorkspace = isPlainObject(versions.runtime_workspace_dependencies) ? versions.runtime_workspace_dependencies : {};
  for (const name of Object.keys(LOCKED.runtime_workspace_dependencies)) {
    checkExact("workspace dependency " + name, declaredWorkspace[name], LOCKED.runtime_workspace_dependencies[name], errors);
  }

  // server/go.mod Go directive.
  const goMatch = typeof state.goModText === "string" ? /^go\s+([0-9]+\.[0-9]+\.[0-9]+)\s*$/m.exec(state.goModText) : null;
  if (!goMatch) {
    errors.push("server/go.mod is missing a Go version directive");
  } else if (isPlainObject(versions.go)) {
    if (goMatch[1] !== versions.go.toolchain || goMatch[1] !== LOCKED.go.toolchain) {
      errors.push(
        "Go version mismatch: go.mod=" + goMatch[1] + " versions.json=" + JSON.stringify(versions.go.toolchain) + " audited=" + LOCKED.go.toolchain,
      );
    }
  }

  // runtime package.json production dependencies.
  const runtimePackage = state.runtimePackage.value;
  if (!isPlainObject(runtimePackage) || !isPlainObject(runtimePackage.dependencies)) {
    errors.push("missing or invalid " + REL.runtimePackage + ": " + (state.runtimePackage.error || "no dependencies"));
  } else {
    const deps = runtimePackage.dependencies;
    for (const name of Object.keys(deps)) {
      if (!(name in lockedRuntime)) {
        errors.push("unexpected production dependency: " + name + " (not part of the audited runtime package set)");
      }
    }
    for (const [name, version] of Object.entries(LOCKED.runtime_packages)) {
      if (!(name in deps)) {
        errors.push("missing " + name + " in " + REL.runtimePackage + " (audited version " + version + ")");
        continue;
      }
      if (!EXACT_SEMVER.test(deps[name])) {
        errors.push("runtime dependency " + name + " must be an exact version: " + JSON.stringify(deps[name]));
      } else if (deps[name] !== version) {
        errors.push("runtime dependency " + name + " does not match the audited lock: lock=" + deps[name] + " audited=" + version);
      }
    }
    for (const [name, linkage] of Object.entries(LOCKED.runtime_workspace_dependencies)) {
      if (!(name in deps)) {
        errors.push("missing " + name + " in " + REL.runtimePackage);
      } else if (deps[name] !== linkage) {
        errors.push("runtime dependency " + name + " must be " + linkage + ": " + JSON.stringify(deps[name]));
      }
    }

    const importer = parsePnpmRuntimeImporter(state.pnpmLockText);
    if (!importer) {
      errors.push("pnpm-lock.yaml is missing the " + RUNTIME_IMPORTER + " importer");
    } else {
      for (const name of Object.keys(lockedRuntime)) {
        const entry = importer[name];
        if (!entry) {
          errors.push("missing " + name + " in the pnpm-lock.yaml runtime importer");
          continue;
        }
        if (entry.specifier !== deps[name]) {
          errors.push("pnpm-lock specifier drift for " + name + ": lock=" + JSON.stringify(entry.specifier) + " package.json=" + JSON.stringify(deps[name]));
        }
        if (typeof entry.version !== "string" || entry.version.length === 0) {
          errors.push("pnpm-lock resolved version is missing for " + name);
          continue;
        }
        if (name in LOCKED.runtime_packages) {
          if (baseVersion(entry.version) !== LOCKED.runtime_packages[name]) {
            errors.push(
              "pnpm-lock resolved version drift for " + name + ": lock=" + entry.version + " audited=" + LOCKED.runtime_packages[name],
            );
          }
          const key = name + "@" + LOCKED.runtime_packages[name];
          const block = findPackageBlock(state.pnpmLockText, key);
          if (!block || !/integrity:\s*sha512-/.test(block)) {
            errors.push("pnpm-lock.yaml is missing a resolved integrity for " + key);
          }
        }
      }
    }
  }

  // Debian apt lock.
  const apt = state.aptLock.value;
  if (!isPlainObject(apt)) {
    errors.push("missing or invalid " + REL.aptLock + ": " + (state.aptLock.error || "not an object"));
  } else {
    checkExact("apt lock schema", apt.schema, "com.multica.aurora.apt-packages-lock", errors);
    if (apt.version !== 1) errors.push("apt-packages.lock version must be 1");
    if (apt.snapshot_timestamp !== LOCKED.debian_snapshot) {
      errors.push(
        "Debian snapshot date mismatch in apt-packages.lock: lock=" + JSON.stringify(apt.snapshot_timestamp) + " audited=" + JSON.stringify(LOCKED.debian_snapshot),
      );
    }
    if (isPlainObject(versions.debian_snapshot) && apt.snapshot_timestamp !== versions.debian_snapshot.timestamp) {
      errors.push("Debian snapshot date drift between versions.json and apt-packages.lock");
    }
    if (JSON.stringify(apt.architectures) !== JSON.stringify(LOCKED.apt_architectures)) {
      errors.push("apt lock architectures mismatch: " + JSON.stringify(apt.architectures));
    }
    const sources = Array.isArray(apt.sources) ? apt.sources : [];
    for (const expected of expectedSources(LOCKED.debian_snapshot)) {
      if (sources.indexOf(expected) < 0) errors.push("apt lock is missing the snapshot source: " + expected);
    }
    const packages = isPlainObject(apt.packages) ? apt.packages : {};
    for (const name of LOCKED.apt_packages) {
      const record = packages[name];
      if (!isPlainObject(record)) {
        errors.push("apt lock is missing package " + name);
        continue;
      }
      for (const arch of LOCKED.apt_architectures) {
        const entry = record[arch];
        if (!isPlainObject(entry)) {
          errors.push("apt lock is missing " + name + " for " + arch);
          continue;
        }
        if (typeof entry.version !== "string" || !/^[0-9]/.test(entry.version)) {
          errors.push("unpinned apt package: " + name + " (" + arch + ") version=" + JSON.stringify(entry.version));
        }
        if (typeof entry.sha256 !== "string" || !DIGEST.test(entry.sha256)) {
          errors.push("unpinned apt package: " + name + " (" + arch + ") sha256=" + JSON.stringify(entry.sha256));
        }
        if (typeof entry.filename !== "string" || entry.filename.length === 0) {
          errors.push("unpinned apt package: " + name + " (" + arch + ") filename is missing");
        }
        if (typeof entry.repository !== "string" || entry.repository.length === 0) {
          errors.push("unpinned apt package: " + name + " (" + arch + ") repository is missing");
        }
      }
    }
    for (const name of Object.keys(packages)) {
      if (LOCKED.apt_packages.indexOf(name) < 0) errors.push("unexpected apt package in the lock: " + name);
    }
  }

  checkVendorTrees(state, versions, errors);

  if (typeof state.pnpmWorkspaceText === "string" && state.pnpmWorkspaceText.indexOf(RUNTIME_IMPORTER) < 0) {
    errors.push("pnpm-workspace.yaml does not include " + RUNTIME_IMPORTER);
  }

  if (state.dockerfile !== null && state.dockerfile !== undefined) {
    checkDockerfile("sandbox", state.dockerfile, versions, state, errors);
  }
  if (state.dockerfileEgress !== null && state.dockerfileEgress !== undefined) {
    checkDockerfile("egress", state.dockerfileEgress, versions, state, errors);
  }

  return errors;
}

function summary(state) {
  let dockerCount = 0;
  if (state.dockerfile !== null && state.dockerfile !== undefined) dockerCount += 1;
  if (state.dockerfileEgress !== null && state.dockerfileEgress !== undefined) dockerCount += 1;
  return (
    "verified aurora sandbox locks: 2 base images, " +
    Object.keys(LOCKED.runtime_packages).length +
    " runtime packages, " +
    LOCKED.apt_packages.length +
    " apt packages on " +
    LOCKED.apt_architectures.length +
    " architectures, " +
    Object.keys(LOCKED.vendor_trees).length +
    " vendor trees, " +
    dockerCount +
    " dockerfile(s)"
  );
}

export async function verify(overrides = {}) {
  const state = loadLockState(overrides.root || REPO_ROOT);
  if (overrides.state) deepAssign(state, overrides.state);
  applyFlatOverrides(state, overrides);
  const errors = validate(state);
  if (errors.length > 0) throw new Error(errors.join("\n"));
  return summary(state);
}

async function main() {
  try {
    const result = await verify();
    console.log(result);
  } catch (error) {
    console.error(error.message);
    console.error("");
    console.error((error.message.match(/\n/g) || []).length + 1 + " verification error(s)");
    process.exit(1);
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
