// Hardened Seedance tool.
//
// The Seedance vendor tree is blocked by the licence gate (issue #140), so this
// adapter is written against the patched-module interface and fails closed when
// the module is absent. It never vendors or fabricates the tree.

import { ALLOWED_MODELS, PROVIDER_RUN_OPERATIONS, assertToolAllowed, sanitizeError } from '../policy.mjs';
import { assertProviderResultUrl } from '../importer.mjs';
import { attachmentList, fileToDataUri, randomArtifactName, resolvePrompt, secretValue, outputPathFor } from './common.mjs';
import fs from 'node:fs';

export function loadSeedanceModule(broker) {
  const vendor = broker.vendor && broker.vendor.seedance;
  if (!vendor || typeof vendor.createTask !== 'function' || typeof vendor.pollTask !== 'function') {
    throw new Error('Seedance vendor adapter is unavailable: the vendored Seedance tree is blocked by the licence gate and is not present');
  }
  return vendor;
}

function assertModel(model) {
  if (!ALLOWED_MODELS.seedance.includes(model)) throw new Error('Seedance model is not on the compiled allowlist');
  return model;
}

async function importOutputs(broker, descriptor, requestedName) {
  const outputs = Array.isArray(descriptor.outputs) ? descriptor.outputs : [];
  if (outputs.length === 0) throw new Error('Seedance returned no output');
  const artifacts = [];
  for (let index = 0; index < outputs.length; index += 1) {
    const output = outputs[index];
    const kind = output.kind === 'image' ? 'image' : 'video';
    const extension = kind === 'image' ? '.png' : '.mp4';
    const mimeType = kind === 'image' ? 'image/png' : 'video/mp4';
    const id = index === 0 ? 'primary-1' : `video-${index + 1}`;
    const role = index === 0 ? 'primary' : 'supporting';
    const name = randomArtifactName(requestedName, index, extension);
    if (!output.url) throw new Error('Seedance output has no URL');
    assertProviderResultUrl(output.url);
    if (typeof broker.importer !== 'function') throw new Error('task-scoped artifact importer is not configured');
    const imported = await broker.importer({ url: output.url, kind, name, mimeType, sizeBytes: null });
    broker.manifest.addStagedObject({
      id,
      stagingId: imported.staging_id,
      name,
      kind,
      role,
      format: kind === 'image' ? 'png' : 'mp4',
      mimeType,
      sizeBytes: imported.size_bytes ?? 0,
      sha256: imported.sha256,
    });
    artifacts.push({ id, kind, role, name, staging_id: imported.staging_id });
  }
  return artifacts;
}

export async function seedanceGenerate(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.seedance_generate');
  const vendor = loadSeedanceModule(broker);
  const prompt = resolvePrompt(broker, args);
  const operation = PROVIDER_RUN_OPERATIONS.seedance;
  const model = assertModel(broker.models.seedance);
  const references = attachmentList(broker, args.attachment_ids, ['image']).map(fileToDataUri);
  const env = { ARK_API_KEY: secretValue(broker, 'ark') };

  let externalId = null;
  let existing = null;
  try {
    existing = await broker.providerRun.get(operation);
  } catch (error) {
    if (!error || error.status !== 404) throw sanitizeError(error);
  }

  try {
    if (existing) {
      if (existing.state === 'succeeded') throw new Error('Seedance provider run has already succeeded');
      if (!existing.external_id) throw new Error('Seedance provider run is ambiguous; refusing a second create');
      externalId = existing.external_id;
    } else {
      const begun = await broker.providerRun.begin({
        provider: 'volcengine-agentplan',
        operation,
        model,
        args: { prompt, referenceCount: references.length },
      });
      if (!begun.create_allowed) throw new Error('Seedance provider run create lease was not granted');
      const created = await vendor.createTask({
        env,
        fetchImpl: broker.transport.providerFetch,
        input: { model, prompt, imageUrls: references },
      });
      externalId = created && created.external_id;
      if (typeof externalId !== 'string' || externalId.length === 0) throw new Error('Seedance create returned no task id');
      await broker.providerRun.recordExternal(operation, externalId);
    }

    const descriptor = await vendor.pollTask({
      env,
      fetchImpl: broker.transport.providerFetch,
      taskId: externalId,
      pollIntervalMs: broker.config.pollIntervalMs,
      timeoutMs: broker.config.pollTimeoutMs,
      now: broker.config.now,
      sleep: broker.config.sleep,
    });
    const artifacts = await importOutputs(broker, descriptor, args.output_name);
    broker.manifest.setProviderRun({ provider: 'volcengine-agentplan', model, external_id: externalId });
    await broker.providerRun.finish(operation, 'succeeded');
    broker.manifest.write();
    return { tool: 'aurora.seedance_generate', operation, model, artifacts };
  } catch (error) {
    await broker.providerRun.finish(operation, 'failed', 'provider_failed');
    throw sanitizeError(error);
  }
}
