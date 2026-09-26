// Versioned artifact manifest contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createManifest, MANIFEST_RELATIVE_PATH, ManifestError } from '../src/manifest.mjs';
import { makeWorkspace, writeInput, writeOutput, pngBytes, sha256Bytes, uuid } from './helpers.mjs';

function manifestFor(ws, overrides = {}) {
  return createManifest({
    outputRoot: ws.outputRoot,
    taskId: uuid(1),
    skillId: 'poster',
    producer: { id: 'byted-ark-seedream-skill', version: '4.0.0', tree_sha256: 'sha256:' + 'a'.repeat(64) },
    ...overrides,
  });
}

test('writes an atomic v1 manifest with computed size and hash', () => {
  const ws = makeWorkspace();
  const bytes = pngBytes(50);
  const file = writeOutput(ws, 'out.png', bytes);
  const manifest = manifestFor(ws);
  manifest.addFile({ id: 'primary-1', path: file, name: 'primary-1.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' });
  const written = manifest.write();
  assert.equal(written, path.join(ws.outputRoot, MANIFEST_RELATIVE_PATH));
  const parsed = JSON.parse(fs.readFileSync(written, 'utf8'));
  assert.equal(parsed.schema, 'com.multica.aurora.artifacts');
  assert.equal(parsed.version, 1);
  assert.equal(parsed.task_id, uuid(1));
  assert.equal(parsed.skill_id, 'poster');
  assert.equal(parsed.producer.id, 'byted-ark-seedream-skill');
  assert.equal(parsed.artifacts.length, 1);
  assert.equal(parsed.artifacts[0].source.type, 'file');
  assert.equal(parsed.artifacts[0].source.relative_path, 'out.png');
  assert.equal(parsed.artifacts[0].size_bytes, bytes.length);
  assert.equal(parsed.artifacts[0].sha256, sha256Bytes(bytes));
  assert.equal(fs.statSync(written).mode & 0o077, 0);
});

test('records staged objects without any provider URL', () => {
  const ws = makeWorkspace();
  const manifest = manifestFor(ws);
  manifest.addStagedObject({
    id: 'primary-1',
    stagingId: uuid(20),
    name: 'primary-1.png',
    kind: 'image',
    role: 'primary',
    format: 'png',
    mimeType: 'image/png',
    sizeBytes: 123,
    sha256: sha256Bytes(Buffer.from('x')),
  });
  manifest.write();
  const parsed = JSON.parse(fs.readFileSync(path.join(ws.outputRoot, MANIFEST_RELATIVE_PATH), 'utf8'));
  assert.deepEqual(parsed.artifacts[0].source, { type: 'staged_object', staging_id: uuid(20) });
  assert.doesNotMatch(JSON.stringify(parsed), /https?:\/\//);
});

test('rejects a manifest with no primary artifact, duplicates, and unsafe names', () => {
  const ws = makeWorkspace();
  const bytes = pngBytes(10);
  const file = writeOutput(ws, 'a.png', bytes);

  const noPrimary = manifestFor(ws);
  noPrimary.addFile({ id: 'side-1', path: file, name: 'a.png', kind: 'image', role: 'supporting', format: 'png', mimeType: 'image/png' });
  assert.throws(() => noPrimary.write(), /primary/);

  const duplicate = manifestFor(ws);
  duplicate.addFile({ id: 'primary-1', path: file, name: 'a.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' });
  assert.throws(
    () => duplicate.addFile({ id: 'primary-1', path: file, name: 'b.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' }),
    /duplicate/,
  );
  assert.throws(
    () => duplicate.addFile({ id: 'primary-2', path: file, name: '../escape.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' }),
    ManifestError,
  );
});

test('rejects files outside the artifact root and symlinks', () => {
  const ws = makeWorkspace();
  const outside = writeInput(ws, 'outside.png', pngBytes(8));
  const manifest = manifestFor(ws);
  assert.throws(
    () => manifest.addFile({ id: 'primary-1', path: outside, name: 'outside.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' }),
    /inside|root|output/,
  );

  const real = writeInput(ws, 'real.png', pngBytes(8));

  const link = path.join(ws.outputRoot, 'link.png');
  fs.symlinkSync(real, link);
  assert.throws(
    () => manifest.addFile({ id: 'primary-1', path: link, name: 'link.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' }),
    /symlink|regular|symbolic/,
  );
});

test('enforces the manifest size and artifact count limits', () => {
  const ws = makeWorkspace();
  const manifest = manifestFor(ws);
  for (let i = 0; i < 20; i += 1) {
    const file = writeOutput(ws, 'a-' + i + '.png', pngBytes(10));
    manifest.addFile({ id: 'a-' + i, path: file, name: 'a-' + i + '.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' });
  }
  const extra = writeOutput(ws, 'a-extra.png', pngBytes(10));
  assert.throws(
    () => manifest.addFile({ id: 'a-21', path: extra, name: 'a-21.png', kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' }),
    /20|count|artifacts/,
  );
});
