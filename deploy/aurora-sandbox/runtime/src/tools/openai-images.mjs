// Hardened OpenAI Images tool.

import fs from 'node:fs';
import OpenAI from 'openai';
import { LIMITS, OPENAI_BASE_URL, PROVIDER_RUN_OPERATIONS, assertToolAllowed, sanitizeError } from '../policy.mjs';
import { attachmentList, outputPathFor, randomArtifactName, resolvePrompt, secretValue } from './common.mjs';

function chooseMode(skillId, attachments) {
  if (skillId === 'image-edit') {
    if (attachments.length === 0) throw new Error('image-edit requires at least one image');
    return 'edit';
  }
  if (skillId === 'product-image') {
    return attachments.length > 0 ? 'edit' : 'generate';
  }
  throw new Error('OpenAI Images is not the configured route for skill ' + skillId);
}

async function persistImages(broker, response, requestedName) {
  const data = Array.isArray(response && response.data) ? response.data : [];
  if (data.length === 0) throw new Error('OpenAI returned no image');
  const artifacts = [];
  for (let index = 0; index < data.length; index += 1) {
    const item = data[index];
    if (!item || typeof item.b64_json !== 'string' || item.b64_json.length === 0) {
      throw new Error('OpenAI returned no base64 image data');
    }
    const id = index === 0 ? 'primary-1' : `image-${index + 1}`;
    const role = index === 0 ? 'primary' : 'supporting';
    const name = randomArtifactName(requestedName, index, '.png');
    const bytes = Buffer.from(item.b64_json, 'base64');
    if (bytes.length > LIMITS.maxImageBytes) throw new Error('OpenAI image exceeds the 25 MiB artifact cap');
    const target = outputPathFor(broker, name);
    fs.writeFileSync(target, bytes, { mode: 0o600 });
    broker.manifest.addFile({ id, path: target, name, kind: 'image', role, format: 'png', mimeType: 'image/png' });
    artifacts.push({ id, kind: 'image', role, name });
  }
  return artifacts;
}

export async function openaiImage(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.openai_image');
  const prompt = resolvePrompt(broker, args);
  const model = broker.models.openai;
  const attachments = attachmentList(broker, args.attachment_ids, ['image']);
  const mode = chooseMode(broker.context.skillId, attachments);
  const operation = mode === 'edit' ? PROVIDER_RUN_OPERATIONS.openaiEdit : PROVIDER_RUN_OPERATIONS.openaiGenerate;

  const begun = await broker.providerRun.begin({ provider: 'openai', operation, model, args: { prompt, referenceCount: attachments.length } });
  if (!begun.create_allowed) throw new Error('OpenAI provider run create lease was not granted');

  try {
    const client = new OpenAI({
      apiKey: secretValue(broker, 'openai'),
      baseURL: OPENAI_BASE_URL,
      fetch: broker.transport.providerFetch,
      maxRetries: 0,
      timeout: broker.config.providerTimeoutMs,
    });
    let response;
    if (mode === 'generate') {
      response = await client.images.generate({ model, prompt, response_format: 'b64_json', n: 1 });
    } else {
      response = await client.images.edit({
        model,
        prompt,
        response_format: 'b64_json',
        image: attachments.map((attachment) => fs.createReadStream(attachment.absolutePath)),
      });
    }
    const artifacts = await persistImages(broker, response, args.output_name);
    broker.manifest.setProviderRun({ provider: 'openai', model, external_id: response.id ?? null });
    await broker.providerRun.finish(operation, 'succeeded');
    broker.manifest.write();
    return { tool: 'aurora.openai_image', operation, model, mode, artifacts };
  } catch (error) {
    await broker.providerRun.finish(operation, 'failed', 'provider_failed');
    throw sanitizeError(error);
  }
}
