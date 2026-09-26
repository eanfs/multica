// Shared helpers for the narrow Aurora tools.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { LIMITS, assertArtifactName, skillPolicy } from '../policy.mjs';

export function ensureOutputDirectory(broker) {
  const dir = path.join(broker.context.outputRoot, 'artifacts');
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  return dir;
}

export function outputPathFor(broker, name) {
  assertArtifactName(name);
  const dir = ensureOutputDirectory(broker);
  const target = path.resolve(dir, name);
  if (target !== dir && !target.startsWith(dir + path.sep)) {
    throw new Error('artifact path escapes the output root');
  }
  return target;
}

export function writePrivateFile(file, data) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  fs.writeFileSync(file, data, { mode: 0o600 });
  fs.chmodSync(file, 0o600);
  return file;
}

export function randomArtifactName(requested, index, extension) {
  const base = index === 0 ? (requested || 'primary-1') : `image-${index + 1}`;
  const stem = base.replace(/\.[A-Za-z0-9]+$/, '');
  return assertArtifactName(stem || 'primary-1') + extension;
}

export function resolvePrompt(broker, args) {
  const policy = skillPolicy(broker.context.skillId);
  const prompt = args.prompt === undefined ? broker.context.prompt : args.prompt;
  if (typeof prompt !== 'string' || prompt.trim().length === 0) throw new Error('prompt is required');
  if (!policy || prompt.length > policy.prompt[1]) throw new Error('prompt exceeds the character cap');
  return prompt;
}

export function fileToDataUri(attachment) {
  if (!['image/png', 'image/jpeg'].includes(attachment.mimeType)) {
    throw new Error('only PNG and JPEG reference images are accepted');
  }
  const bytes = fs.readFileSync(attachment.absolutePath);
  if (bytes.length > LIMITS.maxImageBytes) throw new Error('reference image exceeds the size cap');
  return 'data:' + attachment.mimeType + ';base64,' + bytes.toString('base64');
}

export function attachmentList(broker, ids, kinds) {
  if (ids === undefined) return [];
  if (!Array.isArray(ids)) throw new Error('attachment ids must be an array');
  if (ids.length > 30) throw new Error('too many attachments');
  return ids.map((id) => broker.context.requireAttachment(id, kinds));
}

export function secretValue(broker, kind) {
  const reader = broker.secrets && broker.secrets[kind];
  if (typeof reader !== 'function') throw new Error('compiled secret accessor is unavailable');
  return reader();
}

export function newId() {
  return crypto.randomUUID();
}

export function safeUnlink(file) {
  try {
    fs.rmSync(file, { force: true });
  } catch {
    /* best effort */
  }
}
