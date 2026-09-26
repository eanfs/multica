// Step 7: pass Volcengine result URLs to the task-scoped server importer.
//
// The sandbox never fetches, prints, returns, or persists the URL. Task 6 owns
// the real endpoint; this client only validates the URL and calls the injected
// importer path.

import { sanitizeError } from './policy.mjs';

const PRIVATE_IPV4 = [
  /^0\./,
  /^10\./,
  /^127\./,
  /^169\.254\./,
  /^172\.(1[6-9]|2\d|3[01])\./,
  /^192\.168\./,
  /^100\.(6[4-9]|[7-9]\d|1[0-2]\d)\./,
];

const BLOCKED_HOST_SUFFIXES = ['.local', '.internal', '.localhost', '.home', '.lan'];

export function assertProviderResultUrl(raw) {
  if (typeof raw !== 'string' || raw.length === 0) throw new Error('provider result URL is required');
  let url;
  try {
    url = new URL(raw);
  } catch {
    throw new Error('provider result URL is not a valid URL');
  }
  if (url.protocol !== 'https:') throw new Error('provider result URL must be HTTPS');
  if (url.username || url.password) throw new Error('provider result URL must not contain credentials');
  const host = url.hostname.toLowerCase();
  if (host.length === 0) throw new Error('provider result URL must have a host');
  if (host === 'localhost' || host.endsWith('localhost')) throw new Error('provider result URL must be public');
  for (const suffix of BLOCKED_HOST_SUFFIXES) {
    if (host.endsWith(suffix)) throw new Error('provider result URL must be public');
  }
  const bare = host.startsWith('[') ? host.slice(1, -1) : host;
  const isIpv6 = bare.includes(':');
  if (isIpv6) {
    if (bare === '::1' || bare.startsWith('fe80:') || bare.startsWith('fc') || bare.startsWith('fd')) {
      throw new Error('provider result URL must be public');
    }
  } else if (/^\d{1,3}(\.\d{1,3}){3}$/.test(bare)) {
    if (PRIVATE_IPV4.some((pattern) => pattern.test(bare))) throw new Error('provider result URL must be public');
  }
  return raw;
}

export function normalizeImporter(importer) {
  if (typeof importer === 'function') return importer;
  if (importer && typeof importer.importProviderObject === 'function') return importer.importProviderObject;
  throw new Error('a task-scoped artifact importer is required');
}

export function createHttpImporter({ serverOrigin, taskToken, taskId, path, fetchImpl = globalThis.fetch, timeoutMs = 60000 }) {
  if (typeof path !== 'string' || path.length === 0) throw new Error('importer path is required');
  return async function importProviderObject(request) {
    assertProviderResultUrl(request.url);
    const response = await fetchImpl(serverOrigin + path, {
      method: 'POST',
      headers: { authorization: 'Bearer ' + taskToken, 'content-type': 'application/json' },
      body: JSON.stringify({
        url: request.url,
        kind: request.kind,
        name: request.name,
        mime_type: request.mimeType,
        size_bytes: request.sizeBytes ?? null,
        metadata: request.metadata ?? {},
      }),
      signal: Number.isInteger(timeoutMs) && timeoutMs > 0 ? AbortSignal.timeout(timeoutMs) : undefined,
    });
    const text = await response.text();
    if (response.status >= 400) {
      throw sanitizeError(new Error('artifact import failed with status ' + response.status + ': ' + text.slice(0, 512)));
    }
    let parsed;
    try {
      parsed = text ? JSON.parse(text) : {};
    } catch {
      throw new Error('artifact import response was not valid JSON');
    }
    if (!parsed.staging_id) throw new Error('artifact import response is missing a staging id');
    return parsed;
  };
}
