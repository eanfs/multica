// Seedream adapter fake-provider contract tests.
//
// The real patched vendor module is materialized from the vendored tree and
// driven against a local fake HTTP server. Nothing contacts Volcengine.

import { test, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import {
  makeWorkspace,
  baseContext,
  writeContextFile,
  writeInput,
  attachmentEntry,
  uuid,
  pngBytes,
  materializePatchedVendor,
  loadCjsModule,
  fakeProviderRun,
  readManifest,
  startFakeServer,
} from './helpers.mjs';

const PATCHED_DIR = materializePatchedVendor();
const seedreamModule = loadCjsModule(path.join(PATCHED_DIR, 'byted-ark-seedream-skill/scripts/seedream-broker.js'));
after(() => fs.rmSync(PATCHED_DIR, { recursive: true, force: true }));

const ARK_KEY = 'ark-test-key-abcdef';
const IMAGE_ID = uuid(9);

function setup(ws) {
  const imageBytes = pngBytes(48);
  writeInput(ws, 'ref.png', imageBytes);
  fs.writeFileSync(path.join(ws.secrets, 'ark-api-key'), ARK_KEY, { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'ark-api-key'), 0o400);
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
  const context = baseContext(ws, {
    skill_id: 'poster',
    attachments: { [IMAGE_ID]: attachmentEntry('ref.png', 'image/png', imageBytes.length) },
  });
  const contextPath = writeContextFile(ws, context);
  return {
    contextPath,
    secretPaths: {
      ark: path.join(ws.secrets, 'ark-api-key'),
      openai: path.join(ws.secrets, 'openai-api-key'),
      volcAsr: path.join(ws.secrets, 'volc-asr-api-key'),
      taskToken: path.join(ws.secrets, 'task-token'),
    },
  };
}

function arkHandler(response, options = {}) {
  const status = options.status || 200;
  const headers = options.headers || {};
  return async (req, res) => {
    res.statusCode = status;
    for (const [key, value] of Object.entries(headers)) res.setHeader(key, value);
    res.setHeader('content-type', 'application/json');
    if (options.delayMs) await new Promise((resolve) => setTimeout(resolve, options.delayMs));
    res.end(typeof response === 'string' ? response : JSON.stringify(response));
  };
}

async function runSeedream(ws, fake, overrides = {}) {
  const { contextPath, secretPaths } = setup(ws);
  const providerRun = overrides.providerRun || fakeProviderRun();
  const imported = [];
  const broker = createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths,
    fetchImpl: fake.fetchImpl,
    providerRun,
    vendor: { seedreamModule },
    importer: async (request) => {
      imported.push(request);
      return { staging_id: uuid(40 + imported.length), name: request.name, size_bytes: 12, sha256: 'sha256:' + 'c'.repeat(64) };
    },
    providerTimeoutMs: overrides.providerTimeoutMs,
  });
  const result = await broker.dispatch('aurora.seedream_generate', {
    prompt: 'a poster of a cat',
    attachment_ids: [IMAGE_ID],
    ...overrides.args,
  });
  return { broker, result, imported, providerRun, contextPath, secretPaths };
}

test('Seedream posts the exact method, path, headers, and body', async () => {
  const ws = makeWorkspace();
  const fake = await startFakeServer(arkHandler({ id: 'seedream-1', model: 'doubao-seedream-5.0-pro', data: [{ url: 'https://cdn.example.com/result.png' }] }));
  try {
    const { broker, result, imported, providerRun } = await runSeedream(ws, fake);
    assert.equal(fake.calls.length, 1);
    const call = fake.calls[0];
    assert.equal(call.method, 'POST');
    assert.equal(call.url, '/api/plan/v3/images/generations');
    assert.equal(call.headers.authorization, 'Bearer ' + ARK_KEY);
    const body = JSON.parse(call.bodyText);
    assert.equal(body.model, 'doubao-seedream-5.0-pro');
    assert.equal(body.prompt, 'a poster of a cat');
    assert.equal(body.response_format, 'url');
    assert.match(body.image, /^data:image\/png;base64,/);

    // Create-once: one begin, no create before the lease.
    assert.equal(providerRun.calls.filter((entry) => entry.kind === 'begin').length, 1);
    assert.equal(providerRun.calls[0].provider, 'volcengine-agentplan');
    assert.equal(providerRun.calls[0].operation, 'seedream.generate');
    assert.equal(providerRun.calls[0].model, 'doubao-seedream-5.0-pro');
    assert.ok(providerRun.calls.some((entry) => entry.kind === 'finish' && entry.state === 'succeeded'));

    // The provider URL is imported and never returned to the model.
    assert.equal(imported.length, 1);
    assert.equal(imported[0].url, 'https://cdn.example.com/result.png');
    assert.doesNotMatch(JSON.stringify(result), /cdn\.example\.com/);
    const manifest = readManifest(ws.outputRoot);
    assert.equal(manifest.artifacts.length, 1);
    assert.equal(manifest.artifacts[0].source.type, 'staged_object');
    assert.equal(manifest.artifacts[0].role, 'primary');
    assert.equal(manifest.producer.id, 'byted-ark-seedream-skill');
  } finally {
    await fake.close();
  }
});

test('Seedream refuses an unapproved redirect and never calls the importer', async () => {
  const ws = makeWorkspace();
  const fake = await startFakeServer(arkHandler('', { status: 302, headers: { location: 'https://evil.example/steal' } }));
  try {
    await assert.rejects(runSeedream(ws, fake), (error) => {
      assert.doesNotMatch(error.message, /evil\.example/);
      return /redirect|origin/i.test(error.message);
    });
  } finally {
    await fake.close();
  }
});

test('Seedream refuses a non-HTTPS provider result URL before import', async () => {
  const ws = makeWorkspace();
  const fake = await startFakeServer(arkHandler({ data: [{ url: 'http://cdn.example.com/result.png' }] }));
  try {
    await assert.rejects(runSeedream(ws, fake), /HTTPS/i);
  } finally {
    await fake.close();
  }
});

test('Seedream enforces the provider response size cap', async () => {
  const ws = makeWorkspace();
  const fake = await startFakeServer(arkHandler({ data: 'x'.repeat(1024 * 1024 + 64) }));
  try {
    await assert.rejects(runSeedream(ws, fake), /buffer cap|size|large/);
  } finally {
    await fake.close();
  }
});

test('Seedream times out bounded and sanitizes provider errors', async () => {
  const ws = makeWorkspace();
  const slow = await startFakeServer(arkHandler({ data: [] }, { delayMs: 250 }));
  try {
    await assert.rejects(runSeedream(ws, slow, { providerTimeoutMs: 25 }));
  } finally {
    await slow.close();
  }
  const erroring = await startFakeServer(arkHandler('failed with ark-abcdefgh and data:image/png;base64,AAAA', { status: 500 }));
  try {
    await assert.rejects(runSeedream(ws, erroring), (error) => {
      assert.doesNotMatch(error.message, /ark-abcdefgh/);
      assert.doesNotMatch(error.message, /data:image/);
      return true;
    });
  } finally {
    await erroring.close();
  }
});
