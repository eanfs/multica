// Offline test helpers for the Aurora sandbox broker.
//
// Everything here is local: fake HTTP servers, temp directories, and injected
// transports. No test in this suite contacts a real provider or paid API.

import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

export const HERE = path.dirname(fileURLToPath(import.meta.url));
export const RUNTIME_ROOT = path.resolve(HERE, '..');
export const REPO_ROOT = path.resolve(RUNTIME_ROOT, '../../..');
export const VENDOR_DIR = path.join(REPO_ROOT, 'deploy/aurora-sandbox/vendor/volcengine');

export function uuid(seed = 0) {
  const hex = seed.toString(16).padStart(12, '0');
  return `00000000-0000-4000-8000-${hex}`;
}

export function sha256Bytes(buffer) {
  return 'sha256:' + crypto.createHash('sha256').update(buffer).digest('hex');
}

export function tempRoot(prefix = 'aurora-broker-') {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}

export function makeWorkspace() {
  const root = tempRoot();
  const inputRoot = path.join(root, 'input');
  const outputRoot = path.join(root, 'output');
  const secrets = path.join(root, 'secrets');
  fs.mkdirSync(inputRoot, { recursive: true });
  fs.mkdirSync(outputRoot, { recursive: true });
  fs.mkdirSync(secrets, { recursive: true });
  return { root, inputRoot, outputRoot, secrets };
}

export function writeInput(ws, relativePath, bytes) {
  const target = path.join(ws.inputRoot, relativePath);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, bytes);
  return target;
}

export function writeOutput(ws, relativePath, bytes) {
  const target = path.join(ws.outputRoot, relativePath);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, bytes);
  return target;
}

export function writeSecret(ws, name, value) {
  const target = path.join(ws.secrets, name);
  fs.writeFileSync(target, value, { mode: 0o400 });
  fs.chmodSync(target, 0o400);
  return target;
}

export const PNG_MAGIC = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
export function pngBytes(size = 64) {
  const buffer = Buffer.alloc(size, 0);
  PNG_MAGIC.copy(buffer);
  return buffer;
}

export function attachmentEntry(relativePath, mimeType, sizeBytes) {
  return { relative_path: relativePath, mime_type: mimeType, size_bytes: sizeBytes };
}

export function baseContext(ws, overrides = {}) {
  return {
    schema: 'com.multica.aurora.task-context',
    version: 1,
    task_id: uuid(1),
    generation_id: uuid(2),
    workspace_id: uuid(3),
    skill_id: 'poster',
    prompt: 'a poster of a cat',
    attachments: {},
    output_root: ws.outputRoot,
    server_origin: 'https://multica.test',
    task_token_file: path.join(ws.secrets, 'task-token'),
    ...overrides,
  };
}

export function writeContextFile(ws, context, { mode = 0o400, name = 'task-context.json' } = {}) {
  const target = path.join(ws.root, name);
  fs.writeFileSync(target, JSON.stringify(context), { mode });
  fs.chmodSync(target, mode);
  return target;
}

// A local HTTP fake. The returned fetchImpl records every outbound call and
// forwards it to the local server, preserving method, headers, and body.
export async function startFakeServer(handler) {
  const calls = [];
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = Buffer.concat(chunks);
    const call = {
      method: req.method,
      url: req.url,
      headers: req.headers,
      body,
      bodyText: body.toString('utf8'),
      origin: 'http://127.0.0.1:' + server.address().port,
    };
    calls.push(call);
    try {
      await handler(req, res, call);
    } catch (error) {
      res.statusCode = 500;
      res.end(String(error && error.stack ? error.stack : error));
    }
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  const origin = 'http://127.0.0.1:' + address.port;
  const fetchImpl = async (url, init = {}) => {
    const parsed = new URL(url);
    const target = new URL(parsed.pathname + parsed.search, origin);
    return fetch(target, {
      method: init.method || 'GET',
      headers: init.headers,
      body: init.body,
      redirect: init.redirect || 'manual',
      signal: init.signal,
    });
  };
  fetchImpl.calls = calls;
  fetchImpl.origin = origin;
  return {
    server,
    origin,
    calls,
    fetchImpl,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

// A pure fetch recorder for FormData/multipart bodies that are awkward to
// forward. It never touches the network.
function normalizeHeaders(headers) {
  if (!headers) return {};
  if (typeof headers.forEach === 'function' && typeof headers.get === 'function' && !Array.isArray(headers)) {
    const out = {};
    headers.forEach((value, key) => {
      out[String(key).toLowerCase()] = value;
    });
    return out;
  }
  const out = {};
  for (const [key, value] of Object.entries(headers)) out[String(key).toLowerCase()] = value;
  return out;
}

async function drainBody(body) {
  if (body === undefined || body === null) return '';
  if (typeof body === 'string') return body;
  if (Buffer.isBuffer(body)) return body.toString('utf8');
  if (typeof body.getReader === 'function') {
    const reader = body.getReader();
    const chunks = [];
    for (;;) {
      const next = await reader.read();
      if (next.done) break;
      chunks.push(Buffer.from(next.value));
    }
    return Buffer.concat(chunks).toString('utf8');
  }
  if (typeof body.arrayBuffer === 'function') return Buffer.from(await body.arrayBuffer()).toString('utf8');
  return String(body);
}

export function createFetchRecorder(respond) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const call = {
      url: String(url),
      init,
      method: init.method || 'GET',
      headers: normalizeHeaders(init.headers),
      bodyText: await drainBody(init.body),
    };
    calls.push(call);
    const result = respond ? await respond(call) : {};
    const status = result.status === undefined ? 200 : result.status;
    const body = result.body === undefined ? {} : result.body;
    return new Response(typeof body === 'string' ? body : JSON.stringify(body), {
      status,
      headers: { 'content-type': 'application/json', ...(result.headers || {}) },
    });
  };
  fetchImpl.calls = calls;
  return fetchImpl;
}

export function jsonResponse(body, status = 200, headers = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  });
}

// Materializes the patched Seedream vendor module exactly like the image build
// does: copy the vendored tree, then git-apply the patch series. Returns a path.
export function materializePatchedVendor() {
  const lock = JSON.parse(fs.readFileSync(path.join(VENDOR_DIR, 'vendor-lock.json'), 'utf8'));
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'aurora-broker-vendor-'));
  for (const [skillId, entry] of Object.entries(lock.skills)) {
    const source = path.join(VENDOR_DIR, skillId);
    if (entry.vendored && fs.existsSync(source)) {
      fs.cpSync(source, path.join(dir, skillId), { recursive: true });
    }
  }
  execFileSync('git', ['init', '-q'], { cwd: dir, stdio: 'pipe' });
  for (const entry of Object.values(lock.skills)) {
    for (const patch of entry.patches) {
      execFileSync('git', ['apply', path.join(VENDOR_DIR, patch.path)], { cwd: dir, stdio: 'pipe' });
    }
  }
  return dir;
}

export function loadCjsModule(filePath) {
  const require = createRequire(import.meta.url);
  return require(filePath);
}

export function readManifest(outputRoot) {
  const manifestPath = path.join(outputRoot, '.multica/aurora-artifacts.v1.json');
  return JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
}

// Minimal deterministic ZIP writer for DOCX extraction tests. Supports stored
// and deflate entries, which is all a DOCX archive needs.
export function makeZip(entries) {
  const zlib = require('node:zlib');
  const localParts = [];
  const centralParts = [];
  let offset = 0;
  for (const entry of entries) {
    const name = Buffer.from(entry.name, 'utf8');
    const raw = Buffer.isBuffer(entry.data) ? entry.data : Buffer.from(entry.data || '', 'utf8');
    const method = entry.deflate ? 8 : 0;
    const compressed = method === 8 ? zlib.deflateRawSync(raw) : raw;
    const crc = crc32(raw);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0x0800, 6);
    local.writeUInt16LE(method, 8);
    local.writeUInt16LE(0, 10);
    local.writeUInt16LE(0, 12);
    local.writeUInt32LE(crc >>> 0, 14);
    local.writeUInt32LE(compressed.length, 18);
    local.writeUInt32LE(raw.length, 22);
    local.writeUInt16LE(name.length, 26);
    local.writeUInt16LE(0, 28);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE(20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0x0800, 8);
    central.writeUInt16LE(method, 10);
    central.writeUInt16LE(0, 12);
    central.writeUInt16LE(0, 14);
    central.writeUInt32LE(crc >>> 0, 16);
    central.writeUInt32LE(compressed.length, 20);
    central.writeUInt32LE(raw.length, 24);
    central.writeUInt16LE(name.length, 28);
    central.writeUInt16LE(0, 30);
    central.writeUInt16LE(0, 32);
    central.writeUInt16LE(0, 34);
    central.writeUInt16LE(0, 36);
    central.writeUInt32LE(0, 38);
    central.writeUInt32LE(offset, 42);
    localParts.push(local, name, compressed);
    centralParts.push(central, name);
    offset += local.length + name.length + compressed.length;
  }
  const centralBuffer = Buffer.concat(centralParts);
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0);
  eocd.writeUInt16LE(0, 4);
  eocd.writeUInt16LE(0, 6);
  eocd.writeUInt16LE(entries.length, 8);
  eocd.writeUInt16LE(entries.length, 10);
  eocd.writeUInt32LE(centralBuffer.length, 12);
  eocd.writeUInt32LE(offset, 16);
  eocd.writeUInt16LE(0, 20);
  return Buffer.concat([...localParts, centralBuffer, eocd]);
}

export function makeDocx(paragraphs) {
  const body = paragraphs.map((text) => '<w:p><w:r><w:t>' + escapeXml(text) + '</w:t></w:r></w:p>').join('');
  const document = '<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>' + body + '</w:body></w:document>';
  return makeZip([
    { name: '[Content_Types].xml', data: '<?xml version="1.0"?><Types/>' },
    { name: 'word/document.xml', data: document, deflate: true },
  ]);
}

function escapeXml(value) {
  return String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function crc32(buffer) {
  let crc = 0xffffffff;
  for (let i = 0; i < buffer.length; i += 1) {
    crc ^= buffer[i];
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}


export async function readToolResult(result) {
  if (result && result.structuredContent) return result.structuredContent;
  if (result && Array.isArray(result.content)) {
    const text = result.content.find((item) => item.type === 'text');
    if (text) return JSON.parse(text.text);
  }
  return result;
}

export function fakeProviderRun({ externalId = null, state = 'submitted', createAllowed = true, beginResult } = {}) {
  const calls = [];
  let current = externalId
    ? { provider: 'x', operation: 'x', model: 'y', state, external_id: externalId, create_allowed: false }
    : null;
  return {
    calls,
    async begin(input) {
      calls.push({ kind: 'begin', ...input });
      if (beginResult) return beginResult;
      current = {
        ...input,
        external_id: null,
        state: 'creating',
        create_allowed: createAllowed,
      };
      return { ...current };
    },
    async recordExternal(operation, id) {
      calls.push({ kind: 'recordExternal', operation, externalId: id });
      current = { ...current, operation, external_id: id, state: 'submitted' };
      return { ...current };
    },
    async finish(operation, finishState, errorCode) {
      calls.push({ kind: 'finish', operation, state: finishState, errorCode });
      current = { ...current, operation, state: finishState };
      return { ...current };
    },
    async get(operation) {
      calls.push({ kind: 'get', operation });
      if (!current) {
        const error = new Error('provider run not found');
        error.status = 404;
        throw error;
      }
      return { ...current, operation };
    },
  };
}
