// Bounded, runner-provided task context and compiled secret access.

import fs from 'node:fs';
import path from 'node:path';
import {
  LIMITS,
  assertArtifactName,
  classifyAttachment,
  isUuid,
  kindLimit,
  skillPolicy,
} from './policy.mjs';

export const TASK_CONTEXT_SCHEMA = 'com.multica.aurora.task-context';
export const TASK_CONTEXT_VERSION = 1;
export const MAX_CONTEXT_BYTES = LIMITS.maxContextBytes;

export const DEFAULT_INPUT_ROOT = '/workspace/input';
export const DEFAULT_OUTPUT_ROOT = '/workspace/output';

export const DEFAULT_SECRET_PATHS = Object.freeze({
  ark: '/run/secrets/ark-api-key',
  openai: '/run/secrets/openai-api-key',
  volcAsr: '/run/secrets/volc-asr-api-key',
  taskToken: '/run/secrets/task-token',
});

export class TaskContextError extends Error {}

const REQUIRED_FIELDS = ['schema', 'version', 'task_id', 'generation_id', 'workspace_id', 'skill_id', 'prompt', 'attachments', 'output_root', 'server_origin', 'task_token_file'];
const ATTACHMENT_FIELDS = ['relative_path', 'mime_type', 'size_bytes'];

function fail(message) {
  throw new TaskContextError(message);
}

function readContextFile(contextPath) {
  let stats;
  try {
    stats = fs.lstatSync(contextPath);
  } catch {
    fail('task context file is missing');
  }
  if (stats.isSymbolicLink()) fail('task context file must not be a symlink');
  if (!stats.isFile()) fail('task context file must be a regular file');
  if ((stats.mode & 0o077) !== 0) fail('task context file permissions must be owner-only (mode 0400)');
  if (stats.size > MAX_CONTEXT_BYTES) fail('task context file exceeds 1 MiB');
  return fs.readFileSync(contextPath, 'utf8');
}

export function loadTaskContext({ contextPath, inputRoot = DEFAULT_INPUT_ROOT, outputRoot = DEFAULT_OUTPUT_ROOT, serverOrigin, allowedSecretPaths }) {
  if (typeof contextPath !== 'string' || contextPath.length === 0) fail('task context path is required');
  const raw = readContextFile(contextPath);
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    fail('task context is not valid JSON');
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) fail('task context must be a JSON object');

  for (const key of Object.keys(parsed)) {
    if (!REQUIRED_FIELDS.includes(key)) fail('unknown task context field: ' + key);
  }
  for (const key of REQUIRED_FIELDS) {
    if (!(key in parsed)) fail('missing task context field: ' + key);
  }
  if (parsed.schema !== TASK_CONTEXT_SCHEMA) fail('unexpected task context schema');
  if (parsed.version !== TASK_CONTEXT_VERSION) fail('unsupported task context version');
  for (const key of ['task_id', 'generation_id', 'workspace_id']) {
    if (!isUuid(parsed[key])) fail('invalid task context ' + key);
  }
  if (!isUuid(parsed.task_id) || !isUuid(parsed.generation_id) || !isUuid(parsed.workspace_id)) fail('invalid task context identity');
  const policy = skillPolicy(parsed.skill_id);
  if (!policy) fail('skill ' + String(parsed.skill_id) + ' is not executable');
  if (parsed.output_root !== outputRoot) fail('task context output root does not match the fixed policy');
  if (parsed.server_origin !== serverOrigin) fail('task context server origin does not match the fixed policy');
  if (typeof parsed.task_token_file !== 'string' || parsed.task_token_file.length === 0) fail('task context token file is required');
  if (allowedSecretPaths && !allowedSecretPaths.includes(parsed.task_token_file)) fail('task context token file is not a compiled secret path');

  if (typeof parsed.prompt !== 'string') fail('task context prompt is required');
  if (parsed.prompt.length < policy.prompt[0] || parsed.prompt.length > policy.prompt[1]) fail('prompt length does not match skill ' + parsed.skill_id);

  if (parsed.attachments === null || typeof parsed.attachments !== 'object' || Array.isArray(parsed.attachments)) {
    fail('task context attachments must be an object');
  }
  const attachments = new Map();
  for (const [id, entry] of Object.entries(parsed.attachments)) {
    if (!isUuid(id)) fail('attachment id is not a UUID');
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) fail('attachment entry must be an object');
    for (const key of Object.keys(entry)) {
      if (!ATTACHMENT_FIELDS.includes(key)) fail('unknown attachment field: ' + key);
    }
    for (const key of ATTACHMENT_FIELDS) {
      if (!(key in entry)) fail('missing attachment field: ' + key);
    }
    if (typeof entry.relative_path !== 'string' || entry.relative_path.length === 0) fail('attachment relative path is required');
    if (entry.relative_path.includes('\u0000')) fail('attachment relative path is invalid');
    if (path.isAbsolute(entry.relative_path) || entry.relative_path.split(/[\\/]/).includes('..')) {
      fail('attachment path escapes the authorized input root');
    }
    if (entry.relative_path.includes(':') || /^[a-zA-Z]:/.test(entry.relative_path)) fail('attachment path escapes the authorized input root');
    const absolutePath = path.resolve(inputRoot, entry.relative_path);
    if (absolutePath !== inputRoot && !absolutePath.startsWith(inputRoot + path.sep)) {
      fail('attachment path escapes the authorized input root');
    }
    const kind = classifyAttachment({ name: entry.relative_path, mimeType: entry.mime_type });
    if (!kind) fail('unsupported attachment kind for ' + entry.relative_path);
    if (!Number.isInteger(entry.size_bytes) || entry.size_bytes < 0) fail('attachment size is invalid');
    if (entry.size_bytes > kindLimit(kind)) fail('attachment exceeds the per-file size cap');
    attachments.set(id, {
      id,
      kind,
      relativePath: entry.relative_path,
      absolutePath,
      mimeType: entry.mime_type,
      sizeBytes: entry.size_bytes,
    });
  }

  const list = [...attachments.values()];
  if (list.length < policy.attachments.min || list.length > policy.attachments.max) {
    fail(`attachment count does not match skill ${parsed.skill_id}`);
  }
  for (const attachment of list) {
    if (!policy.attachments.kinds.includes(attachment.kind)) {
      fail(`attachment kind ${attachment.kind} is not accepted by skill ${parsed.skill_id}`);
    }
  }

  return {
    schema: parsed.schema,
    version: parsed.version,
    taskId: parsed.task_id,
    generationId: parsed.generation_id,
    workspaceId: parsed.workspace_id,
    skillId: parsed.skill_id,
    prompt: parsed.prompt,
    outputRoot,
    inputRoot,
    serverOrigin,
    taskTokenFile: parsed.task_token_file,
    attachments,
    resolveAttachment(id) {
      const attachment = attachments.get(id);
      if (!attachment) fail('attachment id is not authorized for this task');
      let stats;
      try {
        stats = fs.lstatSync(attachment.absolutePath);
      } catch {
        fail('authorized attachment is missing');
      }
      if (stats.isSymbolicLink() || !stats.isFile()) fail('authorized attachment must be a regular file');
      return attachment;
    },
    requireAttachment(id, kinds) {
      if (typeof id !== 'string' || id.length === 0) fail('attachment id is required');
      const attachment = attachments.get(id);
      if (!attachment) fail('attachment id is not authorized for this task');
      let stats;
      try {
        stats = fs.lstatSync(attachment.absolutePath);
      } catch {
        fail('authorized attachment is missing');
      }
      if (stats.isSymbolicLink() || !stats.isFile()) fail('authorized attachment must be a regular file');
      if (kinds && kinds.length > 0 && !kinds.includes(attachment.kind)) {
        fail(`attachment is not one of: ${kinds.join(', ')}`);
      }
      return attachment;
    },
  };
}

export function readSecret(filePath, { allowedPaths, maxBytes = LIMITS.maxSecretBytes } = {}) {
  if (typeof filePath !== 'string' || !Array.isArray(allowedPaths) || !allowedPaths.includes(filePath)) {
    throw new TaskContextError('secret path is not a compiled secret path');
  }
  let stats;
  try {
    stats = fs.lstatSync(filePath);
  } catch {
    throw new TaskContextError('compiled secret file is unavailable');
  }
  if (stats.isSymbolicLink()) throw new TaskContextError('compiled secret file must not be a symlink');
  if (!stats.isFile()) throw new TaskContextError('compiled secret file must be a regular file');
  if (stats.size > maxBytes) throw new TaskContextError('compiled secret exceeds the 4 KiB cap');
  return fs.readFileSync(filePath, 'utf8').trim();
}

export { assertArtifactName };
