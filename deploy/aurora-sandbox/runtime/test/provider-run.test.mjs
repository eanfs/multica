// Create-once provider-run HTTP client contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createProviderRunClient } from '../src/provider-run.mjs';
import { startFakeServer, uuid } from './helpers.mjs';

test('begin/external/finish/get use the task-token endpoints and bearer header', async () => {
  const fake = await startFakeServer((req, res, call) => {
    res.setHeader('content-type', 'application/json');
    if (call.url.endsWith('/begin')) {
      res.end(JSON.stringify({ id: uuid(30), operation: 'seedream.generate', state: 'creating', create_allowed: true }));
    } else if (call.url.endsWith('/external')) {
      res.end(JSON.stringify({ id: uuid(30), operation: 'seedream.generate', state: 'submitted', external_id: 'cgt-1', create_allowed: false }));
    } else if (call.url.endsWith('/finish')) {
      res.end(JSON.stringify({ id: uuid(30), operation: 'seedream.generate', state: 'succeeded', create_allowed: false }));
    } else {
      res.end(JSON.stringify({ id: uuid(30), operation: 'seedream.generate', state: 'succeeded', external_id: 'cgt-1', create_allowed: false }));
    }
  });
  try {
    const client = createProviderRunClient({
      serverOrigin: 'https://multica.test',
      taskToken: 'mat-secret-token',
      taskId: uuid(1),
      fetchImpl: fake.fetchImpl,
    });
    const begun = await client.begin({ provider: 'volcengine-agentplan', operation: 'seedream.generate', model: 'doubao-seedream-5.0-pro', args: { prompt: 'x' } });
    assert.equal(begun.create_allowed, true);
    await client.recordExternal('seedream.generate', 'cgt-1');
    await client.finish('seedream.generate', 'succeeded');
    const run = await client.get('seedream.generate');

    assert.equal(fake.calls[0].method, 'POST');
    assert.equal(fake.calls[0].url, '/api/agent/tasks/' + uuid(1) + '/aurora-provider-runs/begin');
    assert.equal(fake.calls[0].headers.authorization, 'Bearer mat-secret-token');
    assert.deepEqual(JSON.parse(fake.calls[0].bodyText), {
      provider: 'volcengine-agentplan',
      operation: 'seedream.generate',
      model: 'doubao-seedream-5.0-pro',
      arguments: { prompt: 'x' },
    });
    assert.equal(fake.calls[1].method, 'PUT');
    assert.equal(fake.calls[1].url, '/api/agent/tasks/' + uuid(1) + '/aurora-provider-runs/seedream.generate/external');
    assert.deepEqual(JSON.parse(fake.calls[1].bodyText), { external_id: 'cgt-1' });
    assert.equal(fake.calls[2].method, 'PUT');
    assert.equal(fake.calls[2].url, '/api/agent/tasks/' + uuid(1) + '/aurora-provider-runs/seedream.generate/finish');
    assert.deepEqual(JSON.parse(fake.calls[2].bodyText), { state: 'succeeded' });
    assert.equal(fake.calls[3].method, 'GET');
    assert.equal(run.external_id, 'cgt-1');
    // The task token never appears in a request body.
    for (const call of fake.calls) assert.doesNotMatch(call.bodyText, /mat-secret-token/);
  } finally {
    await fake.close();
  }
});

test('a 404 from get is a typed not-found error', async () => {
  const fake = await startFakeServer((req, res) => {
    res.statusCode = 404;
    res.end(JSON.stringify({ error: 'not found' }));
  });
  try {
    const client = createProviderRunClient({ serverOrigin: 'https://multica.test', taskToken: 'mat-x', taskId: uuid(1), fetchImpl: fake.fetchImpl });
    await assert.rejects(client.get('seedance.create'), (error) => error.status === 404);
  } finally {
    await fake.close();
  }
});

test('provider-run errors are sanitized', async () => {
  const fake = await startFakeServer((req, res) => {
    res.statusCode = 500;
    res.end('failed with mat-supersecret and ark-abcdefgh');
  });
  try {
    const client = createProviderRunClient({ serverOrigin: 'https://multica.test', taskToken: 'mat-x', taskId: uuid(1), fetchImpl: fake.fetchImpl });
    await assert.rejects(client.begin({ provider: 'p', operation: 'o', model: 'm', args: {} }), (error) => {
      assert.doesNotMatch(error.message, /ark-abcdefgh/);
      return true;
    });
  } finally {
    await fake.close();
  }
});
