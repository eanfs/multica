// Deterministic caption composition: a fixed HyperFrames template plus FFmpeg.
// Cue input is transcript JSON only; the model never supplies source code.

import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { LIMITS, assertToolAllowed, sanitizeValue } from '../policy.mjs';
import { newId, randomArtifactName, outputPathFor, writePrivateFile } from './common.mjs';

function assertCues(cues) {
  if (!Array.isArray(cues) || cues.length === 0) throw new Error('cues are required');
  if (cues.length > LIMITS.maxCues) throw new Error('too many cues');
  return cues.map((cue) => {
    if (!cue || typeof cue !== 'object') throw new Error('each cue must be an object');
    const start = Number(cue.start);
    const end = Number(cue.end);
    if (!Number.isFinite(start) || !Number.isFinite(end) || start < 0 || end < start) {
      throw new Error('cue timing is invalid');
    }
    if (typeof cue.text !== 'string' || cue.text.length === 0 || cue.text.length > 2000) {
      throw new Error('cue text is invalid');
    }
    return { start, end, text: cue.text };
  });
}

export function buildCaptionComposition({ cues, videoUrl }) {
  const payload = JSON.stringify(sanitizeValue(cues)).replace(/</g, '\\u003c');
  const source = typeof videoUrl === 'string' && videoUrl.startsWith('file://') ? videoUrl : '';
  return [
    '<!doctype html>',
    '<html lang="en">',
    '<head><meta charset="utf-8"><title>Aurora captions</title>',
    '<style>html,body{margin:0;padding:0;background:#000}canvas{display:block}</style>',
    '</head>',
    '<body>',
    '<video id="source" src="' + source + '" muted playsinline></video>',
    '<canvas id="stage" width="1280" height="720"></canvas>',
    '<script id="aurora-cues" type="application/json">' + payload + '</script>',
    '<script>',
    'const cues = JSON.parse(document.getElementById("aurora-cues").textContent);',
    'const ctx = document.getElementById("stage").getContext("2d");',
    'function frame(t){ ctx.fillStyle="#000"; ctx.fillRect(0,0,1280,720); const cue = cues.find((c)=>t>=c.start&&t<=c.end); if(cue){ ctx.fillStyle="#fff"; ctx.font="48px sans-serif"; ctx.textAlign="center"; ctx.fillText(cue.text,640,650);} }',
    'frame(0);',
    '</script>',
    '</body>',
    '</html>',
    '',
  ].join('\n');
}

export async function renderVideoCaptions(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.render_video_captions');
  const attachment = broker.context.requireAttachment(args.attachment_id, ['video']);
  const cues = assertCues(args.cues);
  const projectDirectory = path.join(broker.context.outputRoot, '.multica', 'captions-' + newId());
  const composition = path.join(projectDirectory, 'index.html');
  writePrivateFile(composition, buildCaptionComposition({ cues, videoUrl: pathToFileURL(attachment.absolutePath).href }));
  const name = randomArtifactName(args.output_name, 0, '.mp4');
  const target = outputPathFor(broker, name);
  await broker.processRunner.run({
    command: 'hyperframes',
    args: ['render', '-c', composition, '-o', target],
    cwd: projectDirectory,
    timeoutMs: broker.config.renderTimeoutMs,
  });
  broker.manifest.addFile({ id: 'primary-1', path: target, name, kind: 'video', role: 'primary', format: 'mp4', mimeType: 'video/mp4' });
  broker.manifest.write();
  return { tool: 'aurora.render_video_captions', artifacts: [{ id: 'primary-1', kind: 'video', role: 'primary', name }] };
}
