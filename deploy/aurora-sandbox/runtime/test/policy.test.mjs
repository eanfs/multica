// Fixed route policy contract tests.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  AVAILABLE_SKILLS,
  PROVIDER_ORIGINS,
  DEFAULT_MODELS,
  ALLOWED_MODELS,
  skillPolicy,
  allowedToolsForSkill,
  assertToolAllowed,
  classifyAttachment,
  assertArtifactName,
  isUuid,
  sanitizeValue,
  sanitizeError,
} from '../src/policy.mjs';

test('exactly the 13 available skills are executable', () => {
  assert.equal(AVAILABLE_SKILLS.length, 13);
  for (const skill of ['poster', 'xhs-image', 'product-image', 'text-image', 'image-edit', 'id-photo', 'image-video', 'text-video', 'video-captions', 'xhs-copy', 'resume', 'document-summary', 'transcription']) {
    assert.ok(AVAILABLE_SKILLS.includes(skill), skill);
    assert.ok(skillPolicy(skill), skill);
  }
  for (const unavailable of ['avatar-video', 'ppt', 'excel']) {
    assert.equal(AVAILABLE_SKILLS.includes(unavailable), false);
    assert.equal(skillPolicy(unavailable), null);
  }
});

test('each skill maps to exactly its fixed provider tool route', () => {
  assert.deepEqual(allowedToolsForSkill('poster'), ['aurora.seedream_generate']);
  assert.deepEqual(allowedToolsForSkill('xhs-image'), ['aurora.seedream_generate']);
  assert.deepEqual(allowedToolsForSkill('text-image'), ['aurora.seedream_generate']);
  assert.deepEqual(allowedToolsForSkill('product-image'), ['aurora.openai_image']);
  assert.deepEqual(allowedToolsForSkill('image-edit'), ['aurora.openai_image']);
  assert.deepEqual(allowedToolsForSkill('id-photo'), ['aurora.id_photo']);
  assert.deepEqual(allowedToolsForSkill('image-video'), ['aurora.seedance_generate']);
  assert.deepEqual(allowedToolsForSkill('text-video'), ['aurora.seedance_generate']);
  assert.deepEqual(allowedToolsForSkill('video-captions'), ['aurora.volc_asr_transcribe', 'aurora.render_video_captions']);
  assert.deepEqual(allowedToolsForSkill('resume'), ['aurora.render_resume', 'aurora.read_document']);
  assert.deepEqual(allowedToolsForSkill('transcription'), ['aurora.volc_asr_transcribe']);
  assert.ok(allowedToolsForSkill('document-summary').includes('aurora.write_text_artifact'));
  assert.ok(allowedToolsForSkill('xhs-copy').includes('aurora.write_text_artifact'));
});

test('provider choice is not substitutable', () => {
  assert.throws(() => assertToolAllowed('poster', 'aurora.openai_image'), /not allowed/);
  assert.throws(() => assertToolAllowed('id-photo', 'aurora.seedream_generate'), /not allowed/);
  assert.throws(() => assertToolAllowed('not-a-skill', 'aurora.seedream_generate'), /not allowed/);
  assert.equal(assertToolAllowed('poster', 'aurora.seedream_generate'), true);
});

test('provider origins, endpoints, and models are compiled fixed values', () => {
  assert.equal(PROVIDER_ORIGINS.ark, 'https://ark.cn-beijing.volces.com');
  assert.equal(PROVIDER_ORIGINS.volcAsr, 'https://openspeech.bytedance.com');
  assert.equal(PROVIDER_ORIGINS.openai, 'https://api.openai.com');
  assert.deepEqual(ALLOWED_MODELS.seedream, ['doubao-seedream-5.0-lite', 'doubao-seedream-5.0-pro']);
  assert.ok(ALLOWED_MODELS.seedance.includes('doubao-seedance-2.0'));
  assert.ok(!ALLOWED_MODELS.seedance.some((m) => /1\.5/.test(m)));
  assert.equal(ALLOWED_MODELS.openai.includes('gpt-image-2.5-sunburst'), true);
  assert.ok(ALLOWED_MODELS.seedream.includes(DEFAULT_MODELS.seedream));
  assert.ok(ALLOWED_MODELS.seedance.includes(DEFAULT_MODELS.seedance));
  assert.ok(ALLOWED_MODELS.openai.includes(DEFAULT_MODELS.openai));
});

test('attachment kinds are derived from extension and MIME only', () => {
  assert.equal(classifyAttachment({ name: 'a.png', mimeType: 'image/png' }), 'image');
  assert.equal(classifyAttachment({ name: 'a.JPG', mimeType: 'image/jpeg' }), 'image');
  assert.equal(classifyAttachment({ name: 'a.pdf', mimeType: 'application/pdf' }), 'document');
  assert.equal(classifyAttachment({ name: 'a.docx', mimeType: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' }), 'document');
  assert.equal(classifyAttachment({ name: 'a.md', mimeType: 'text/markdown' }), 'document');
  assert.equal(classifyAttachment({ name: 'a.mp3', mimeType: 'audio/mpeg' }), 'audio');
  assert.equal(classifyAttachment({ name: 'a.wav', mimeType: 'audio/wav' }), 'audio');
  assert.equal(classifyAttachment({ name: 'a.ogg', mimeType: 'audio/ogg' }), 'audio');
  assert.equal(classifyAttachment({ name: 'a.mp4', mimeType: 'video/mp4' }), 'video');
  assert.equal(classifyAttachment({ name: 'a.mov', mimeType: 'video/quicktime' }), 'video');
  assert.equal(classifyAttachment({ name: 'a.exe', mimeType: 'application/octet-stream' }), null);
  assert.equal(classifyAttachment({ name: 'a.png', mimeType: 'text/plain' }), null);
});

test('artifact names are controlled and reject traversal or absolute paths', () => {
  assert.equal(assertArtifactName('primary-1.png'), 'primary-1.png');
  for (const bad of ['../escape.png', '/etc/passwd', 'a/b.png', 'a\\b.png', '..', '', 'a\u0000b.png', '.hidden']) {
    assert.throws(() => assertArtifactName(bad));
  }
});

test('sanitization removes credentials, data URIs, and signed URL secrets', () => {
  const value = sanitizeValue({
    authorization: 'Bearer ark-super-secret-value',
    note: 'key ark-abcdefgh data:image/png;base64,AAAA and https://x/y?X-Amz-Signature=deadbeef&a=1',
  });
  assert.equal(value.authorization, '[redacted]');
  assert.doesNotMatch(value.note, /ark-abcdefgh/);
  assert.doesNotMatch(value.note, /data:image/);
  assert.doesNotMatch(value.note, /deadbeef/);
  const error = sanitizeError(new Error('failed with ark-abcdefgh and data:image/png;base64,AAAA'));
  assert.doesNotMatch(error.message, /ark-abcdefgh/);
  assert.doesNotMatch(error.message, /data:image/);
});

test('uuid validation matches the server contract', () => {
  assert.equal(isUuid('00000000-0000-4000-8000-000000000001'), true);
  assert.equal(isUuid('not-a-uuid'), false);
  assert.equal(isUuid(123), false);
});
