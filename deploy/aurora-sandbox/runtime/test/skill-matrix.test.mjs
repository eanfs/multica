// Canonical 13-skill route matrix.
//
// Every available skill runs its exact reviewed tool chain against local fakes:
// no test here contacts a real provider, downloads a real result URL, or starts
// an agent. The assertions pin the route (which provider is called and how many
// times), the produced v1 manifest, and the no-fallback failure contract: when
// the selected provider fails, no other provider is ever called.
//
// The per-skill chain this suite proves:
//   poster/xhs-image/text-image -> Ark Seedream only
//   product-image               -> OpenAI generation with no image, edit with image
//   image-edit                  -> OpenAI edit only
//   id-photo                    -> local tool only
//   image-video/text-video      -> Ark Seedance create once + poll only
//   video-captions              -> Volc ASR then HyperFrames
//   xhs-copy/document-summary   -> document read then text artifact
//   resume                      -> fixed Chromium render
//   transcription               -> Volc ASR only

import { test, after } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker, TOOL_NAMES } from '../src/server.mjs';
import { AVAILABLE_SKILLS, PROVIDER_ORIGINS, SKILL_ROUTES } from '../src/policy.mjs';
import { readBoundedJson } from '../src/transport.mjs';
import {
  attachmentEntry,
  baseContext,
  fakeProviderRun,
  jsonResponse,
  loadCjsModule,
  makeWorkspace,
  materializePatchedVendor,
  pngBytes,
  readManifest,
  uuid,
  writeContextFile,
  writeInput,
} from './helpers.mjs';

const ARK = PROVIDER_ORIGINS.ark;
const OPENAI = PROVIDER_ORIGINS.openai;
const ASR = PROVIDER_ORIGINS.volcAsr;
const EXTERNAL_ID = 'cgt-20260926120000-matrix1';
const B64 = Buffer.from('matrix-png-output').toString('base64');
const IMG_BYTES = pngBytes(48);
const DOC_BYTES = Buffer.from('# Matrix document\n\nA short reviewable body.');
const AUDIO_BYTES = Buffer.from('ID3-matrix-audio-bytes');
const VIDEO_BYTES = Buffer.alloc(64, 7);
const IMAGE_ID = uuid(9);
const DOC_ID = uuid(8);
const AUDIO_ID = uuid(7);
const VIDEO_ID = uuid(6);
const MAX_PROVIDER_BYTES = 1024 * 1024;

// The patched Seedream adapter is materialized from the vendored tree exactly
// as the image build does, so the matrix drives the real reviewed module.
const PATCHED_DIR = materializePatchedVendor();
const seedreamModule = loadCjsModule(path.join(PATCHED_DIR, 'byted-ark-seedream-skill/scripts/seedream-broker.js'));
after(() => fs.rmSync(PATCHED_DIR, { recursive: true, force: true }));

function groupFor(origin) {
  if (origin === ARK) return 'ark';
  if (origin === OPENAI) return 'openai';
  if (origin === ASR) return 'asr';
  return 'other';
}

// A local fetch dispatcher that never touches the network: it records every
// call by provider origin and hands it to the scripted handler for that
// provider. A provider with no handler is a test failure, which is what makes
// "zero calls to every non-selected provider" a real assertion.
function createRouter(handlers) {
  const groups = { ark: [], openai: [], asr: [], other: [] };
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const parsed = new URL(url);
    const record = {
      group: groupFor(parsed.origin),
      url: parsed.href,
      path: parsed.pathname,
      method: (init.method || 'GET').toUpperCase(),
      init,
    };
    groups[record.group].push(record);
    calls.push(record);
    const handler = handlers[record.group];
    if (typeof handler !== 'function') {
      throw new Error('unexpected provider call to ' + parsed.origin + parsed.pathname);
    }
    return handler(record);
  };
  fetchImpl.groups = groups;
  fetchImpl.calls = calls;
  return { fetchImpl, groups, calls, counts: () => ({ ark: groups.ark.length, openai: groups.openai.length, asr: groups.asr.length }) };
}

function delayedResponse(body, status, headers, delayMs) {
  return (record) => new Promise((resolve, reject) => {
    const signal = record.init && record.init.signal;
    const timer = setTimeout(() => resolve(new Response(body, { status, headers })), delayMs);
    if (!signal) return;
    const onAbort = () => {
      clearTimeout(timer);
      reject(new Error('provider request aborted'));
    };
    if (signal.aborted) {
      onAbort();
      return;
    }
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

function arkDefault(record) {
  if (record.path === '/api/plan/v3/images/generations') {
    return jsonResponse({ id: 'seedream-matrix', model: 'doubao-seedream-5.0-pro', data: [{ url: 'https://cdn.example.com/seedream-matrix.png' }] });
  }
  if (record.path === '/api/plan/v3/contents/generations/tasks' && record.method === 'POST') {
    return jsonResponse({ id: EXTERNAL_ID, status: 'queued' });
  }
  if (record.path === '/api/plan/v3/contents/generations/tasks/' + EXTERNAL_ID && record.method === 'GET') {
    return jsonResponse({ id: EXTERNAL_ID, status: 'succeeded', content: { video_url: 'https://cdn.example.com/seedance-matrix.mp4' } });
  }
  throw new Error('unexpected Ark path ' + record.method + ' ' + record.path);
}

function openaiDefault() {
  return jsonResponse({ created: 1, data: [{ b64_json: B64 }] });
}

function asrDefault() {
  return jsonResponse({ result: { text: 'matrix transcript' } }, 200, { 'x-api-status-code': '20000000' });
}

function defaultHandlers() {
  return { ark: arkDefault, openai: openaiDefault, asr: asrDefault };
}

// The Seedance vendor tree is absent under the licence gate (issue #140), so
// the matrix drives the adapter's exact create-then-poll interface with a fake
// vendor that performs the same two bounded HTTP calls the real one will.
function fakeSeedanceVendor() {
  const calls = [];
  return {
    calls,
    API_ORIGIN: ARK,
    async createTask({ fetchImpl, env, input }) {
      calls.push({ kind: 'create', input, env });
      const response = await fetchImpl(ARK + '/api/plan/v3/contents/generations/tasks', {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: 'Bearer ' + env.ARK_API_KEY },
        body: JSON.stringify({ model: input.model, content: [{ type: 'text', text: input.prompt }] }),
      });
      if (!response.ok) throw new Error('Seedance create failed with status ' + response.status);
      const parsed = await readBoundedJson(response, MAX_PROVIDER_BYTES);
      if (typeof parsed.id !== 'string' || parsed.id.length === 0) throw new Error('Seedance create returned no task id');
      return { provider: 'volcengine-agentplan', model: input.model, external_id: parsed.id, status: 'queued', outputs: [] };
    },
    async pollTask({ fetchImpl, env, taskId }) {
      calls.push({ kind: 'poll', taskId });
      const response = await fetchImpl(ARK + '/api/plan/v3/contents/generations/tasks/' + encodeURIComponent(taskId), {
        method: 'GET',
        headers: { authorization: 'Bearer ' + env.ARK_API_KEY },
      });
      if (!response.ok) throw new Error('Seedance poll failed with status ' + response.status);
      const parsed = await readBoundedJson(response, MAX_PROVIDER_BYTES);
      if (parsed.status !== 'succeeded' || !parsed.content || typeof parsed.content.video_url !== 'string') {
        throw new Error('Seedance task did not succeed');
      }
      return {
        provider: 'volcengine-agentplan',
        model: 'doubao-seedance-2.0',
        external_id: taskId,
        status: 'succeeded',
        outputs: [{ kind: 'video', url: parsed.content.video_url }],
      };
    },
    producerMetadata(treeSha256) {
      return { id: 'byted-ark-seedance-skill', version: '5.0.0', tree_sha256: treeSha256 || null };
    },
  };
}

function writeSecrets(ws) {
  const secrets = {
    'ark-api-key': 'ark-matrix-key-abcdef',
    'openai-api-key': 'sk-matrix-openai',
    'volc-asr-api-key': 'asr-matrix-key',
    'task-token': 'mat-matrix-task-token',
  };
  for (const [name, value] of Object.entries(secrets)) {
    const target = path.join(ws.secrets, name);
    fs.writeFileSync(target, value, { mode: 0o400 });
    fs.chmodSync(target, 0o400);
  }
}

function secretPaths(ws) {
  return {
    ark: path.join(ws.secrets, 'ark-api-key'),
    openai: path.join(ws.secrets, 'openai-api-key'),
    volcAsr: path.join(ws.secrets, 'volc-asr-api-key'),
    taskToken: path.join(ws.secrets, 'task-token'),
  };
}

// A deterministic process runner for every local tool the matrix exercises. It
// records the argv and writes the output file the tool expects, so the manifest
// can be validated without ImageMagick, FFmpeg, HyperFrames, or Chromium.
function localProcessRunner() {
  const calls = [];
  return {
    calls,
    async run({ command, args = [] }) {
      calls.push({ command, args });
      if (command === 'ffprobe') {
        return {
          stdout: JSON.stringify({
            format: { duration: '12.5' },
            streams: [{ codec_type: 'video', codec_name: 'h264' }, { codec_type: 'audio', codec_name: 'aac' }],
          }),
          stderr: '',
          code: 0,
        };
      }
      if (command === 'ffmpeg') {
        fs.writeFileSync(args[args.length - 1], Buffer.from('matrix-wav'));
        return { stdout: '', stderr: '', code: 0 };
      }
      if (command === 'magick') {
        fs.writeFileSync(args[args.length - 1], pngBytes(96));
        return { stdout: '', stderr: '', code: 0 };
      }
      if (command === 'hyperframes') {
        fs.writeFileSync(args[args.indexOf('-o') + 1], Buffer.from('matrix-mp4'));
        return { stdout: '', stderr: '', code: 0 };
      }
      if (command === 'chromium') {
        const marker = args.find((arg) => arg.startsWith('--print-to-pdf='));
        fs.writeFileSync(marker.slice('--print-to-pdf='.length), Buffer.from('%PDF-1.4 matrix'));
        return { stdout: '', stderr: '', code: 0 };
      }
      if (command === 'pdftotext') return { stdout: 'matrix pdf text', stderr: '', code: 0 };
      throw new Error('matrix process runner: unexpected command ' + command);
    },
  };
}

const INPUT_WRITERS = {
  image: (ws) => {
    writeInput(ws, 'ref.png', IMG_BYTES);
    return { [IMAGE_ID]: attachmentEntry('ref.png', 'image/png', IMG_BYTES.length) };
  },
  document: (ws) => {
    writeInput(ws, 'notes.md', DOC_BYTES);
    return { [DOC_ID]: attachmentEntry('notes.md', 'text/markdown', DOC_BYTES.length) };
  },
  audio: (ws) => {
    writeInput(ws, 'clip.mp3', AUDIO_BYTES);
    return { [AUDIO_ID]: attachmentEntry('clip.mp3', 'audio/mpeg', AUDIO_BYTES.length) };
  },
  video: (ws) => {
    writeInput(ws, 'clip.mp4', VIDEO_BYTES);
    return { [VIDEO_ID]: attachmentEntry('clip.mp4', 'video/mp4', VIDEO_BYTES.length) };
  },
};

// skillScenario describes one skill's exact tool chain: the authorized inputs
// it needs and the ordered dispatch calls the workflow brief prescribes.
function skillScenario(ws, skillId, options = {}) {
  const image = () => INPUT_WRITERS.image(ws);
  const document = () => INPUT_WRITERS.document(ws);
  const audio = () => INPUT_WRITERS.audio(ws);
  const video = () => INPUT_WRITERS.video(ws);
  switch (skillId) {
    case 'poster':
    case 'xhs-image':
      return { attachments: image(), run: (broker) => broker.dispatch('aurora.seedream_generate', { prompt: 'matrix reference', attachment_ids: [IMAGE_ID] }) };
    case 'text-image':
      return { attachments: {}, run: (broker) => broker.dispatch('aurora.seedream_generate', { prompt: 'matrix text to image' }) };
    case 'product-image':
      if (options.mode === 'edit') {
        return { attachments: image(), run: (broker) => broker.dispatch('aurora.openai_image', { prompt: 'matrix product edit', attachment_ids: [IMAGE_ID] }) };
      }
      return { attachments: {}, run: (broker) => broker.dispatch('aurora.openai_image', { prompt: 'matrix product generation' }) };
    case 'image-edit':
      return { attachments: image(), run: (broker) => broker.dispatch('aurora.openai_image', { prompt: 'matrix edit', attachment_ids: [IMAGE_ID] }) };
    case 'id-photo':
      return { attachments: image(), run: (broker) => broker.dispatch('aurora.id_photo', { attachment_id: IMAGE_ID, output_name: 'id.png' }) };
    case 'image-video':
      return { attachments: image(), seedance: true, run: (broker) => broker.dispatch('aurora.seedance_generate', { prompt: 'matrix image to video', attachment_ids: [IMAGE_ID] }) };
    case 'text-video':
      return { attachments: {}, seedance: true, run: (broker) => broker.dispatch('aurora.seedance_generate', { prompt: 'matrix text to video' }) };
    case 'video-captions':
      return {
        attachments: video(),
        run: async (broker) => {
          await broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: VIDEO_ID });
          return broker.dispatch('aurora.render_video_captions', {
            attachment_id: VIDEO_ID,
            cues: [{ start: 0, end: 1.5, text: 'matrix caption' }],
            output_name: 'captioned.mp4',
          });
        },
      };
    case 'xhs-copy': {
      const attachments = options.emptyDocument ? {} : document();
      return {
        attachments,
        run: async (broker) => {
          if (Object.keys(attachments).length > 0) {
            const doc = await broker.dispatch('aurora.read_document', { attachment_id: DOC_ID });
            return broker.dispatch('aurora.write_text_artifact', { content: 'matrix copy: ' + doc.text, name: 'copy.md' });
          }
          return broker.dispatch('aurora.write_text_artifact', { content: 'matrix copy', name: 'copy.md' });
        },
      };
    }
    case 'document-summary':
      return {
        attachments: document(),
        run: async (broker) => {
          const doc = await broker.dispatch('aurora.read_document', { attachment_id: DOC_ID });
          return broker.dispatch('aurora.write_text_artifact', { content: doc.text, name: 'summary.md' });
        },
      };
    case 'resume':
      return {
        attachments: {},
        run: (broker) => broker.dispatch('aurora.render_resume', {
          sections: { name: 'Ada Matrix', title: 'Engineer', contact: ['ada@example.com'] },
          output_name: 'resume',
        }),
      };
    case 'transcription':
      return { attachments: audio(), run: (broker) => broker.dispatch('aurora.volc_asr_transcribe', { attachment_id: AUDIO_ID }) };
    default:
      throw new Error('matrix does not know skill ' + skillId);
  }
}

function prepareScenario(ws, skillId, options = {}) {
  writeSecrets(ws);
  const scenario = skillScenario(ws, skillId, options);
  const router = options.router || createRouter(defaultHandlers());
  const processRunner = options.processRunner || localProcessRunner();
  const providerRun = options.providerRun || fakeProviderRun();
  const context = baseContext(ws, { skill_id: skillId, attachments: scenario.attachments });
  const contextPath = writeContextFile(ws, context);
  const vendor = scenario.seedance
    ? { seedanceModule: options.seedanceModule || fakeSeedanceVendor() }
    : { seedreamModule };
  const broker = createBroker({
    contextPath,
    inputRoot: ws.inputRoot,
    outputRoot: ws.outputRoot,
    serverOrigin: 'https://multica.test',
    secretPaths: secretPaths(ws),
    fetchImpl: router.fetchImpl,
    providerRun,
    processRunner,
    vendor,
    importer: async (request) => ({ staging_id: uuid(41), name: request.name, size_bytes: 4, sha256: 'sha256:' + 'd'.repeat(64) }),
    providerTimeoutMs: options.providerTimeoutMs,
  });
  return { broker, router, processRunner, providerRun, run: scenario.run };
}

const ROUTE = {
  poster: 'seedream',
  'xhs-image': 'seedream',
  'text-image': 'seedream',
  'product-image': 'openai',
  'image-edit': 'openai',
  'id-photo': 'local',
  'image-video': 'seedance',
  'text-video': 'seedance',
  'video-captions': 'asr',
  'xhs-copy': 'local',
  'document-summary': 'local',
  resume: 'local',
  transcription: 'asr',
};

// Per-route outbound HTTP counts. Seedance is one create plus one poll on Ark;
// every other route makes exactly one call to exactly one provider.
const ROUTE_COUNTS = {
  seedream: { ark: 1, openai: 0, asr: 0 },
  openai: { ark: 0, openai: 1, asr: 0 },
  seedance: { ark: 2, openai: 0, asr: 0 },
  asr: { ark: 0, openai: 0, asr: 1 },
  local: { ark: 0, openai: 0, asr: 0 },
};

const MANIFEST = {
  poster: { producer: 'byted-ark-seedream-skill', kinds: ['image'] },
  'xhs-image': { producer: 'byted-ark-seedream-skill', kinds: ['image'] },
  'text-image': { producer: 'byted-ark-seedream-skill', kinds: ['image'] },
  'product-image': { producer: 'openai-images', kinds: ['image'] },
  'image-edit': { producer: 'openai-images', kinds: ['image'] },
  'id-photo': { producer: 'multica-aurora-runtime', kinds: ['image'] },
  'image-video': { producer: 'byted-ark-seedance-skill', kinds: ['video'] },
  'text-video': { producer: 'byted-ark-seedance-skill', kinds: ['video'] },
  'video-captions': { producer: 'volcengine-asr', kinds: ['video'] },
  'xhs-copy': { producer: 'multica-aurora-runtime', kinds: ['text'] },
  'document-summary': { producer: 'multica-aurora-runtime', kinds: ['text'] },
  resume: { producer: 'multica-aurora-runtime', kinds: ['pdf', 'text'] },
  transcription: { producer: 'volcengine-asr', kinds: ['text'] },
};

function assertManifest(ws, skillId) {
  const manifest = readManifest(ws.outputRoot);
  const expected = MANIFEST[skillId];
  assert.equal(manifest.schema, 'com.multica.aurora.artifacts');
  assert.equal(manifest.version, 1);
  assert.equal(manifest.skill_id, skillId);
  assert.equal(manifest.task_id, uuid(1));
  assert.equal(manifest.producer.id, expected.producer);
  assert.ok(Array.isArray(manifest.artifacts) && manifest.artifacts.length > 0);
  const primary = manifest.artifacts.filter((artifact) => artifact.role === 'primary');
  assert.ok(primary.length >= 1, 'manifest requires a primary artifact');
  for (const artifact of primary) {
    assert.ok(expected.kinds.includes(artifact.kind), 'primary kind ' + artifact.kind + ' is not ' + expected.kinds);
  }
  for (const artifact of manifest.artifacts) {
    assert.ok(artifact.size_bytes > 0, 'artifact size must be positive');
    assert.match(artifact.sha256, /^sha256:[0-9a-f]{64}$/);
    assert.ok(['file', 'staged_object'].includes(artifact.source.type));
  }
  // No provider URL may survive into the manifest or the tool result.
  assert.doesNotMatch(JSON.stringify(manifest), /cdn\.example\.com/);
}

function assertNoProviderUrls(value) {
  assert.doesNotMatch(JSON.stringify(value), /cdn\.example\.com/);
  assert.doesNotMatch(JSON.stringify(value), new RegExp(B64.slice(0, 16)));
}

const MATRIX_CASES = [
  ...AVAILABLE_SKILLS.map((skill) => ({ skill, label: skill })),
  { skill: 'product-image', label: 'product-image with an image', mode: 'edit' },
  { skill: 'xhs-copy', label: 'xhs-copy without a document', emptyDocument: true },
];

test('the fixed route table covers exactly the thirteen available skills with registered broker tools', () => {
  assert.equal(AVAILABLE_SKILLS.length, 13);
  assert.equal(Object.keys(SKILL_ROUTES).length, 13);
  for (const skill of AVAILABLE_SKILLS) {
    const policy = SKILL_ROUTES[skill];
    assert.ok(policy, skill);
    assert.ok(policy.tools.length > 0, skill);
    for (const tool of policy.tools) {
      assert.ok(TOOL_NAMES.includes(tool), skill + ' names unregistered tool ' + tool);
    }
  }
});

test('the MCP broker exposes exactly nine named tools, none general purpose', () => {
  assert.equal(TOOL_NAMES.length, 9);
  for (const tool of TOOL_NAMES) {
    assert.match(tool, /^aurora\.[a-z_]+$/);
  }
});

for (const matrixCase of MATRIX_CASES) {
  test('route ' + matrixCase.label + ' runs its exact reviewed chain against local fakes', async () => {
    const ws = makeWorkspace();
    const scenario = prepareScenario(ws, matrixCase.skill, matrixCase);
    const result = await scenario.run(scenario.broker);

    assert.ok(result, 'tool chain returned no result');
    assert.deepEqual(scenario.router.counts(), ROUTE_COUNTS[ROUTE[matrixCase.skill]], matrixCase.label + ' provider counts');
    assert.equal(scenario.router.groups.other.length, 0, matrixCase.label + ' contacted a non-provider origin');
    assertManifest(ws, matrixCase.skill);
    assertNoProviderUrls(result);
  });
}

// Seedance begins its provider run before create, records the cgt id, and then
// polls. The create must happen exactly once across the whole chain.
test('image-video submits Seedance exactly once and records the external id before polling', async () => {
  const ws = makeWorkspace();
  const scenario = prepareScenario(ws, 'image-video');
  await scenario.run(scenario.broker);
  const recordExternal = scenario.providerRun.calls.filter((entry) => entry.kind === 'recordExternal');
  const begin = scenario.providerRun.calls.filter((entry) => entry.kind === 'begin');
  assert.equal(begin.length, 1);
  assert.equal(begin[0].operation, 'seedance.create');
  assert.equal(recordExternal.length, 1);
  assert.equal(recordExternal[0].externalId, EXTERNAL_ID);
  const recordIndex = scenario.providerRun.calls.findIndex((entry) => entry.kind === 'recordExternal');
  const finishIndex = scenario.providerRun.calls.findIndex((entry) => entry.kind === 'finish');
  assert.ok(recordIndex < finishIndex, 'external id must be recorded before the run finishes');
});

test('a Seedance retry after a recorded create never submits a second create', async () => {
  const ws = makeWorkspace();
  const scenario = prepareScenario(ws, 'image-video');
  const vendor = scenario.broker.vendor.seedance;
  // First dispatch creates exactly once.
  await scenario.run(scenario.broker);
  assert.equal(vendor.calls.filter((entry) => entry.kind === 'create').length, 1);
  // The retry finds the recorded submitted run and polls it again.
  await assert.rejects(scenario.run(scenario.broker), /already succeeded|refusing a second create|ambiguous/i);
  assert.equal(vendor.calls.filter((entry) => entry.kind === 'create').length, 1, 'the retry must never create twice');
});

test('an ambiguous Seedance create lease fails closed without submitting', async () => {
  const ws = makeWorkspace();
  const providerRun = fakeProviderRun();
  providerRun.begin = async (input) => ({ ...input, external_id: null, state: 'creating', create_allowed: false });
  const scenario = prepareScenario(ws, 'text-video', { providerRun });
  await assert.rejects(scenario.run(scenario.broker), /lease|ambig|refus/i);
  const vendor = scenario.broker.vendor.seedance;
  assert.equal(vendor.calls.filter((entry) => entry.kind === 'create').length, 0);
  assert.deepEqual(scenario.router.counts(), { ark: 0, openai: 0, asr: 0 });
  assert.equal(scenario.router.groups.other.length, 0);
});

test('video-captions chains the ASR transcript into the caption renderer without an id collision', async () => {
  const ws = makeWorkspace();
  const scenario = prepareScenario(ws, 'video-captions');
  await scenario.run(scenario.broker);

  assert.deepEqual(scenario.processRunner.calls.map((entry) => entry.command), ['ffprobe', 'ffmpeg', 'hyperframes']);
  assert.deepEqual(scenario.router.counts(), { ark: 0, openai: 0, asr: 1 });

  const manifest = readManifest(ws.outputRoot);
  const transcript = manifest.artifacts.find((artifact) => artifact.role === 'transcript');
  const primary = manifest.artifacts.find((artifact) => artifact.role === 'primary');
  assert.ok(transcript, 'the ASR transcript must be a transcript-role artifact');
  assert.equal(transcript.id, 'transcript-1');
  assert.equal(transcript.kind, 'text');
  assert.ok(primary, 'the rendered captions must be the primary artifact');
  assert.equal(primary.kind, 'video');
});

const FAILURE_ROUTES = [
  { route: 'seedream', group: 'ark', skill: 'poster' },
  { route: 'openai', group: 'openai', skill: 'product-image' },
  { route: 'seedance', group: 'ark', skill: 'image-video' },
  { route: 'asr', group: 'asr', skill: 'transcription' },
];

const FAILURE_MODES = ['401', '429', '500', 'malformed', 'oversized', 'timeout'];

function failureHandler(group, mode) {
  if (mode === 'timeout') {
    if (group === 'asr') {
      return delayedResponse(JSON.stringify({ result: { text: 'late' } }), 200, { 'content-type': 'application/json', 'x-api-status-code': '20000000' }, 250);
    }
    return delayedResponse(JSON.stringify({ data: [{ url: 'https://cdn.example.com/late.png' }] }), 200, { 'content-type': 'application/json' }, 250);
  }
  if (mode === 'malformed') {
    if (group === 'asr') {
      return () => new Response('not-valid-json', { status: 200, headers: { 'content-type': 'application/json', 'x-api-status-code': '20000000' } });
    }
    return () => new Response('not-valid-json', { status: 200, headers: { 'content-type': 'application/json' } });
  }
  if (mode === 'oversized') {
    if (group === 'asr') {
      return () => jsonResponse({ result: { text: 'x'.repeat(MAX_PROVIDER_BYTES + 128) } }, 200, { 'x-api-status-code': '20000000' });
    }
    if (group === 'openai') {
      // Base64 decodes to ~3/4 of its length, so this is just over the 25 MiB
      // per-artifact image cap the manifest contract defines.
      const oversizedBase64 = 'A'.repeat(36 * 1024 * 1024);
      return () => jsonResponse({ created: 1, data: [{ b64_json: oversizedBase64 }] });
    }
    return () => jsonResponse({ data: 'x'.repeat(MAX_PROVIDER_BYTES + 128) });
  }
  const status = Number(mode);
  if (group === 'asr') {
    return () => jsonResponse({ message: 'provider failure' }, status);
  }
  return () => jsonResponse({ error: 'provider failure', status }, status);
}

function isSelectedGroup(route, group) {
  return route === 'openai' ? group === 'openai' : group === 'ark' || group === 'asr';
}

for (const failure of FAILURE_ROUTES) {
  for (const mode of FAILURE_MODES) {
    test('no fallback: ' + failure.route + ' ' + mode + ' fails the task and calls no other provider', async () => {
      const ws = makeWorkspace();
      const router = createRouter({ [failure.group]: failureHandler(failure.group, mode) });
      const scenario = prepareScenario(ws, failure.skill, { router, providerTimeoutMs: 25 });
      await assert.rejects(scenario.run(scenario.broker));

      // No manifest is published for a failed route.
      assert.equal(fs.existsSync(path.join(ws.outputRoot, '.multica/aurora-artifacts.v1.json')), false);
      // The selected provider is the only one that may have been contacted.
      assert.equal(scenario.router.groups.other.length, 0, 'a non-provider origin was contacted');
      const counts = scenario.router.counts();
      for (const group of ['ark', 'openai', 'asr']) {
        if (group !== failure.group) {
          assert.equal(counts[group], 0, 'non-selected provider ' + group + ' was contacted for route ' + failure.route);
        }
      }
      assert.ok(counts[failure.group] >= 1, 'the selected provider was never contacted');
      assert.equal(isSelectedGroup(failure.route, failure.group), true);
    });
  }
}

// The OpenAI route returns base64 output, so its response is not bounded by the
// small descriptor cap. The adapter bounds each decoded image at the manifest
// contract's 25 MiB per-artifact image limit; just over that must fail closed.
test('an OpenAI image above the 25 MiB per-artifact cap fails closed', async () => {
  const ws = makeWorkspace();
  const router = createRouter({ openai: failureHandler('openai', 'oversized') });
  const scenario = prepareScenario(ws, 'product-image', { router });
  await assert.rejects(scenario.run(scenario.broker), /25 MiB|artifact cap|exceeds/i);
  assert.deepEqual(scenario.router.counts(), { ark: 0, openai: 1, asr: 0 });
  assert.equal(scenario.router.groups.other.length, 0);
});
