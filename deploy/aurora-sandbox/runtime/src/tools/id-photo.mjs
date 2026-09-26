// Deterministic ID photo transform. Fixed ImageMagick argv, no model call.

import { assertToolAllowed } from '../policy.mjs';
import { outputPathFor, randomArtifactName } from './common.mjs';

export const ID_PHOTO_ARGS = Object.freeze([
  '-auto-orient',
  '-colorspace',
  'sRGB',
  '-resize',
  '600x600^',
  '-background',
  'white',
  '-gravity',
  'center',
  '-extent',
  '600x600',
]);

export async function idPhoto(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.id_photo');
  const attachment = broker.context.requireAttachment(args.attachment_id, ['image']);
  const name = randomArtifactName(args.output_name, 0, '.png');
  const target = outputPathFor(broker, name);
  await broker.processRunner.run({
    command: 'magick',
    args: [...ID_PHOTO_ARGS.slice(0, 1), attachment.absolutePath, ...ID_PHOTO_ARGS.slice(1), target],
    timeoutMs: broker.config.processTimeoutMs,
  });
  broker.manifest.addFile({ id: 'primary-1', path: target, name, kind: 'image', role: 'primary', format: 'png', mimeType: 'image/png' });
  broker.manifest.write();
  return { tool: 'aurora.id_photo', artifacts: [{ id: 'primary-1', kind: 'image', role: 'primary', name }] };
}
