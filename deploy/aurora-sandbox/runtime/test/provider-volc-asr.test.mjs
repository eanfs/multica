// Volcengine ASR adapter fake-provider contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import { createProcessRunner } from '../src/transport.mjs';
import { makeWorkspace, baseContext, writeContextFile, writeInput, attachmentEntry, uuid, fakeProviderRun, readManifest } from './helpers.mjs';

const ASR_KEY = 'asr-test-key';
const AUDIO_ID = uuid(9);
const VIDEO_ID = uuid(8);
const AUDIO = Buffer.from('ID3-fake-audio-bytes');
const AUDIO_B64 = AUDIO.toString('base64');

function secretPaths(ws) {
  return {
    ark: path.join(ws.secrets, 'ark-api-key'),
    openai: path.join(ws.secrets, 'openai-api-key'),
    volcAsr: path.join(ws.secrets, 'volc-asr-api-key'),
    taskToken: path.join(ws.secrets, 'task-token'),
  };
}

function prepareSecrets(ws) {
  fs.writeFileSync(path.join(ws.secrets, 'volc-asr-api-key'), ASR_KEY, { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'volc-asr-api-key'), 0o400);
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
}

function build(ws, { skillId, attachments, fetchImpl, processRunner }) {
  prepareSecrets(ws);
  const contextPath = writeContextFile(ws, baseContext(ws, { skill_id: skillId, attachments }));
  const providerRun = fakeProviderRun();
  const broker = createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths: secretPaths(ws),
    fetchImpl,
    providerRun,
    processRunner,
    vendor: {},
    importer: async () => { throw new Error('ASR must not import'); },
    asrPollIntervalMs: 1,
  });
  return { broker, providerRun };
}

function asrHandler({ statusCode = '20000000', body = { result: { text: 'hello world' } }, extraHeaders = {} } = {}) {
  return {
    origin: 'http://127.0.0.1',
    fetchImpl: async (url, init) => new Response(typeof body === 'string' ? body : JSON.stringify(body), {
      status: 200,
      headers: { 'content-type': 'application/json', 'x-api-status-code': statusCode, 'x-api-message': 'ok', ...extraHeaders },
    }),
  };
}

test('transcription posts the fixed flash request with the resource id and streaming base64', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'clip.mp3', AUDIO);
  const calls = [];
  const fake = asrHandler();
  const fetchImpl = async (url, init) => { calls.push({ url: String(url), init, body: JSON.parse(init.body) }); return fake.fetchImpl(url, init); };
  const { broker, providerRun } = build(ws, {
    skillId: 'transcription',
    attachments: { [AUDIO_ID]: attachmentEntry('clip.mp3', 'audio/mpeg', AUDIO.length) },
    fetchImpl,
  });
  const result = await broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: AUDIO_ID });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'https://openspeech.bytedance.com/api/v3/auc/bigmodel/recognize/flash');
  assert.equal(calls[0].init.method, 'POST');
  const headers = calls[0].init.headers;
  assert.equal(headers['x-api-key'] || headers['X-Api-Key'], ASR_KEY);
  assert.equal(headers['x-api-resource-id'] || headers['X-Api-Resource-Id'], 'volc.bigasr.auc_turbo');
  assert.equal(headers['x-api-sequence'] || headers['X-Api-Sequence'], '-1');
  const requestId = headers['x-api-request-id'] || headers['X-Api-Request-Id'];
  assert.match(requestId, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i);
  assert.equal(calls[0].body.audio.data, AUDIO_B64);
  assert.equal(calls[0].body.request.model_name, 'bigmodel');
  assert.equal(calls[0].body.request.enable_punc, true);
  assert.equal(calls[0].body.request.enable_itn, true);
  assert.equal(calls[0].body.callback, undefined);
  assert.equal(providerRun.calls[0].operation, 'asr.recognize');
  assert.equal(providerRun.calls[0].model, 'bigmodel');

  assert.match(result.text, /hello world/);
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].kind, 'text');
  assert.equal(manifest.artifacts[0].role, 'primary');
});

test('ASR rejects a missing or non-success X-Api-Status-Code', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'clip.mp3', AUDIO);
  const fake = asrHandler({ statusCode: '40000000', body: { message: 'bad' } });
  const { broker } = build(ws, {
    skillId: 'transcription',
    attachments: { [AUDIO_ID]: attachmentEntry('clip.mp3', 'audio/mpeg', AUDIO.length) },
    fetchImpl: fake.fetchImpl,
  });
  await assert.rejects(broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: AUDIO_ID }), /status|20000000/i);
});

test('ASR bounds the response body', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'clip.mp3', AUDIO);
  const fake = asrHandler({ body: { result: { text: 'x'.repeat(2 * 1024 * 1024) } } });
  const { broker } = build(ws, {
    skillId: 'transcription',
    attachments: { [AUDIO_ID]: attachmentEntry('clip.mp3', 'audio/mpeg', AUDIO.length) },
    fetchImpl: fake.fetchImpl,
  });
  await assert.rejects(broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: AUDIO_ID }), /size|large|cap/i);
});

test('video captions probe and extract bounded audio before ASR', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'clip.mp4', Buffer.alloc(64, 1));
  const order = [];
  const processCalls = [];
  const extracted = Buffer.from('extracted-wav-bytes');
  const processRunner = {
    async run(input) {
      processCalls.push(input);
      if (input.command === 'ffprobe') {
        order.push('ffprobe');
        return { stdout: JSON.stringify({ format: { duration: '12.5' }, streams: [{ codec_type: 'video', codec_name: 'h264' }, { codec_type: 'audio', codec_name: 'aac' }] }), stderr: '', code: 0 };
      }
      if (input.command === 'ffmpeg') {
        order.push('ffmpeg');
        const out = input.args[input.args.length - 1];
        fs.writeFileSync(out, extracted);
        return { stdout: '', stderr: '', code: 0 };
      }
      throw new Error('unexpected command ' + input.command);
    },
  };
  const calls = [];
  const fake = asrHandler({ body: { result: { text: 'captioned' } } });
  const fetchImpl = async (url, init) => { order.push('asr'); calls.push(JSON.parse(init.body)); return fake.fetchImpl(url, init); };
  const { broker } = build(ws, {
    skillId: 'video-captions',
    attachments: { [VIDEO_ID]: attachmentEntry('clip.mp4', 'video/mp4', 64) },
    fetchImpl,
    processRunner,
  });
  await broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: VIDEO_ID });
  assert.deepEqual(order, ['ffprobe', 'ffmpeg', 'asr']);
  assert.equal(calls[0].audio.data, extracted.toString('base64'));
  const ffmpeg = processCalls.find((entry) => entry.command === 'ffmpeg');
  assert.ok(ffmpeg.args.includes('-vn'));
  assert.equal(ffmpeg.shell, undefined);
});

test('the process runner terminates timed-out process trees', async () => {
  const runner = createProcessRunner();
  const started = Date.now();
  await assert.rejects(
    runner.run({ command: 'bash', args: ['-c', 'sleep 30 & sleep 30'], timeoutMs: 120 }),
    /tim(e|ed) out/i,
  );
  assert.ok(Date.now() - started < 5000);
});
