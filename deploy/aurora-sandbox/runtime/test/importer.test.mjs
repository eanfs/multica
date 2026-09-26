// Step-7 provider URL validation and staged-object import contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { assertProviderResultUrl, createHttpImporter, normalizeImporter } from '../src/importer.mjs';
import { startFakeServer, uuid } from './helpers.mjs';

test('only public HTTPS provider result URLs are accepted', () => {
  assert.equal(assertProviderResultUrl('https://ark-cn-beijing.volces.com/a.png'), 'https://ark-cn-beijing.volces.com/a.png');
  assert.equal(assertProviderResultUrl('https://cdn.example.com/a/b.png?sig=1'), 'https://cdn.example.com/a/b.png?sig=1');
  for (const bad of [
    'http://cdn.example.com/a.png',
    'https://user:pass@cdn.example.com/a.png',
    'https://localhost/a.png',
    'https://127.0.0.1/a.png',
    'https://10.0.0.1/a.png',
    'https://192.168.1.1/a.png',
    'https://169.254.0.1/a.png',
    'https://[::1]/a.png',
    'https://service.internal/a.png',
    'ftp://cdn.example.com/a.png',
    'not a url',
    '',
  ]) {
    assert.throws(() => assertProviderResultUrl(bad), undefined, bad);
  }
});

test('the http importer posts a bounded request and returns a staging id', async () => {
  const fake = await startFakeServer((req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify({ staging_id: uuid(40), name: 'a.png', size_bytes: 12, sha256: 'sha256:' + 'b'.repeat(64) }));
  });
  try {
    const importer = createHttpImporter({
      serverOrigin: 'https://multica.test',
      taskToken: 'mat-import-token',
      taskId: uuid(1),
      path: '/api/agent/tasks/' + uuid(1) + '/aurora-artifacts/import',
      fetchImpl: fake.fetchImpl,
    });
    const result = await importer({ url: 'https://cdn.example.com/a.png?sig=secret', kind: 'image', name: 'a.png', mimeType: 'image/png', sizeBytes: 12 });
    assert.equal(result.staging_id, uuid(40));
    assert.equal(fake.calls[0].method, 'POST');
    assert.equal(fake.calls[0].headers.authorization, 'Bearer mat-import-token');
    const body = JSON.parse(fake.calls[0].bodyText);
    assert.equal(body.url, 'https://cdn.example.com/a.png?sig=secret');
    assert.equal(body.kind, 'image');
    assert.equal(body.name, 'a.png');
  } finally {
    await fake.close();
  }
});

test('normalizeImporter accepts a function or an importProviderObject client', () => {
  const fn = async () => ({ staging_id: 'x' });
  assert.equal(normalizeImporter(fn), fn);
  const client = { importProviderObject: fn };
  assert.equal(normalizeImporter(client), fn);
  assert.throws(() => normalizeImporter(null), /importer/);
});
