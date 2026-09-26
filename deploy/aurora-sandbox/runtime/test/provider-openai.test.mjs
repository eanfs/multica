// OpenAI Images adapter fake-provider contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import { makeWorkspace, baseContext, writeContextFile, writeInput, attachmentEntry, uuid, pngBytes, createFetchRecorder, fakeProviderRun, readManifest } from './helpers.mjs';

const OPENAI_KEY = 'sk-test-openai-key';
const IMAGE_ID = uuid(9);
const B64 = Buffer.from('fake-png-output').toString('base64');

function setup(ws, skillId, attachments) {
  const context = baseContext(ws, { skill_id: skillId, attachments });
  fs.writeFileSync(path.join(ws.secrets, 'openai-api-key'), OPENAI_KEY, { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'openai-api-key'), 0o400);
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
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

function build(ws, { skillId, attachments, fetchImpl }) {
  const { contextPath, secretPaths } = setup(ws, skillId, attachments);
  const providerRun = fakeProviderRun();
  const broker = createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths,
    fetchImpl,
    providerRun,
    vendor: {},
    importer: async () => { throw new Error('OpenAI returns b64_json and must not import'); },
  });
  return { broker, providerRun };
}

function bodyJson(call) {
  const body = call.init.body;
  if (typeof body === 'string') return JSON.parse(body);
  if (Buffer.isBuffer(body)) return JSON.parse(body.toString('utf8'));
  return null;
}

test('product-image with no attachment uses generation with b64_json and the fixed model', async () => {
  const ws = makeWorkspace();
  const fetchImpl = createFetchRecorder(() => ({ body: { created: 1, data: [{ b64_json: B64 }] } }));
  const { broker, providerRun } = build(ws, { skillId: 'product-image', attachments: {}, fetchImpl });
  const result = await broker.dispatch('aurora.openai_image', { prompt: 'a product shot' });
  assert.equal(fetchImpl.calls.length, 1);
  const call = fetchImpl.calls[0];
  assert.equal(call.method, 'POST');
  assert.equal(call.url, 'https://api.openai.com/v1/images/generations');
  assert.equal(call.headers.authorization, 'Bearer ' + OPENAI_KEY);
  const body = bodyJson(call);
  assert.equal(body.model, 'gpt-image-2.5-sunburst');
  assert.equal(body.prompt, 'a product shot');
  assert.equal(body.response_format, 'b64_json');
  assert.equal(providerRun.calls.filter((entry) => entry.kind === 'begin').length, 1);
  assert.equal(providerRun.calls[0].operation, 'images.generate');
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].source.type, 'file');
  assert.equal(manifest.artifacts[0].kind, 'image');
  assert.doesNotMatch(JSON.stringify(result), new RegExp(B64.slice(0, 12)));
});

test('product-image with an attachment uses the edit endpoint', async () => {
  const ws = makeWorkspace();
  const imageBytes = pngBytes(30);
  writeInput(ws, 'product.png', imageBytes);
  const fetchImpl = createFetchRecorder(() => ({ body: { created: 1, data: [{ b64_json: B64 }] } }));
  const { broker, providerRun } = build(ws, {
    skillId: 'product-image',
    attachments: { [IMAGE_ID]: attachmentEntry('product.png', 'image/png', imageBytes.length) },
    fetchImpl,
  });
  await broker.dispatch('aurora.openai_image', { prompt: 'edit it', attachment_ids: [IMAGE_ID] });
  const call = fetchImpl.calls[0];
  assert.equal(call.url, 'https://api.openai.com/v1/images/edits');
  assert.equal(call.method, 'POST');
  assert.match(call.headers['content-type'] || '', /multipart\/form-data/);
  assert.match(call.bodyText, /name="model"[\s\S]*gpt-image-2\.5-sunburst/);
  assert.match(call.bodyText, /name="response_format"[\s\S]*b64_json/);
  assert.match(call.bodyText, /name="image/);
  assert.equal(providerRun.calls[0].operation, 'images.edit');
});

test('image-edit always uses the edit endpoint', async () => {
  const ws = makeWorkspace();
  const imageBytes = pngBytes(30);
  writeInput(ws, 'src.png', imageBytes);
  const fetchImpl = createFetchRecorder(() => ({ body: { data: [{ b64_json: B64 }] } }));
  const { broker } = build(ws, {
    skillId: 'image-edit',
    attachments: { [IMAGE_ID]: attachmentEntry('src.png', 'image/png', imageBytes.length) },
    fetchImpl,
  });
  await broker.dispatch('aurora.openai_image', { prompt: 'make it blue', attachment_ids: [IMAGE_ID] });
  assert.equal(fetchImpl.calls[0].url, 'https://api.openai.com/v1/images/edits');
});

test('OpenAI rejects a model override and a missing credential', async () => {
  const ws = makeWorkspace();
  const fetchImpl = createFetchRecorder(() => ({ body: { data: [{ b64_json: B64 }] } }));
  const { broker } = build(ws, { skillId: 'product-image', attachments: {}, fetchImpl });
  await assert.rejects(broker.dispatch('aurora.openai_image', { prompt: 'x', model: 'evil-model' }), /model/);
  assert.equal(fetchImpl.calls.length, 0);

  fs.rmSync(path.join(ws.secrets, 'openai-api-key'));
  await assert.rejects(broker.dispatch('aurora.openai_image', { prompt: 'x' }), /credential|secret|openai/i);
});
