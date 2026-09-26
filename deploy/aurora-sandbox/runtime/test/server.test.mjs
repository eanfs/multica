// MCP surface contract: exactly the nine named tools are registered and callable.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import fs from 'node:fs';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { createBroker, registerTools, TOOL_NAMES } from '../src/server.mjs';
import { makeWorkspace, baseContext, writeContextFile } from './helpers.mjs';

async function connectedClient() {
  const ws = makeWorkspace();
  fs.writeFileSync(path.join(ws.secrets, 'task-token'), 'mat-task-token', { mode: 0o400 });
  fs.chmodSync(path.join(ws.secrets, 'task-token'), 0o400);
  const contextPath = writeContextFile(ws, baseContext(ws, { skill_id: 'xhs-copy' }));
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
    fetchImpl: async () => { throw new Error('no network in the MCP surface test'); },
    providerRun: { async begin() { throw new Error('no provider runs in the MCP surface test'); } },
    processRunner: { async run() { throw new Error('no processes in the MCP surface test'); } },
    vendor: {},
    importer: async () => { throw new Error('no imports in the MCP surface test'); },
  });
  const server = new McpServer({ name: 'aurora-sandbox-broker', version: 'test' });
  registerTools(server, broker);
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: 'test-client', version: '1.0.0' });
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
  return { client, broker, ws };
}

test('the MCP server exposes exactly the nine Aurora methods', async () => {
  const { client } = await connectedClient();
  const listed = await client.listTools();
  assert.deepEqual(listed.tools.map((tool) => tool.name).sort(), [...TOOL_NAMES].sort());
});

test('a tool call reaches the broker and a forbidden argument fails closed', async () => {
  const { client } = await connectedClient();
  const ok = await client.callTool({ name: 'aurora.write_text_artifact', arguments: { content: '# Title', name: 'summary.md' } });
  assert.equal(ok.isError, undefined);
  const bad = await client.callTool({ name: 'aurora.write_text_artifact', arguments: { content: '# Title', name: 'summary.md', model: 'evil' } });
  assert.equal(bad.isError, true);
});
