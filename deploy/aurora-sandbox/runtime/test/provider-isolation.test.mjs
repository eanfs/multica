// No-fallback isolation: one provider failure must not touch any other provider.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import { makeWorkspace, baseContext, writeContextFile, writeInput, attachmentEntry, uuid, pngBytes, fakeProviderRun } from './helpers.mjs';

const IMAGE_ID = uuid(9);

function fakeSeedreamThatFails(seen) {
  return {
    async generate({ fetchImpl }) {
      seen.push('seedream');
      const response = await fetchImpl('https://ark.cn-beijing.volces.com/api/plan/v3/images/generations', { method: 'POST', headers: {}, body: '{}' });
      throw new Error('provider unavailable with status ' + response.status);
    },
    producerMetadata() { return { id: 'byted-ark-seedream-skill', version: '4.0.0', tree_sha256: null }; },
  };
}

test('a Seedream provider failure never calls OpenAI, ASR, or the importer', async () => {
  const ws = makeWorkspace();
  const imageBytes = pngBytes(20);
  writeInput(ws, 'ref.png', imageBytes);
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
  fs.writeFileSync(path.join(ws.secrets, 'ark-api-key'), 'ark-isolation-key', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'ark-api-key'), 0o400);
  const contextPath = writeContextFile(ws, baseContext(ws, {
    skill_id: 'poster',
    attachments: { [IMAGE_ID]: attachmentEntry('ref.png', 'image/png', imageBytes.length) },
  }));

  const seen = [];
  const outboundHosts = [];
  const fetchImpl = async (url) => {
    outboundHosts.push(new URL(url).host);
    seen.push('fetch');
    return new Response('{}', { status: 500 });
  };
  let imported = 0;
  const broker = createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths: {
      ark: path.join(ws.secrets, 'ark-api-key'),
      openai: path.join(ws.secrets, 'openai-api-key'),
      volcAsr: path.join(ws.secrets, 'volc-asr-api-key'),
      taskToken: path.join(ws.secrets, 'task-token'),
    },
    fetchImpl,
    providerRun: fakeProviderRun(),
    vendor: { seedreamModule: fakeSeedreamThatFails(seen) },
    importer: async () => { imported += 1; return { staging_id: uuid(50) }; },
  });

  await assert.rejects(broker.dispatch('aurora.seedream_generate', { prompt: 'x', attachment_ids: [IMAGE_ID] }), /unavailable|provider/i);
  assert.equal(imported, 0);
  assert.deepEqual(seen, ['seedream', 'fetch']);
  assert.deepEqual(outboundHosts, ['ark.cn-beijing.volces.com']);
});
