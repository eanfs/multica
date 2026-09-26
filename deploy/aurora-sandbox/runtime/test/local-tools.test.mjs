// Deterministic local tool contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createBroker } from '../src/server.mjs';
import { buildResumeHtml } from '../src/tools/resume.mjs';
import { makeWorkspace, baseContext, writeContextFile, writeInput, attachmentEntry, uuid, makeDocx, pngBytes, readManifest } from './helpers.mjs';

const DOC_ID = uuid(9);
const IMAGE_ID = uuid(8);
const VIDEO_ID = uuid(7);

function makeBroker(ws, { skillId, attachments, processRunner = { calls: [], async run() { throw new Error('unexpected process'); } } }) {
  const contextPath = writeContextFile(ws, baseContext(ws, { skill_id: skillId, attachments }));
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
    fetchImpl: async () => { throw new Error('local tools must not fetch'); },
    providerRun: { async begin() { throw new Error('local tools must not create provider runs'); } },
    processRunner,
    vendor: {},
    importer: async () => { throw new Error('local tools must not import'); },
  });
  return broker;
}

test('read_document returns bounded UTF-8 text and Markdown', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'notes.md', Buffer.from('# Title\n\nbody'));
  const broker = makeBroker(ws, { skillId: 'document-summary', attachments: { [DOC_ID]: attachmentEntry('notes.md', 'text/markdown', 14) } });
  const result = await broker.dispatch('aurora.read_document', { attachment_id: DOC_ID });
  assert.match(result.text, /# Title/);
});

test('read_document extracts a fixed DOCX ZIP/XML document', async () => {
  const ws = makeWorkspace();
  const docx = makeDocx(['Hello world', 'Second paragraph']);
  writeInput(ws, 'resume.docx', docx);
  const broker = makeBroker(ws, {
    skillId: 'document-summary',
    attachments: { [DOC_ID]: attachmentEntry('resume.docx', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document', docx.length) },
  });
  const result = await broker.dispatch('aurora.read_document', { attachment_id: DOC_ID });
  assert.match(result.text, /Hello world/);
  assert.match(result.text, /Second paragraph/);
});

test('read_document uses pdftotext for PDF and caps Unicode text', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'doc.pdf', Buffer.from('%PDF-1.4'));
  const processRunner = {
    calls: [],
    async run(input) {
      this.calls.push(input);
      return { stdout: 'p'.repeat(250000), stderr: '', code: 0 };
    },
  };
  const broker = makeBroker(ws, {
    skillId: 'document-summary',
    attachments: { [DOC_ID]: attachmentEntry('doc.pdf', 'application/pdf', 8) },
    processRunner,
  });
  const result = await broker.dispatch('aurora.read_document', { attachment_id: DOC_ID });
  assert.equal(processRunner.calls[0].command, 'pdftotext');
  assert.ok(Array.isArray(processRunner.calls[0].args));
  assert.ok([...result.text].length <= 200000);
});

test('id_photo runs a fixed ImageMagick pipeline and writes a manifest image', async () => {
  const ws = makeWorkspace();
  const bytes = pngBytes(40);
  writeInput(ws, 'portrait.png', bytes);
  const processRunner = {
    calls: [],
    async run(input) {
      this.calls.push(input);
      const output = input.args[input.args.length - 1];
      fs.writeFileSync(output, pngBytes(80));
      return { stdout: '', stderr: '', code: 0 };
    },
  };
  const broker = makeBroker(ws, {
    skillId: 'id-photo',
    attachments: { [IMAGE_ID]: attachmentEntry('portrait.png', 'image/png', bytes.length) },
    processRunner,
  });
  await broker.dispatch('aurora.id_photo', { attachment_id: IMAGE_ID, output_name: 'id.png' });
  const call = processRunner.calls[0];
  assert.equal(call.command, 'magick');
  for (const flag of ['-auto-orient', '-colorspace', '-resize', '-background', '-gravity', '-extent']) {
    assert.ok(call.args.includes(flag), 'missing ' + flag);
  }
  assert.equal(call.shell, undefined);
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].kind, 'image');
  assert.equal(manifest.artifacts[0].role, 'primary');
});

test('render_video_captions uses a fixed composition and never arbitrary source', async () => {
  const ws = makeWorkspace();
  writeInput(ws, 'clip.mp4', Buffer.alloc(32, 2));
  const processRunner = {
    calls: [],
    async run(input) {
      this.calls.push(input);
      const outputIndex = input.args.indexOf('-o');
      const output = input.args[outputIndex + 1];
      fs.writeFileSync(output, Buffer.from('mp4'));
      return { stdout: '', stderr: '', code: 0 };
    },
  };
  const broker = makeBroker(ws, {
    skillId: 'video-captions',
    attachments: { [VIDEO_ID]: attachmentEntry('clip.mp4', 'video/mp4', 32) },
    processRunner,
  });
  const result = await broker.dispatch('aurora.render_video_captions', {
    attachment_id: VIDEO_ID,
    cues: [{ start: 0, end: 1.5, text: '</script><script>alert(1)</script>' }],
  });
  const call = processRunner.calls[0];
  assert.equal(call.command, 'hyperframes');
  assert.ok(call.args.includes('render'));
  assert.ok(call.args.includes('-c'));
  const composition = call.args[call.args.indexOf('-c') + 1];
  const html = fs.readFileSync(composition, 'utf8');
  assert.doesNotMatch(html, /<script>alert/);
  assert.equal(call.shell, undefined);
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].kind, 'video');
  assert.doesNotMatch(JSON.stringify(result), /alert\(1\)/);
});

test('render_resume escapes structured sections and prints PDF with network disabled', async () => {
  const ws = makeWorkspace();
  const processRunner = {
    calls: [],
    async run(input) {
      this.calls.push(input);
      const marker = input.args.find((arg) => arg.startsWith('--print-to-pdf='));
      fs.writeFileSync(marker.slice('--print-to-pdf='.length), Buffer.from('%PDF-1.4'));
      return { stdout: '', stderr: '', code: 0 };
    },
  };
  const broker = makeBroker(ws, { skillId: 'resume', attachments: {}, processRunner });
  const sections = {
    name: 'Ada <script>alert(1)</script>',
    title: 'Engineer',
    contact: ['ada@example.com'],
    summary: 'Builds',
    experience: [{ company: 'A & B', role: 'Dev', dates: '2020', bullets: ['did <b>things</b>'] }],
    education: [{ school: 'U', degree: 'BS', dates: '2019' }],
    skills: ['js'],
  };
  const html = buildResumeHtml(sections);
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /&lt;script&gt;/);
  await broker.dispatch('aurora.render_resume', { sections, output_name: 'resume' });
  const chromium = processRunner.calls.find((entry) => /chromium/.test(entry.command));
  assert.ok(chromium);
  assert.ok(chromium.args.some((arg) => arg.includes('host-resolver-rules')));
  assert.ok(chromium.args.some((arg) => arg.includes('disable-background-networking')));
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts.length, 2);
  assert.deepEqual(manifest.artifacts.map((a) => a.kind).sort(), ['pdf', 'text']);
});

test('write_text_artifact is UTF-8 Markdown/text only and bounded', async () => {
  const ws = makeWorkspace();
  const broker = makeBroker(ws, { skillId: 'xhs-copy', attachments: {} });
  const result = await broker.dispatch('aurora.write_text_artifact', { content: '# Summary', name: 'summary.md' });
  assert.match(result.artifacts[0].name, /summary\.md/);
  const manifest = readManifest(ws.outputRoot);
  assert.equal(manifest.artifacts[0].kind, 'text');
  assert.equal(manifest.artifacts[0].mime_type, 'text/markdown');

  await assert.rejects(broker.dispatch('aurora.write_text_artifact', { content: 'x', name: '../evil.md' }), /name|path/);
  await assert.rejects(broker.dispatch('aurora.write_text_artifact', { content: 'x'.repeat(2 * 1024 * 1024 + 1), name: 'big.md' }), /size|2 MiB|limit/);
  await assert.rejects(broker.dispatch('aurora.write_text_artifact', { content: 'x', name: 'evil.html' }), /markdown|text|format|name/);
});

test('a tool outside the skill route is refused', async () => {
  const ws = makeWorkspace();
  const bytes = pngBytes(10);
  writeInput(ws, 'a.png', bytes);
  const broker = makeBroker(ws, {
    skillId: 'poster',
    attachments: { [IMAGE_ID]: attachmentEntry('a.png', 'image/png', bytes.length) },
  });
  await assert.rejects(broker.dispatch('aurora.id_photo', { attachment_id: IMAGE_ID }), /not allowed/);
});
