// Offline security regressions for the hardened Volcengine AgentPlan adapters.
//
// The test applies the vendor patch series to a throwaway copy of the vendored
// trees, loads the resulting broker adapters, and drives them with fake HTTP
// implementations. It never touches the network, credentials, or paid APIs.

import { after, test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(HERE, '../../../..');
const VENDOR_DIR = path.join(REPO_ROOT, 'deploy/aurora-sandbox/vendor/volcengine');
const LOCK_PATH = path.join(VENDOR_DIR, 'vendor-lock.json');

if (!fs.existsSync(LOCK_PATH)) {
  throw new Error('missing vendor lock; run scripts/update-aurora-volc-skills.sh first');
}
const lock = JSON.parse(fs.readFileSync(LOCK_PATH, 'utf8'));

function materializePatchedVendor() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'aurora-volc-security-'));
  for (const [skillId, entry] of Object.entries(lock.skills)) {
    const source = path.join(VENDOR_DIR, skillId);
    if (entry.vendored && fs.existsSync(source)) {
      fs.cpSync(source, path.join(dir, skillId), { recursive: true });
    }
  }
  execFileSync('git', ['init', '-q'], { cwd: dir, stdio: 'pipe' });
  const patches = [];
  for (const entry of Object.values(lock.skills)) {
    for (const patch of entry.patches) patches.push(patch.path);
  }
  for (const patch of patches) {
    execFileSync('git', ['apply', path.join(VENDOR_DIR, patch)], { cwd: dir, stdio: 'pipe' });
  }
  for (const skillId of ['byted-ark-seedance-skill', 'byted-ark-seedream-skill']) {
    const target = path.join(dir, skillId, 'scripts', skillId.includes('seedance') ? 'seedance-broker.js' : 'seedream-broker.js');
    if (!fs.existsSync(target)) throw new Error('broker adapter was not materialized: ' + target);
  }
  return dir;
}

const PATCHED_DIR = materializePatchedVendor();
const require = createRequire(import.meta.url);
const seedance = require(path.join(PATCHED_DIR, 'byted-ark-seedance-skill/scripts/seedance-broker.js'));
const seedream = require(path.join(PATCHED_DIR, 'byted-ark-seedream-skill/scripts/seedream-broker.js'));
const seedanceSource = fs.readFileSync(path.join(PATCHED_DIR, 'byted-ark-seedance-skill/scripts/seedance-broker.js'), 'utf8');
const seedreamSource = fs.readFileSync(path.join(PATCHED_DIR, 'byted-ark-seedream-skill/scripts/seedream-broker.js'), 'utf8');

after(() => {
  fs.rmSync(PATCHED_DIR, { recursive: true, force: true });
});

const ARK_KEY = 'ark-fixture-not-a-real-key';
const TASK_ID = 'cgt-20260926120000-abc12';
const DATA_URI_PNG = 'data:image/png;base64,iVBORw0KGgo=';

function response(body, options) {
  const opts = options || {};
  const status = opts.status === undefined ? 200 : opts.status;
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  const headers = opts.headers || {};
  return {
    ok: status >= 200 && status < 300,
    status: status,
    headers: { get: (name) => (Object.prototype.hasOwnProperty.call(headers, name.toLowerCase()) ? headers[name.toLowerCase()] : null) },
    text: async () => text,
  };
}

function fetchOnce(body, options) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url: url, init: init });
    return response(body, options);
  };
  fetchImpl.calls = calls;
  return fetchImpl;
}

test('official origin lock accepts only the exact Ark HTTPS origin', () => {
  assert.equal(seedance.assertApiOrigin('https://ark.cn-beijing.volces.com'), seedance.API_ORIGIN);
  assert.equal(seedream.assertApiOrigin('https://ark.cn-beijing.volces.com'), seedream.API_ORIGIN);
  const bad = [
    'http://ark.cn-beijing.volces.com',
    'https://example.com',
    'https://ark.cn-beijing.volces.com.example.com',
    'https://user:pass@ark.cn-beijing.volces.com',
    'https://ark.cn-beijing.volces.com:8443',
    'https://ark.cn-beijing.volces.com/path',
    'https://ark.cn-beijing.volces.com/?query=1',
    'https://ark.cn-beijing.volces.com/#fragment',
  ];
  for (const url of bad) {
    assert.throws(() => seedance.assertApiOrigin(url), /Ark origin/);
    assert.throws(() => seedream.assertApiOrigin(url), /Ark origin/);
  }
});

test('HTTP and origin overrides are rejected; requests stay on the locked origin', async () => {
  assert.throws(() => seedance.assertApiOrigin('http://ark.cn-beijing.volces.com'));
  assert.throws(
    () => seedance.buildCreateBody({ model: 'doubao-seedance-2.0', prompt: 'x', baseUrl: 'http://evil.example' }),
    /disabled Seedance input/,
  );
  assert.throws(
    () => seedream.buildGenerateBody({ model: 'doubao-seedream-5.0-pro', prompt: 'x', baseUrl: 'http://evil.example' }),
    /disabled Seedream input/,
  );
  const fetchImpl = fetchOnce({ id: TASK_ID, status: 'queued' });
  await seedance.createTask({
    env: { ARK_API_KEY: ARK_KEY, ARK_BASE_URL: 'http://evil.example', ARK_SEEDREAM_API_BASE_URL: 'http://evil.example' },
    fetchImpl: fetchImpl,
    input: { model: 'doubao-seedance-2.0', prompt: 'x' },
  });
  assert.equal(new URL(fetchImpl.calls[0].url).origin, seedance.API_ORIGIN);
});

test('generic credential names are rejected; only broker ARK_API_KEY is accepted', () => {
  for (const env of [{ API_KEY: ARK_KEY }, { apiKey: ARK_KEY }, { ANTHROPIC_AUTH_TOKEN: ARK_KEY }, { OPENAI_API_KEY: ARK_KEY }, {}]) {
    assert.throws(() => seedance.readBrokerApiKey(env), /ARK_API_KEY/);
    assert.throws(() => seedream.readBrokerApiKey(env), /ARK_API_KEY/);
  }
  assert.equal(seedance.readBrokerApiKey({ ARK_API_KEY: ARK_KEY }), ARK_KEY);
  assert.equal(seedream.readBrokerApiKey({ ARK_API_KEY: ARK_KEY }), ARK_KEY);
});

test('workstation configuration is never read', async () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'aurora-volc-home-'));
  try {
    fs.mkdirSync(path.join(home, '.openclaw'), { recursive: true });
    fs.writeFileSync(path.join(home, '.openclaw', 'openclaw.json'), JSON.stringify({ models: { providers: { p: { apiKey: ARK_KEY } } } }));
    fs.mkdirSync(path.join(home, '.hermes'), { recursive: true });
    fs.writeFileSync(path.join(home, '.hermes', 'config.yaml'), 'model:\n  api_key: ' + ARK_KEY + '\n');
    fs.mkdirSync(path.join(home, '.claude'), { recursive: true });
    fs.writeFileSync(path.join(home, '.claude', 'settings.json'), JSON.stringify({ env: { ANTHROPIC_AUTH_TOKEN: ARK_KEY } }));

    await assert.rejects(
      seedance.createTask({ env: { HOME: home }, fetchImpl: fetchOnce({ id: TASK_ID }), input: { model: 'doubao-seedance-2.0', prompt: 'x' } }),
      /ARK_API_KEY/,
    );
    await assert.rejects(
      seedream.generate({ env: { HOME: home }, fetchImpl: fetchOnce({}), input: { model: 'doubao-seedream-5.0-pro', prompt: 'x' } }),
      /ARK_API_KEY/,
    );

    for (const source of [seedanceSource, seedreamSource]) {
      assert.doesNotMatch(source, /openclaw|hermes|homedir/);
      assert.doesNotMatch(source, /process\.env/);
      assert.doesNotMatch(source, /child_process|execSync|spawnSync/);
    }
  } finally {
    fs.rmSync(home, { recursive: true, force: true });
  }
});

test('credential saving and configuration mutation are absent', async () => {
  const original = fs.writeFileSync;
  let writes = 0;
  try {
    fs.writeFileSync = () => {
      writes += 1;
      throw new Error('unexpected configuration write');
    };
    await seedance.createTask({
      env: { ARK_API_KEY: ARK_KEY },
      fetchImpl: fetchOnce({ id: TASK_ID, status: 'queued' }),
      input: { model: 'doubao-seedance-2.0', prompt: 'x' },
    });
    assert.equal(writes, 0);
    assert.equal(typeof seedance.saveApiKey, 'undefined');
    assert.equal(typeof seedance.autoSaveApiKey, 'undefined');
    assert.equal(typeof seedream.autoSaveApiKey, 'undefined');
    assert.equal(typeof seedream.saveApiKey, 'undefined');
  } finally {
    fs.writeFileSync = original;
  }
});

test('Seedance model IDs are restricted to the plan allowlist and 1.5 is absent', () => {
  for (const model of ['doubao-seedance-2.0', 'doubao-seedance-2.0-fast', 'doubao-seedance-2.0-mini', 'doubao-seedance-2.5']) {
    assert.equal(seedance.assertModel(model), model);
    assert.equal(seedance.buildCreateBody({ model: model, prompt: 'x' }).model, model);
  }
  assert.equal(seedance.assertModel('doubao-seedance-2-0-260128'), 'doubao-seedance-2.0');
  for (const model of ['doubao-seedance-1.5-pro', 'doubao-seedance-1.0', 'doubao-seedance-2.0-4k', '']) {
    assert.throws(() => seedance.assertModel(model), /allowlist/);
  }
  assert.equal(seedance.ALLOWED_MODELS.some((model) => /1\.5/.test(model)), false);
  assert.throws(() => seedance.buildCreateBody({ model: 'doubao-seedance-1.5-pro', prompt: 'x' }), /allowlist/);
});

test('Seedance callback, payload/task file, list, delete and preference overrides are rejected', () => {
  assert.equal(typeof seedance.listTasks, 'undefined');
  assert.equal(typeof seedance.deleteTask, 'undefined');
  const base = { model: 'doubao-seedance-2.0', prompt: 'x' };
  const disabledKeys = [
    'callback',
    'callbackUrl',
    'callback_url',
    'payloadFile',
    'payload_file',
    'taskFile',
    'task_file',
    'preferences',
    'preference',
    'savedPreference',
    'baseUrl',
    'base_url',
    'apiKey',
    'api_key',
    'saveApiKey',
    'save_api_key',
    'downloadDir',
    'download_dir',
    'userOverrides',
  ];
  for (const key of disabledKeys) {
    const input = Object.assign({}, base);
    input[key] = 'x';
    assert.throws(() => seedance.buildCreateBody(input), /disabled Seedance input/);
  }
});

test('task-scoped paths cannot escape and output names are UUIDs at mode 0600', () => {
  const taskDir = fs.mkdtempSync(path.join(os.tmpdir(), 'aurora-volc-task-'));
  try {
    const dir = seedance.taskStateDir(taskDir, TASK_ID);
    assert.ok(dir.startsWith(path.resolve(taskDir) + path.sep));
    for (const bad of ['../outside', 'cgt-../../outside', 'cgt-abc/escape', 'not-a-task', '']) {
      assert.throws(() => seedance.taskStateDir(taskDir, bad));
    }
    const name = seedance.uuidFilename('.json');
    assert.match(name, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$/i);
    const file = path.join(dir, name);
    seedance.writePrivateJson(file, { hello: 'world' });
    assert.equal(fs.statSync(file).mode & 0o777, 0o600);
    assert.deepEqual(JSON.parse(fs.readFileSync(file, 'utf8')), { hello: 'world' });
  } finally {
    fs.rmSync(taskDir, { recursive: true, force: true });
  }
});

test('response buffers and input Data URIs are capped', async () => {
  await assert.rejects(
    seedance.boundedText(response('x', { headers: { 'content-length': String(seedance.MAX_RESPONSE_BYTES + 1) } }), seedance.MAX_RESPONSE_BYTES),
    /buffer cap/,
  );
  await assert.rejects(
    seedance.boundedText({ headers: { get: () => null }, text: async () => 'x'.repeat(seedance.MAX_RESPONSE_BYTES + 1) }, seedance.MAX_RESPONSE_BYTES),
    /buffer cap/,
  );
  assert.equal(seedance.MAX_SSE_BYTES, 4 * 1024 * 1024);
  assert.equal(seedance.MAX_DATA_URI_BYTES, 25 * 1024 * 1024);
  assert.throws(
    () => seedance.buildCreateBody({ model: 'doubao-seedance-2.0', prompt: 'x', imageUrls: ['https://evil.example/x.png'] }),
    /data URI/i,
  );
  assert.throws(
    () => seedream.buildGenerateBody({ model: 'doubao-seedream-5.0-pro', prompt: 'x', referenceImages: ['https://evil.example/x.png'] }),
    /data URI/i,
  );
});

test('credentials, Data URIs, signed URLs and machine JSON are redacted', () => {
  const sanitized = seedance.sanitizeValue({
    api_key: ARK_KEY,
    authorization: 'Bearer ' + ARK_KEY,
    nested: {
      token: ARK_KEY,
      payload: DATA_URI_PNG,
      url: 'https://media.example.com/a.mp4?X-Amz-Signature=deadbeefdeadbeef&x=1',
    },
  });
  const text = JSON.stringify(sanitized);
  assert.ok(!text.includes('not-a-real-key'));
  assert.ok(!text.includes('iVBORw0KGgo'));
  assert.ok(!text.includes('deadbeef'));
  assert.ok(text.includes('[redacted]'));

  const line = seedance.machineJson({ api_key: ARK_KEY, ok: true });
  assert.equal(line.includes('\n'), false);
  assert.ok(!line.includes('not-a-real-key'));
});

test('synchronous polling fails closed for bad task ids, failure, timeout and missing output', async () => {
  await assert.rejects(
    seedance.pollTask({ env: { ARK_API_KEY: ARK_KEY }, fetchImpl: fetchOnce({ id: TASK_ID }), taskId: '../escape' }),
    /invalid Seedance task id/,
  );
  await assert.rejects(
    seedance.pollTask({ env: { ARK_API_KEY: ARK_KEY }, fetchImpl: fetchOnce({ id: TASK_ID, status: 'succeeded', content: {} }), taskId: TASK_ID }),
    /without output/,
  );
  await assert.rejects(
    seedance.pollTask({ env: { ARK_API_KEY: ARK_KEY }, fetchImpl: fetchOnce({ id: TASK_ID, status: 'failed' }), taskId: TASK_ID }),
    /ended as failed/,
  );
  let clock = 0;
  await assert.rejects(
    seedance.pollTask({
      env: { ARK_API_KEY: ARK_KEY },
      fetchImpl: fetchOnce({ id: TASK_ID, status: 'running' }),
      taskId: TASK_ID,
      timeoutMs: 1,
      now: () => {
        clock += 10;
        return clock;
      },
      sleep: async () => {},
    }),
    /timed out/,
  );
  const ok = await seedance.pollTask({
    env: { ARK_API_KEY: ARK_KEY },
    fetchImpl: fetchOnce({ id: TASK_ID, status: 'succeeded', model: 'doubao-seedance-2.0', content: { video_url: 'https://media.example.com/v.mp4' } }),
    taskId: TASK_ID,
  });
  assert.equal(ok.provider, 'volcengine-agentplan');
  assert.equal(ok.outputs.length, 1);
});

test('provider descriptors never download output and emit producer metadata', async () => {
  const fetchImpl = fetchOnce({ id: TASK_ID, status: 'queued' });
  const descriptor = await seedance.createTask({
    env: { ARK_API_KEY: ARK_KEY },
    fetchImpl: fetchImpl,
    input: { model: 'doubao-seedance-2.0', prompt: 'x' },
  });
  assert.equal(fetchImpl.calls.length, 1);
  assert.equal(descriptor.provider, 'volcengine-agentplan');
  assert.equal(JSON.stringify(descriptor).includes('download'), false);
  assert.deepEqual(seedance.producerMetadata('sha256:tree'), { id: 'byted-ark-seedance-skill', version: '5.0.0', tree_sha256: 'sha256:tree' });
  assert.deepEqual(seedream.producerMetadata('sha256:tree'), { id: 'byted-ark-seedream-skill', version: '4.0.0', tree_sha256: 'sha256:tree' });
});

test('Seedream model allowlist, Data URIs and descriptors are enforced', async () => {
  for (const model of ['doubao-seedream-5.0-lite', 'doubao-seedream-5.0-pro']) {
    assert.equal(seedream.assertModel(model), model);
  }
  assert.throws(() => seedream.assertModel('doubao-seedream-4.0'), /allowlist/);
  assert.throws(
    () => seedream.buildGenerateBody({ model: 'doubao-seedream-5.0-pro', prompt: 'x', downloadDir: '/tmp/x' }),
    /disabled Seedream input/,
  );
  const fetchImpl = fetchOnce({ model: 'doubao-seedream-5.0-pro', data: [{ url: 'https://media.example.com/a.png' }] });
  const descriptor = await seedream.generate({
    env: { ARK_API_KEY: ARK_KEY },
    fetchImpl: fetchImpl,
    input: { model: 'doubao-seedream-5.0-pro', prompt: 'x', referenceImages: [DATA_URI_PNG] },
  });
  assert.equal(fetchImpl.calls.length, 1);
  assert.equal(new URL(fetchImpl.calls[0].url).origin, seedream.API_ORIGIN);
  assert.equal(descriptor.provider, 'volcengine-agentplan');
  assert.equal(descriptor.images.length, 1);
  assert.equal('download' in descriptor, false);
  const body = seedream.buildGenerateBody({ model: 'doubao-seedream-5.0-pro', prompt: 'x', referenceImages: [DATA_URI_PNG] });
  assert.equal(body.response_format, 'url');
  assert.equal(body.image, DATA_URI_PNG);
});

test('the patched upstream Seedream CLI refuses direct execution', () => {
  const result = spawnSync(process.execPath, [path.join(PATCHED_DIR, 'byted-ark-seedream-skill/scripts/generate.js')], { encoding: 'utf8' });
  assert.equal(result.status, 2);
  assert.match(result.stderr, /disabled/);
});
