// Bounded task-context and secret contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {
  loadTaskContext,
  readSecret,
  TASK_CONTEXT_SCHEMA,
  MAX_CONTEXT_BYTES,
} from '../src/task-context.mjs';
import {
  makeWorkspace,
  baseContext,
  writeContextFile,
  writeInput,
  attachmentEntry,
  uuid,
  pngBytes,
} from './helpers.mjs';

function options(ws, contextPath, extra = {}) {
  return {
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    allowedSecretPaths: [path.join(ws.secrets, 'task-token'), path.join(ws.secrets, 'ark-api-key')],
    ...extra,
  };
}

test('loads a bounded context and resolves authorized attachments', () => {
  const ws = makeWorkspace();
  const bytes = pngBytes(32);
  writeInput(ws, 'ref.png', bytes);
  const attachmentId = uuid(9);
  const context = baseContext(ws, {
    attachments: { [attachmentId]: attachmentEntry('ref.png', 'image/png', bytes.length) },
  });
  const contextPath = writeContextFile(ws, context);
  const loaded = loadTaskContext(options(ws, contextPath));
  assert.equal(loaded.schema, TASK_CONTEXT_SCHEMA);
  assert.equal(loaded.taskId, context.task_id);
  assert.equal(loaded.skillId, 'poster');
  const attachment = loaded.resolveAttachment(attachmentId);
  assert.equal(attachment.kind, 'image');
  assert.equal(attachment.relativePath, 'ref.png');
  assert.ok(attachment.absolutePath.startsWith(ws.inputRoot + path.sep));
  assert.throws(() => loaded.resolveAttachment(uuid(10)), /not authorized/);
});

test('rejects unknown fields, wrong schema, and invalid ids', () => {
  const ws = makeWorkspace();
  const good = baseContext(ws);
  for (const [label, mutate] of [
    ['unknown top-level field', (c) => ({ ...c, extra: 1 })],
    ['unknown attachment field', (c) => ({ ...c, attachments: { [uuid(9)]: { relative_path: 'a.png', mime_type: 'image/png', size_bytes: 1, extra: 1 } } })],
    ['bad schema', (c) => ({ ...c, schema: 'wrong' })],
    ['bad task id', (c) => ({ ...c, task_id: 'not-a-uuid' })],
    ['bad skill', (c) => ({ ...c, skill_id: 'ppt' })],
    ['missing token path', (c) => ({ ...c, task_token_file: null })],
  ]) {
    const contextPath = writeContextFile(ws, mutate(good), { name: label.replace(/[^a-z]/gi, '-') + '.json' });
    assert.throws(() => loadTaskContext(options(ws, contextPath)), undefined, label);
  }
});

test('rejects a symlinked, over-permissive, or oversized context file', () => {
  const ws = makeWorkspace();
  const context = baseContext(ws);
  const real = writeContextFile(ws, context, { mode: 0o400, name: 'real.json' });
  const link = path.join(ws.root, 'link.json');
  fs.symlinkSync(real, link);
  assert.throws(() => loadTaskContext(options(ws, link)), /symlink|symbolic|regular/);

  const open = writeContextFile(ws, context, { mode: 0o644, name: 'open.json' });
  assert.throws(() => loadTaskContext(options(ws, open)), /permission|mode/);

  const oversized = path.join(ws.root, 'big.json');
  fs.writeFileSync(oversized, Buffer.alloc(MAX_CONTEXT_BYTES + 1, 0x20), { mode: 0o400 });
  fs.chmodSync(oversized, 0o400);
  assert.throws(() => loadTaskContext(options(ws, oversized)), /size|large|MiB/);
});

test('rejects attachment paths outside the authorized input root', () => {
  const ws = makeWorkspace();
  for (const [index, bad] of ['../escape.png', '/etc/passwd', 'a/../../b.png'].entries()) {
    const context = baseContext(ws, {
      attachments: { [uuid(9)]: attachmentEntry(bad, 'image/png', 10) },
    });
    const contextPath = writeContextFile(ws, context, { name: 'escape-' + index + '.json' });
    assert.throws(() => loadTaskContext(options(ws, contextPath)), /input|escape|absolute|relative/, bad);
  }
});

test('rejects unsupported attachment kinds and policy mismatches', () => {
  const ws = makeWorkspace();
  writeInput(ws, 'a.exe', Buffer.from('MZ'));
  const unsupported = baseContext(ws, {
    attachments: { [uuid(9)]: attachmentEntry('a.exe', 'application/octet-stream', 2) },
  });
  assert.throws(() => loadTaskContext(options(ws, writeContextFile(ws, unsupported, { name: 'u.json' }))), /unsupported/);

  const editNoImage = baseContext(ws, { skill_id: 'image-edit', attachments: {} });
  assert.throws(() => loadTaskContext(options(ws, writeContextFile(ws, editNoImage, { name: 'e.json' }))), /image-edit/);
});

test('rejects a context origin or output root that mismatches the fixed policy', () => {
  const ws = makeWorkspace();
  const badOrigin = baseContext(ws, { server_origin: 'https://evil.example' });
  assert.throws(() => loadTaskContext(options(ws, writeContextFile(ws, badOrigin, { name: 'o.json' }))), /origin/);
  const badRoot = baseContext(ws, { output_root: '/tmp/somewhere' });
  assert.throws(() => loadTaskContext(options(ws, writeContextFile(ws, badRoot, { name: 'r.json' }))), /output/);
});

test('secret helper reads only compiled paths and trims at most 4 KiB', () => {
  const ws = makeWorkspace();
  const allowed = path.join(ws.secrets, 'ark-api-key');
  fs.writeFileSync(allowed, '  ark-abcdefgh  \n', { mode: 0o400 });
  fs.chmodSync(allowed, 0o400);
  assert.equal(readSecret(allowed, { allowedPaths: [allowed] }), 'ark-abcdefgh');

  const other = path.join(ws.root, 'other-secret');
  fs.writeFileSync(other, 'nope', { mode: 0o400 });
  assert.throws(() => readSecret(other, { allowedPaths: [allowed] }), /compiled|allowed|path/);

  const tooLong = path.join(ws.secrets, 'too-long');
  fs.writeFileSync(tooLong, 'x'.repeat(4097), { mode: 0o400 });
  assert.throws(() => readSecret(tooLong, { allowedPaths: [tooLong] }), /size|4 KiB|4096/);

  // Generic environment variables and home configuration are never consulted.
  const previous = process.env.ARK_API_KEY;
  process.env.ARK_API_KEY = 'ark-generic-env-must-not-be-read';
  try {
    assert.throws(() => readSecret(path.join(ws.root, 'missing'), { allowedPaths: [allowed] }), /compiled|allowed|path/);
  } finally {
    if (previous === undefined) delete process.env.ARK_API_KEY;
    else process.env.ARK_API_KEY = previous;
  }
});
