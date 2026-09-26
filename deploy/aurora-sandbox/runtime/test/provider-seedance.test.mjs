// Seedance adapter fake-provider contract tests.
//
// The Seedance vendor tree is deliberately absent (licence gate, issue #140),
// so these tests drive the adapter against an injected fake vendor module and a
// fake provider-run client. A missing vendor module must fail closed.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import { makeWorkspace, baseContext, writeContextFile, writeInput, attachmentEntry, uuid, pngBytes, readManifest } from './helpers.mjs';

const IMAGE_ID = uuid(9);
const EXTERNAL_ID = 'cgt-20260926120000-abc12';

function fakeProviderRunOrdered(events) {
  const calls = [];
  let current = null;
  return {
    calls,
    async get(operation) {
      calls.push({ kind: 'get', operation });
      if (!current) {
        const error = new Error('not found');
        error.status = 404;
        throw error;
      }
      return { ...current, operation };
    },
    async begin(input) {
      events.push('begin');
      calls.push({ kind: 'begin', ...input });
      current = { ...input, external_id: null, state: 'creating', create_allowed: true };
      return { ...current };
    },
    async recordExternal(operation, id) {
      calls.push({ kind: 'recordExternal', operation, externalId: id });
      current = { ...current, operation, external_id: id, state: 'submitted' };
      return { ...current };
    },
    async finish(operation, state, errorCode) {
      calls.push({ kind: 'finish', operation, state, errorCode });
      current = { ...current, operation, state };
      return { ...current };
    },
  };
}

function fakeSeedanceVendor(events, options = {}) {
  const calls = [];
  const outputs = options.outputs || [{ kind: 'video', url: 'https://cdn.example.com/out.mp4' }];
  return {
    calls,
    API_ORIGIN: 'https://ark.cn-beijing.volces.com',
    ALLOWED_MODELS: ['doubao-seedance-2.0', 'doubao-seedance-2.0-fast', 'doubao-seedance-2.0-mini', 'doubao-seedance-2.5'],
    PRODUCER_ID: 'byted-ark-seedance-skill',
    PRODUCER_VERSION: '5.0.0',
    async createTask(options) {
      events.push('create');
      calls.push({ kind: 'createTask', options });
      return { provider: 'volcengine-agentplan', model: options.input.model, external_id: EXTERNAL_ID, status: 'queued', outputs: [] };
    },
    async pollTask(options) {
      calls.push({ kind: 'pollTask', options });
      return { provider: 'volcengine-agentplan', model: 'doubao-seedance-2.0', external_id: options.taskId, status: 'succeeded', outputs };
    },
    producerMetadata(treeSha256) {
      return { id: 'byted-ark-seedance-skill', version: '5.0.0', tree_sha256: treeSha256 || null };
    },
  };
}

function setup(ws) {
  const imageBytes = pngBytes(40);
  writeInput(ws, 'ref.png', imageBytes);
  fs.writeFileSync(path.join(ws.secrets, 'ark-api-key'), 'ark-test-key-abcdef', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'ark-api-key'), 0o400);
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
  const context = baseContext(ws, {
    skill_id: 'image-video',
    attachments: { [IMAGE_ID]: attachmentEntry('ref.png', 'image/png', imageBytes.length) },
  });
  return {
    contextPath: writeContextFile(ws, context),
    secretPaths: {
      ark: path.join(ws.secrets, 'ark-api-key'),
      openai: path.join(ws.secrets, 'openai-api-key'),
      volcAsr: path.join(ws.secrets, 'volc-asr-api-key'),
      taskToken: path.join(ws.secrets, 'task-token'),
    },
  };
}

function build(ws, { vendor, providerRun, imported }) {
  const { contextPath, secretPaths } = setup(ws);
  return createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths,
    fetchImpl: async () => { throw new Error('Seedance must not fetch without a poll'); },
    providerRun,
    vendor,
    importer: async (request) => {
      imported.push(request);
      return { staging_id: uuid(41), name: request.name, size_bytes: 4, sha256: 'sha256:' + 'd'.repeat(64) };
    },
  });
}

test('Seedance begins the run before create, records cgt id immediately, then polls', async () => {
  const ws = makeWorkspace();
  const events = [];
  const providerRun = fakeProviderRunOrdered(events);
  const vendor = fakeSeedanceVendor(events);
  const imported = [];
  const broker = build(ws, { vendor: { seedanceModule: vendor }, providerRun, imported });
  const result = await broker.dispatch('aurora.seedance_generate', { prompt: 'a moving cat', attachment_ids: [IMAGE_ID] });

  assert.deepEqual(events, ['begin', 'create']);
  assert.equal(vendor.calls.filter((entry) => entry.kind === 'createTask').length, 1);
  assert.equal(vendor.calls[0].options.input.model, 'doubao-seedance-2.0');
  assert.equal(vendor.calls[0].options.env.ARK_API_KEY, 'ark-test-key-abcdef');
  // The key never travels in argv or tool input.
  assert.equal(JSON.stringify(vendor.calls[0].options.input).includes('ark-test-key'), false);
  const poll = vendor.calls.find((entry) => entry.kind === 'pollTask');
  assert.equal(poll.options.taskId, EXTERNAL_ID);
  assert.ok(
    providerRun.calls.findIndex((entry) => entry.kind === 'recordExternal' && entry.externalId === EXTERNAL_ID) <
      providerRun.calls.findIndex((entry) => entry.kind === 'finish'),
  );
  assert.equal(imported.length, 1);
  assert.doesNotMatch(JSON.stringify(result), /cdn\.example\.com/);
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].kind, 'video');
  assert.equal(manifest.artifacts[0].source.type, 'staged_object');
});

test('Seedance resumes an existing submitted run instead of creating again', async () => {
  const ws = makeWorkspace();
  const events = [];
  const providerRun = fakeProviderRunOrdered(events);
  const existing = {
    provider: 'volcengine-agentplan',
    operation: 'seedance.create',
    model: 'doubao-seedance-2.0',
    state: 'submitted',
    external_id: EXTERNAL_ID,
    create_allowed: false,
  };
  providerRun.get = async (operation) => ({ ...existing, operation });
  const vendor = fakeSeedanceVendor(events);
  const imported = [];
  const broker = build(ws, { vendor: { seedanceModule: vendor }, providerRun, imported });
  await broker.dispatch('aurora.seedance_generate', { prompt: 'x', attachment_ids: [IMAGE_ID] });

  assert.equal(events.includes('create'), false);
  assert.equal(vendor.calls.some((entry) => entry.kind === 'createTask'), false);
  assert.equal(vendor.calls.find((entry) => entry.kind === 'pollTask').options.taskId, EXTERNAL_ID);
});

test('Seedance fails closed on an ambiguous create lease', async () => {
  const ws = makeWorkspace();
  const events = [];
  const providerRun = fakeProviderRunOrdered(events);
  providerRun.begin = async (input) => {
    events.push('begin');
    providerRun.calls.push({ kind: 'begin', ...input });
    return { ...input, external_id: null, state: 'creating', create_allowed: false };
  };
  const vendor = fakeSeedanceVendor(events);
  const imported = [];
  const broker = build(ws, { vendor: { seedanceModule: vendor }, providerRun, imported });
  await assert.rejects(broker.dispatch('aurora.seedance_generate', { prompt: 'x', attachment_ids: [IMAGE_ID] }), /ambig|lease|create/i);
  assert.equal(events.includes('create'), false);
  assert.equal(imported.length, 0);
});

test('Seedance fails closed when the vendor module is absent', async () => {
  const ws = makeWorkspace();
  const events = [];
  const providerRun = fakeProviderRunOrdered(events);
  const imported = [];
  const broker = build(ws, {
    vendor: { vendorDir: path.join(ws.root, 'missing-vendor') },
    providerRun,
    imported,
  });
  await assert.rejects(broker.dispatch('aurora.seedance_generate', { prompt: 'x', attachment_ids: [IMAGE_ID] }), /Seedance|vendor|unavailable|licensed|absent/i);
  assert.equal(events.length, 0);
});
