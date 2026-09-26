// Fixed escaped HTML resume template plus headless Chromium PDF print.

import path from 'node:path';
import { assertToolAllowed, sanitizeValue } from '../policy.mjs';
import { newId, outputPathFor, writePrivateFile } from './common.mjs';

const STRING_FIELDS = ['name', 'title', 'summary'];
const LIST_FIELDS = ['contact', 'skills', 'languages', 'certifications'];
const OBJECT_LIST_FIELDS = ['experience', 'education', 'projects'];

function escapeHtml(value) {
  return String(value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function assertSections(sections) {
  if (!sections || typeof sections !== 'object' || Array.isArray(sections)) {
    throw new Error('resume sections must be an object');
  }
  const allowed = new Set([...STRING_FIELDS, ...LIST_FIELDS, ...OBJECT_LIST_FIELDS]);
  for (const key of Object.keys(sections)) {
    if (!allowed.has(key)) throw new Error('unknown resume section: ' + key);
  }
  const safe = sanitizeValue(sections);
  if (typeof safe.name !== 'string' || safe.name.trim().length === 0) throw new Error('resume name is required');
  return safe;
}

export function buildResumeHtml(sections) {
  const safe = assertSections(sections);
  const contact = Array.isArray(safe.contact) ? safe.contact.map(escapeHtml).join(' &middot; ') : '';
  const experience = (Array.isArray(safe.experience) ? safe.experience : [])
    .map((item) => {
      const bullets = (Array.isArray(item && item.bullets) ? item.bullets : []).map((bullet) => '<li>' + escapeHtml(bullet) + '</li>').join('');
      return `<section class="item"><h3>${escapeHtml(item && item.role ? item.role : '')} — ${escapeHtml(item && item.company ? item.company : '')}</h3><p class="dates">${escapeHtml(item && item.dates ? item.dates : '')}</p><ul>${bullets}</ul></section>`;
    })
    .join('');
  const education = (Array.isArray(safe.education) ? safe.education : [])
    .map((item) => `<section class="item"><h3>${escapeHtml(item && item.degree ? item.degree : '')} — ${escapeHtml(item && item.school ? item.school : '')}</h3><p class="dates">${escapeHtml(item && item.dates ? item.dates : '')}</p></section>`)
    .join('');
  const skills = Array.isArray(safe.skills) ? safe.skills.map(escapeHtml).join(', ') : '';
  return [
    '<!doctype html>',
    '<html lang="en"><head><meta charset="utf-8">',
    '<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'">',
    '<title>' + escapeHtml(safe.name) + '</title>',
    '<style>body{font-family:system-ui,sans-serif;margin:48px;color:#111}h1{margin:0}h3{margin:0}.dates{color:#555;margin:2px 0 8px}.item{margin-bottom:16px}</style>',
    '</head><body>',
    '<h1>' + escapeHtml(safe.name) + '</h1>',
    safe.title ? '<p>' + escapeHtml(safe.title) + '</p>' : '',
    contact ? '<p>' + contact + '</p>' : '',
    safe.summary ? '<p>' + escapeHtml(safe.summary) + '</p>' : '',
    experience ? '<h2>Experience</h2>' + experience : '',
    education ? '<h2>Education</h2>' + education : '',
    skills ? '<h2>Skills</h2><p>' + skills + '</p>' : '',
    '</body></html>',
    '',
  ].join('\n');
}

export function buildResumeMarkdown(sections) {
  const safe = assertSections(sections);
  const lines = ['# ' + safe.name];
  if (safe.title) lines.push('', safe.title);
  if (Array.isArray(safe.contact) && safe.contact.length) lines.push('', safe.contact.join(' · '));
  if (safe.summary) lines.push('', safe.summary);
  if (Array.isArray(safe.experience) && safe.experience.length) {
    lines.push('', '## Experience');
    for (const item of safe.experience) {
      lines.push('', `### ${item.role || ''} — ${item.company || ''}`, item.dates || '');
      for (const bullet of Array.isArray(item.bullets) ? item.bullets : []) lines.push('- ' + bullet);
    }
  }
  if (Array.isArray(safe.education) && safe.education.length) {
    lines.push('', '## Education');
    for (const item of safe.education) lines.push('', `### ${item.degree || ''} — ${item.school || ''}`, item.dates || '');
  }
  if (Array.isArray(safe.skills) && safe.skills.length) lines.push('', '## Skills', safe.skills.join(', '));
  return lines.join('\n') + '\n';
}

export async function renderResume(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.render_resume');
  const sections = assertSections(args.sections);
  const base = typeof args.output_name === 'string' && args.output_name.length > 0 ? args.output_name.replace(/\.[A-Za-z0-9]+$/, '') : 'resume';
  const pdfName = base + '.pdf';
  const markdownName = base + '.md';
  const pdfTarget = outputPathFor(broker, pdfName);
  const markdownTarget = outputPathFor(broker, markdownName);
  writePrivateFile(markdownTarget, buildResumeMarkdown(sections));
  const htmlPath = path.join(broker.context.outputRoot, '.multica', 'resume-' + newId() + '.html');
  writePrivateFile(htmlPath, buildResumeHtml(sections));
  await broker.processRunner.run({
    command: 'chromium',
    args: [
      '--headless=new',
      '--no-sandbox',
      '--disable-gpu',
      '--disable-background-networking',
      '--disable-features=NetworkService',
      '--host-resolver-rules=MAP * ~NOTFOUND',
      '--no-pdf-header-footer',
      '--print-to-pdf=' + pdfTarget,
      'file://' + htmlPath,
    ],
    timeoutMs: broker.config.renderTimeoutMs,
  });
  broker.manifest.addFile({ id: 'resume-pdf', path: pdfTarget, name: pdfName, kind: 'pdf', role: 'primary', format: 'pdf', mimeType: 'application/pdf' });
  broker.manifest.addFile({ id: 'resume-markdown', path: markdownTarget, name: markdownName, kind: 'text', role: 'supporting', format: 'md', mimeType: 'text/markdown' });
  broker.manifest.write();
  return {
    tool: 'aurora.render_resume',
    artifacts: [
      { id: 'resume-pdf', kind: 'pdf', role: 'primary', name: pdfName },
      { id: 'resume-markdown', kind: 'text', role: 'supporting', name: markdownName },
    ],
  };
}
