// Deterministic document reader: bounded UTF-8/Markdown, pdftotext for PDF,
// and a fixed DOCX ZIP/XML extraction. No external library author is trusted.

import fs from 'node:fs';
import zlib from 'node:zlib';
import { LIMITS, assertToolAllowed } from '../policy.mjs';

const EOCD_SIGNATURE = 0x06054b50;
const CENTRAL_SIGNATURE = 0x02014b50;
const LOCAL_SIGNATURE = 0x04034b50;

function findEndOfCentralDirectory(buffer) {
  const minimum = Math.max(0, buffer.length - 65557);
  for (let index = buffer.length - 22; index >= minimum; index -= 1) {
    if (buffer.readUInt32LE(index) === EOCD_SIGNATURE) return index;
  }
  return -1;
}

export function extractZipEntry(buffer, wantedName) {
  const eocd = findEndOfCentralDirectory(buffer);
  if (eocd < 0) throw new Error('document archive is not a valid ZIP');
  const count = buffer.readUInt16LE(eocd + 10);
  let cursor = buffer.readUInt32LE(eocd + 16);
  for (let index = 0; index < count; index += 1) {
    if (cursor + 46 > buffer.length || buffer.readUInt32LE(cursor) !== CENTRAL_SIGNATURE) {
      throw new Error('document archive central directory is malformed');
    }
    const method = buffer.readUInt16LE(cursor + 10);
    const compressedSize = buffer.readUInt32LE(cursor + 20);
    const nameLength = buffer.readUInt16LE(cursor + 28);
    const extraLength = buffer.readUInt16LE(cursor + 30);
    const commentLength = buffer.readUInt16LE(cursor + 32);
    const localOffset = buffer.readUInt32LE(cursor + 42);
    const name = buffer.slice(cursor + 46, cursor + 46 + nameLength).toString('utf8');
    if (name === wantedName) {
      if (localOffset + 30 > buffer.length || buffer.readUInt32LE(localOffset) !== LOCAL_SIGNATURE) {
        throw new Error('document archive local header is malformed');
      }
      const localNameLength = buffer.readUInt16LE(localOffset + 26);
      const localExtraLength = buffer.readUInt16LE(localOffset + 28);
      const dataStart = localOffset + 30 + localNameLength + localExtraLength;
      const data = buffer.slice(dataStart, dataStart + compressedSize);
      if (method === 0) return data;
      if (method === 8) return zlib.inflateRawSync(data);
      throw new Error('document archive uses an unsupported compression method');
    }
    cursor += 46 + nameLength + extraLength + commentLength;
  }
  return null;
}

function decodeXmlEntities(value) {
  return value
    .replace(/&#x([0-9a-f]+);/gi, (_match, hex) => String.fromCodePoint(parseInt(hex, 16)))
    .replace(/&#([0-9]+);/g, (_match, digits) => String.fromCodePoint(parseInt(digits, 10)))
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, '&');
}

export function extractDocxText(buffer) {
  const document = extractZipEntry(buffer, 'word/document.xml');
  if (!document) throw new Error('DOCX is missing word/document.xml');
  const xml = document.toString('utf8');
  const normalized = xml
    .replace(/<w:tab\b[^>]*\/>/g, '\t')
    .replace(/<w:br\b[^>]*\/>/g, '\n')
    .replace(/<\/w:p>/g, '\n')
    .replace(/<[^>]+>/g, '');
  return decodeXmlEntities(normalized).replace(/\n{3,}/g, '\n\n').trim();
}

function extensionOf(relativePath) {
  return relativePath.includes('.') ? relativePath.slice(relativePath.lastIndexOf('.') + 1).toLowerCase() : '';
}

export async function readDocument(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.read_document');
  const attachment = broker.context.requireAttachment(args.attachment_id, ['document']);
  const extension = extensionOf(attachment.relativePath);
  let text;
  if (['txt', 'md', 'markdown'].includes(extension)) {
    text = fs.readFileSync(attachment.absolutePath, 'utf8').replace(/^\uFEFF/, '');
  } else if (extension === 'pdf') {
    const result = await broker.processRunner.run({
      command: 'pdftotext',
      args: [attachment.absolutePath, '-'],
      timeoutMs: broker.config.processTimeoutMs,
    });
    text = result.stdout;
  } else if (extension === 'docx') {
    text = extractDocxText(fs.readFileSync(attachment.absolutePath));
  } else {
    throw new Error('unsupported document format');
  }
  const characters = [...text];
  const truncated = characters.length > LIMITS.maxDocumentChars;
  return {
    tool: 'aurora.read_document',
    attachment_id: attachment.id,
    text: truncated ? characters.slice(0, LIMITS.maxDocumentChars).join('') : text,
    truncated,
  };
}
