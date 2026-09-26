// Hardened Seedream tool. Invokes only the patched vendor module.

import fs from 'node:fs';
import { PROVIDER_RUN_OPERATIONS, assertToolAllowed, sanitizeError } from '../policy.mjs';
import { assertProviderResultUrl } from '../importer.mjs';
import { attachmentList, fileToDataUri, randomArtifactName, resolvePrompt, secretValue, outputPathFor } from './common.mjs';

export function loadSeedreamModule(broker) {
  const vendor = broker.vendor && broker.vendor.seedream;
  if (!vendor || typeof vendor.generate !== 'function') {
    throw new Error('Seedream vendor adapter is unavailable');
  }
  return vendor;
}

async function importOutputs(broker, descriptor, requestedName) {
  const images = Array.isArray(descriptor.images) ? descriptor.images : [];
  if (images.length === 0) throw new Error('Seedream returned no image');
  const artifacts = [];
  for (let index = 0; index < images.length; index += 1) {
    const image = images[index];
    const id = index === 0 ? 'primary-1' : `image-${index + 1}`;
    const role = index === 0 ? 'primary' : 'supporting';
    const name = randomArtifactName(requestedName, index, '.png');
    if (image.url) {
      assertProviderResultUrl(image.url);
      if (typeof broker.importer !== 'function') throw new Error('task-scoped artifact importer is not configured');
      const imported = await broker.importer({
        url: image.url,
        kind: 'image',
        name,
        mimeType: 'image/png',
        sizeBytes: null,
      });
      broker.manifest.addStagedObject({
        id,
        stagingId: imported.staging_id,
        name,
        kind: 'image',
        role,
        format: 'png',
        mimeType: 'image/png',
        sizeBytes: imported.size_bytes ?? 0,
        sha256: imported.sha256,
      });
      artifacts.push({ id, kind: 'image', role, name, staging_id: imported.staging_id });
    } else if (image.b64_json) {
      const target = outputPathFor(broker, name);
      fs.writeFileSync(target, Buffer.from(image.b64_json, 'base64'), { mode: 0o600 });
      broker.manifest.addFile({ id, path: target, name, kind: 'image', role, format: 'png', mimeType: 'image/png' });
      artifacts.push({ id, kind: 'image', role, name });
    } else {
      throw new Error('Seedream image has neither a URL nor base64 output');
    }
  }
  return artifacts;
}

export async function seedreamGenerate(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.seedream_generate');
  const vendor = loadSeedreamModule(broker);
  const prompt = resolvePrompt(broker, args);
  const operation = PROVIDER_RUN_OPERATIONS.seedream;
  const model = broker.models.seedream;
  const references = attachmentList(broker, args.attachment_ids, ['image']).map(fileToDataUri);

  const begun = await broker.providerRun.begin({
    provider: 'volcengine-agentplan',
    operation,
    model,
    args: { prompt, referenceCount: references.length },
  });
  if (!begun.create_allowed) throw new Error('Seedream provider run create lease was not granted');

  try {
    const descriptor = await vendor.generate({
      env: { ARK_API_KEY: secretValue(broker, 'ark') },
      fetchImpl: broker.transport.providerFetch,
      input: { model, prompt, referenceImages: references, outputFormat: 'png' },
    });
    const artifacts = await importOutputs(broker, descriptor, args.output_name);
    broker.manifest.setProviderRun({ provider: 'volcengine-agentplan', model, external_id: descriptor.external_id ?? null });
    await broker.providerRun.finish(operation, 'succeeded');
    broker.manifest.write();
    return { tool: 'aurora.seedream_generate', operation, model, artifacts };
  } catch (error) {
    await broker.providerRun.finish(operation, 'failed', 'provider_failed');
    throw sanitizeError(error);
  }
}
