// Atomic, versioned artifact manifest writer.
//
// Only this library may author the manifest; the model never supplies JSON.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { LIMITS, assertArtifactName, isUuid } from './policy.mjs';

export const MANIFEST_SCHEMA = 'com.multica.aurora.artifacts';
export const MANIFEST_VERSION = 1;
export const MANIFEST_RELATIVE_PATH = '.multica/aurora-artifacts.v1.json';

export class ManifestError extends Error {}

function fail(message) {
  throw new ManifestError(message);
}

function sha256(buffer) {
  return 'sha256:' + crypto.createHash('sha256').update(buffer).digest('hex');
}

function artifactRole(role) {
  if (!['primary', 'supporting', 'transcript'].includes(role)) fail('invalid artifact role');
  return role;
}

export function createManifest({ outputRoot, taskId, skillId, producer }) {
  if (typeof outputRoot !== 'string' || outputRoot.length === 0) fail('output root is required');
  if (!isUuid(taskId)) fail('manifest task id is invalid');
  if (!producer || typeof producer.id !== 'string') fail('manifest producer is required');
  const resolvedRoot = path.resolve(outputRoot);
  const artifacts = [];
  const ids = new Set();
  const names = new Set();
  const relativePaths = new Set();

  function assertUnique(id, name, relativePath) {
    if (typeof id !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(id)) fail('invalid artifact id');
    if (ids.has(id)) fail('duplicate artifact id: ' + id);
    if (names.has(name)) fail('duplicate artifact name: ' + name);
    if (relativePath && relativePaths.has(relativePath)) fail('duplicate artifact path: ' + relativePath);
  }

  function push(artifact) {
    if (artifacts.length >= LIMITS.maxArtifacts) fail('artifact count exceeds the 20 limit');
    ids.add(artifact.id);
    names.add(artifact.name);
    if (artifact.source.type === 'file') relativePaths.add(artifact.source.relative_path);
    artifacts.push(artifact);
  }

  return {
    addFile({ id, path: filePath, name, kind, role, format, mimeType, metadata = {} }) {
      try {
        assertArtifactName(name);
      } catch {
        fail('artifact name is not permitted');
      }
      role = artifactRole(role);
      if (typeof filePath !== 'string') fail('artifact file path is required');
      const resolved = path.resolve(filePath);
      if (resolved !== resolvedRoot && !resolved.startsWith(resolvedRoot + path.sep)) {
        fail('artifact file must live inside the output root');
      }
      let stats;
      try {
        stats = fs.lstatSync(resolved);
      } catch {
        fail('artifact file does not exist');
      }
      if (stats.isSymbolicLink()) fail('artifact file must not be a symlink');
      if (!stats.isFile()) fail('artifact file must be a regular file');
      const relativePath = path.relative(resolvedRoot, resolved).split(path.sep).join('/');
      assertUnique(id, name, relativePath);
      const bytes = fs.readFileSync(resolved);
      push({
        id,
        source: { type: 'file', relative_path: relativePath },
        name,
        kind,
        role,
        format,
        mime_type: mimeType,
        size_bytes: bytes.length,
        sha256: sha256(bytes),
        metadata,
      });
      return this;
    },

    addStagedObject({ id, stagingId, name, kind, role, format, mimeType, sizeBytes, sha256: digest, metadata = {} }) {
      try {
        assertArtifactName(name);
      } catch {
        fail('artifact name is not permitted');
      }
      role = artifactRole(role);
      if (!isUuid(stagingId)) fail('staged object id is invalid');
      if (!Number.isInteger(sizeBytes) || sizeBytes < 0) fail('staged object size is invalid');
      if (typeof digest !== 'string' || !/^sha256:[0-9a-f]{64}$/.test(digest)) {
        digest = 'sha256:' + '0'.repeat(64);
      }
      assertUnique(id, name, null);
      push({
        id,
        source: { type: 'staged_object', staging_id: stagingId },
        name,
        kind,
        role,
        format,
        mime_type: mimeType,
        size_bytes: sizeBytes,
        sha256: digest,
        metadata,
      });
      return this;
    },

    toJSON() {
      return {
        schema: MANIFEST_SCHEMA,
        version: MANIFEST_VERSION,
        task_id: taskId,
        skill_id: skillId,
        producer: { id: producer.id, version: producer.version, tree_sha256: producer.tree_sha256 ?? null },
        provider_run: this.providerRun || null,
        artifacts: artifacts.map((artifact) => ({ ...artifact })),
      };
    },

    setProviderRun(run) {
      this.providerRun = run;
      return this;
    },

    write() {
      if (!artifacts.some((artifact) => artifact.role === 'primary')) fail('manifest requires at least one primary artifact');
      const total = artifacts.reduce((sum, artifact) => sum + artifact.size_bytes, 0);
      if (total > LIMITS.maxTotalArtifactBytes) fail('manifest total size exceeds the 600 MiB limit');
      const payload = JSON.stringify(this.toJSON(), null, 2);
      const bytes = Buffer.byteLength(payload, 'utf8');
      if (bytes > LIMITS.maxManifestBytes) fail('manifest exceeds 1 MiB');
      const directory = path.join(resolvedRoot, '.multica');
      fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
      const target = path.join(resolvedRoot, MANIFEST_RELATIVE_PATH);
      const temporary = path.join(directory, '.aurora-artifacts.' + crypto.randomUUID() + '.tmp');
      fs.writeFileSync(temporary, payload, { mode: 0o600 });
      fs.chmodSync(temporary, 0o600);
      fs.renameSync(temporary, target);
      return target;
    },
  };
}
