// UTF-8 Markdown/text artifact writer with controlled names.

import fs from 'node:fs';
import { LIMITS, assertArtifactName, assertToolAllowed } from '../policy.mjs';
import { outputPathFor } from './common.mjs';

const ALLOWED_EXTENSIONS = Object.freeze({
  '.md': { format: 'md', mimeType: 'text/markdown' },
  '.markdown': { format: 'md', mimeType: 'text/markdown' },
  '.txt': { format: 'txt', mimeType: 'text/plain' },
});

export async function writeTextArtifact(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.write_text_artifact');
  if (typeof args.content !== 'string' || args.content.length === 0) throw new Error('artifact content is required');
  if (Buffer.byteLength(args.content, 'utf8') > LIMITS.maxTextArtifactBytes) {
    throw new Error('text artifact exceeds the 2 MiB limit');
  }
  const name = assertArtifactName(args.name);
  const extension = name.includes('.') ? name.slice(name.lastIndexOf('.')).toLowerCase() : '';
  const descriptor = ALLOWED_EXTENSIONS[extension];
  if (!descriptor) throw new Error('only Markdown and text artifact names are permitted');
  const target = outputPathFor(broker, name);
  fs.writeFileSync(target, args.content, { mode: 0o600 });
  fs.chmodSync(target, 0o600);
  broker.manifest.addFile({ id: 'primary-1', path: target, name, kind: 'text', role: 'primary', format: descriptor.format, mimeType: descriptor.mimeType });
  broker.manifest.write();
  return { tool: 'aurora.write_text_artifact', artifacts: [{ id: 'primary-1', kind: 'text', role: 'primary', name }] };
}
