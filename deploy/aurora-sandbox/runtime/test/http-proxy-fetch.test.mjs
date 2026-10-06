// Absolute-form HTTP proxying for the sandbox's server-origin calls.
//
// The egress sidecar only authorizes the configured server origin as an
// absolute-form forward request; Node's global fetch cannot produce one (it
// ignores HTTP_PROXY, and NODE_USE_ENV_PROXY tunnels via CONNECT, which the
// sidecar refuses for the http server origin). These tests pin the broker's
// replacement fetch.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createHttpProxyFetch } from '../src/transport.mjs';

function startProxy(handler) {
  const server = http.createServer(handler);
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ url: 'http://127.0.0.1:' + port, close: () => new Promise((done) => server.close(done)) });
    });
  });
}

function readBody(request) {
  return new Promise((resolve) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
  });
}

test('no proxy URL returns the injected fetch unchanged', () => {
  const fetchImpl = async () => new Response('x');
  assert.equal(createHttpProxyFetch({ fetchImpl }), fetchImpl);
  assert.equal(createHttpProxyFetch({ proxyUrl: '   ', fetchImpl }), fetchImpl);
});

test('plain HTTP is forwarded in absolute-form with the target Host', async () => {
  const seen = {};
  const proxy = await startProxy(async (request, response) => {
    seen.url = request.url;
    seen.host = request.headers.host;
    seen.method = request.method;
    seen.authorization = request.headers.authorization;
    seen.body = await readBody(request);
    response.writeHead(201, { 'content-type': 'application/json' });
    response.end('{"ok":true}');
  });
  try {
    const fetchImpl = createHttpProxyFetch({ proxyUrl: proxy.url });
    const response = await fetchImpl('http://server.test/health', {
      method: 'POST',
      headers: { authorization: 'Bearer task-token', host: 'attacker.test' },
      body: '{"a":1}',
    });
    assert.equal(response.status, 201);
    assert.equal(await response.text(), '{"ok":true}');
    assert.equal(seen.url, 'http://server.test/health');
    assert.equal(seen.host, 'server.test');
    assert.equal(seen.method, 'POST');
    assert.equal(seen.authorization, 'Bearer task-token');
    assert.equal(seen.body, '{"a":1}');
  } finally {
    await proxy.close();
  }
});

test('HTTPS and NO_PROXY targets fall through to the injected fetch', async () => {
  const calls = [];
  const fetchImpl = async (url) => {
    calls.push(url);
    return new Response('direct', { status: 200 });
  };
  const fetchWithProxy = createHttpProxyFetch({
    proxyUrl: 'http://127.0.0.1:9',
    noProxy: 'egress,127.0.0.1,localhost',
    fetchImpl,
  });
  await fetchWithProxy('https://ark.cn-beijing.volces.com/x');
  await fetchWithProxy('http://127.0.0.1:18102/health');
  await fetchWithProxy('http://localhost:18102/health');
  assert.deepEqual(calls, [
    'https://ark.cn-beijing.volces.com/x',
    'http://127.0.0.1:18102/health',
    'http://localhost:18102/health',
  ]);
});

test('an unreachable proxy rejects instead of silently dialing direct', async () => {
  const fetchImpl = createHttpProxyFetch({
    proxyUrl: 'http://127.0.0.1:9',
    fetchImpl: async () => new Response('should-not-run'),
  });
  await assert.rejects(fetchImpl('http://server.test/health'));
});
