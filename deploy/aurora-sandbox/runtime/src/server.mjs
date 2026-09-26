// MCP entry point for the narrow Aurora sandbox broker.
//
// The broker exposes exactly nine named tools. There is no Bash, browser, or
// arbitrary-file tool, and no tool may name a provider, model, origin, or
// callback.

import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { z } from 'zod';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  DEFAULT_MODELS,
  PROVIDER_ORIGINS,
  producerForSkill,
  sanitizeError,
  sanitizeValue,
} from './policy.mjs';
import {
  loadTaskContext,
  readSecret,
  DEFAULT_INPUT_ROOT,
  DEFAULT_OUTPUT_ROOT,
  DEFAULT_SECRET_PATHS,
} from './task-context.mjs';
import { createManifest } from './manifest.mjs';
import { createProviderRunClient } from './provider-run.mjs';
import { createProviderFetch, createProcessRunner } from './transport.mjs';
import { createHttpImporter, normalizeImporter } from './importer.mjs';
import { seedreamGenerate } from './tools/seedream.mjs';
import { seedanceGenerate } from './tools/seedance.mjs';
import { openaiImage } from './tools/openai-images.mjs';
import { volcAsrTranscribe } from './tools/volc-asr.mjs';
import { readDocument } from './tools/documents.mjs';
import { idPhoto } from './tools/id-photo.mjs';
import { renderVideoCaptions } from './tools/hyperframes.mjs';
import { renderResume } from './tools/resume.mjs';
import { writeTextArtifact } from './tools/text-artifact.mjs';

const require = createRequire(import.meta.url);

export const TOOL_NAMES = Object.freeze([
  'aurora.seedream_generate',
  'aurora.seedance_generate',
  'aurora.openai_image',
  'aurora.volc_asr_transcribe',
  'aurora.read_document',
  'aurora.id_photo',
  'aurora.render_video_captions',
  'aurora.render_resume',
  'aurora.write_text_artifact',
]);

const TOOL_ARGUMENTS = Object.freeze({
  'aurora.seedream_generate': ['prompt', 'attachment_ids', 'output_name'],
  'aurora.seedance_generate': ['prompt', 'attachment_ids', 'output_name'],
  'aurora.openai_image': ['prompt', 'attachment_ids', 'output_name'],
  'aurora.volc_asr_transcribe': ['attachment_id', 'output_name'],
  'aurora.read_document': ['attachment_id'],
  'aurora.id_photo': ['attachment_id', 'output_name'],
  'aurora.render_video_captions': ['attachment_id', 'cues', 'output_name'],
  'aurora.render_resume': ['sections', 'output_name'],
  'aurora.write_text_artifact': ['content', 'name'],
});

const TOOL_HANDLERS = Object.freeze({
  'aurora.seedream_generate': seedreamGenerate,
  'aurora.seedance_generate': seedanceGenerate,
  'aurora.openai_image': openaiImage,
  'aurora.volc_asr_transcribe': volcAsrTranscribe,
  'aurora.read_document': readDocument,
  'aurora.id_photo': idPhoto,
  'aurora.render_video_captions': renderVideoCaptions,
  'aurora.render_resume': renderResume,
  'aurora.write_text_artifact': writeTextArtifact,
});

const DEFAULT_VENDOR_DIR = '/opt/aurora/vendor/volcengine';

function loadVendorModule(vendorDir, skillDirectory, scriptName, injected) {
  if (injected) return injected;
  try {
    return require(path.join(vendorDir, skillDirectory, 'scripts', scriptName));
  } catch {
    return null;
  }
}

function normalizeStaging(response) {
  if (!response || typeof response !== 'object') throw new Error('artifact importer returned an invalid response');
  const stagingId = response.staging_id || response.stagingId;
  if (!stagingId) throw new Error('artifact importer returned no staging id');
  return { staging_id: stagingId, size_bytes: response.size_bytes ?? response.sizeBytes ?? 0, sha256: response.sha256 };
}

export function createBroker(options = {}) {
  const secretPaths = { ...DEFAULT_SECRET_PATHS, ...(options.secretPaths || {}) };
  const allowedSecretPaths = options.allowedSecretPaths || Object.values(secretPaths);
  const inputRoot = options.inputRoot || DEFAULT_INPUT_ROOT;
  const outputRoot = options.outputRoot || DEFAULT_OUTPUT_ROOT;
  const serverOrigin = options.serverOrigin;
  if (!options.context && (typeof serverOrigin !== 'string' || serverOrigin.length === 0)) {
    throw new Error('server origin is required');
  }
  const context = options.context || loadTaskContext({ contextPath: options.contextPath, inputRoot, outputRoot, serverOrigin, allowedSecretPaths });
  const fetchImpl = options.fetchImpl || globalThis.fetch;
  const providerFetch = options.providerFetch || createProviderFetch({
    fetchImpl,
    allowedOrigins: Object.values(PROVIDER_ORIGINS),
    timeoutMs: options.providerTimeoutMs ?? 120000,
  });
  const processRunner = options.processRunner || createProcessRunner();

  let importer = null;
  if (options.importer) {
    const delegate = normalizeImporter(options.importer);
    importer = async (request) => normalizeStaging(await delegate(request));
  } else if (options.importerPath) {
    const delegate = createHttpImporter({
      serverOrigin,
      taskToken: readSecret(context.taskTokenFile, { allowedPaths: allowedSecretPaths }),
      taskId: context.taskId,
      path: options.importerPath,
      fetchImpl,
      timeoutMs: options.importerTimeoutMs,
    });
    importer = async (request) => normalizeStaging(await delegate(request));
  }

  const providerRun = options.providerRun || createProviderRunClient({
    serverOrigin,
    taskToken: readSecret(context.taskTokenFile, { allowedPaths: allowedSecretPaths }),
    taskId: context.taskId,
    fetchImpl,
    timeoutMs: options.providerRunTimeoutMs,
  });

  const vendorDir = (options.vendor && options.vendor.vendorDir) || DEFAULT_VENDOR_DIR;
  const vendor = {
    seedream: loadVendorModule(vendorDir, 'byted-ark-seedream-skill', 'seedream-broker.js', options.vendor && options.vendor.seedreamModule),
    seedance: loadVendorModule(vendorDir, 'byted-ark-seedance-skill', 'seedance-broker.js', options.vendor && options.vendor.seedanceModule),
  };

  function secretReader(kind, label) {
    return () => {
      try {
        return readSecret(secretPaths[kind], { allowedPaths: allowedSecretPaths });
      } catch {
        throw new Error(label + ' credential is unavailable');
      }
    };
  }

  const manifest = options.manifest || createManifest({
    outputRoot: context.outputRoot,
    taskId: context.taskId,
    skillId: context.skillId,
    producer: producerForSkill(context.skillId),
  });

  const broker = {
    context,
    manifest,
    providerRun,
    processRunner,
    importer,
    vendor,
    transport: { providerFetch },
    secrets: {
      ark: secretReader('ark', 'ARK'),
      openai: secretReader('openai', 'OpenAI'),
      volcAsr: secretReader('volcAsr', 'Volcengine ASR'),
    },
    models: { ...DEFAULT_MODELS, ...(options.models || {}) },
    config: {
      providerTimeoutMs: options.providerTimeoutMs ?? 120000,
      processTimeoutMs: options.processTimeoutMs ?? 120000,
      renderTimeoutMs: options.renderTimeoutMs ?? 600000,
      pollIntervalMs: options.pollIntervalMs ?? 5000,
      pollTimeoutMs: options.pollTimeoutMs ?? 30 * 60 * 1000,
      now: options.now,
      sleep: options.sleep,
    },
    async dispatch(toolName, args = {}) {
      const allowed = TOOL_ARGUMENTS[toolName];
      if (!allowed) throw new Error('unknown Aurora tool: ' + toolName);
      if (args === null || typeof args !== 'object' || Array.isArray(args)) throw new Error('tool arguments must be an object');
      for (const key of Object.keys(args)) {
        if (!allowed.includes(key)) throw new Error('tool argument is not permitted: ' + key);
      }
      return TOOL_HANDLERS[toolName](broker, args);
    },
    async call(toolName, args = {}) {
      try {
        const result = await broker.dispatch(toolName, args);
        return { content: [{ type: 'text', text: JSON.stringify(sanitizeValue(result)) }] };
      } catch (error) {
        return { isError: true, content: [{ type: 'text', text: sanitizeError(error).message }] };
      }
    },
  };
  return broker;
}

const TOOL_SCHEMAS = {
  'aurora.seedream_generate': { prompt: z.string().optional(), attachment_ids: z.array(z.string()).optional(), output_name: z.string().optional() },
  'aurora.seedance_generate': { prompt: z.string().optional(), attachment_ids: z.array(z.string()).optional(), output_name: z.string().optional() },
  'aurora.openai_image': { prompt: z.string().optional(), attachment_ids: z.array(z.string()).optional(), output_name: z.string().optional() },
  'aurora.volc_asr_transcribe': { attachment_id: z.string(), output_name: z.string().optional() },
  'aurora.read_document': { attachment_id: z.string() },
  'aurora.id_photo': { attachment_id: z.string(), output_name: z.string().optional() },
  'aurora.render_video_captions': {
    attachment_id: z.string(),
    cues: z.array(z.object({ start: z.number(), end: z.number(), text: z.string() })),
    output_name: z.string().optional(),
  },
  'aurora.render_resume': { sections: z.record(z.string(), z.any()), output_name: z.string().optional() },
  'aurora.write_text_artifact': { content: z.string(), name: z.string() },
};

export function registerTools(server, broker) {
  for (const name of TOOL_NAMES) {
    server.registerTool(name, { description: 'Aurora sandbox media tool ' + name, inputSchema: TOOL_SCHEMAS[name] }, (args) => broker.call(name, args));
  }
  return server;
}

export async function startServer(options = {}) {
  const broker = createBroker(options);
  const server = new McpServer({ name: 'aurora-sandbox-broker', version: '1.0.0' });
  registerTools(server, broker);
  const transport = new StdioServerTransport();
  await server.connect(transport);
  return { server, broker };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const serverOrigin = process.env.AURORA_SERVER_ORIGIN;
  const contextPath = process.env.AURORA_TASK_CONTEXT_FILE;
  if (!serverOrigin || !contextPath) {
    process.stderr.write('AURORA_SERVER_ORIGIN and AURORA_TASK_CONTEXT_FILE are required\n');
    process.exit(2);
  }
  startServer({
    contextPath,
    inputRoot: process.env.AURORA_INPUT_ROOT || DEFAULT_INPUT_ROOT,
    outputRoot: process.env.AURORA_OUTPUT_ROOT || DEFAULT_OUTPUT_ROOT,
    serverOrigin,
    importerPath: process.env.AURORA_ARTIFACT_IMPORT_PATH,
    secretPaths: {
      ark: process.env.ARK_API_KEY_FILE || DEFAULT_SECRET_PATHS.ark,
      openai: process.env.OPENAI_API_KEY_FILE || DEFAULT_SECRET_PATHS.openai,
      volcAsr: process.env.VOLC_ASR_API_KEY_FILE || DEFAULT_SECRET_PATHS.volcAsr,
      taskToken: process.env.AURORA_TASK_TOKEN_FILE || DEFAULT_SECRET_PATHS.taskToken,
    },
  }).catch((error) => {
    process.stderr.write(sanitizeError(error).message + '\n');
    process.exit(1);
  });
}
