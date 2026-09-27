// Deterministic caption composition: a fixed HyperFrames template plus FFmpeg.
// Cue input is transcript JSON only; the model never supplies source code.

import fs from 'node:fs';
import path from 'node:path';
import { LIMITS, assertToolAllowed, sanitizeValue } from '../policy.mjs';
import { newId, randomArtifactName, outputPathFor, writePrivateFile } from './common.mjs';

const COMPOSITION_ID = 'aurora-captions';
const STAGE_WIDTH = 1280;
const STAGE_HEIGHT = 720;
const MEDIA_BASENAME = 'source';

// Fixed extension map: the attachment mime type is validated by the task
// context, and the served file name is compiled, not model-controlled.
const VIDEO_EXTENSIONS = Object.freeze({
  'video/mp4': '.mp4',
  'video/quicktime': '.mov',
  'video/webm': '.webm',
});

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

export function mediaNameFor(attachment) {
  const extension = VIDEO_EXTENSIONS[attachment.mimeType];
  if (!extension) throw new Error('video attachment type is not supported');
  return MEDIA_BASENAME + extension;
}

export function formatDurationSeconds(duration) {
  if (!Number.isFinite(duration) || duration <= 0) throw new Error('video duration must be a positive number');
  return String(Number(duration.toFixed(3)));
}

// ffprobe is the same bounded probe the ASR tool uses; the render needs the
// real input length because a composition with no declared duration is never
// captured ("Composition has zero duration").
export function parseVideoDuration(stdout) {
  let info;
  try {
    info = JSON.parse(stdout);
  } catch {
    throw new Error('ffprobe returned invalid JSON');
  }
  const duration = Number(info && info.format ? info.format.duration : NaN);
  if (!Number.isFinite(duration) || duration <= 0) throw new Error('video duration could not be determined');
  return duration;
}

async function probeVideoDuration(broker, attachment) {
  const probe = await broker.processRunner.run({
    command: 'ffprobe',
    args: ['-v', 'error', '-print_format', 'json', '-show_format', attachment.absolutePath],
    timeoutMs: broker.config.processTimeoutMs,
  });
  return parseVideoDuration(probe.stdout);
}

// The composition is served by HyperFrames' own loopback static server, which
// serves the project directory but rejects file:// subresources. The media is
// copied beside the entry and referenced by its project-relative name.
export function buildCaptionComposition({ cues, mediaName, durationSeconds }) {
  if (typeof mediaName !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(mediaName) || mediaName.includes('..')) {
    throw new Error('composition media name is invalid');
  }
  const duration = formatDurationSeconds(durationSeconds);
  const payload = JSON.stringify(sanitizeValue(cues)).replace(/</g, '\\u003c');
  // data-no-timeline: the captions are drawn synchronously, so there is no
  // window.__timelines entry for the producer to poll (it otherwise stalls
  // 45 s per render waiting for one).
  return [
    '<!doctype html>',
    '<html lang="en">',
    '<head><meta charset="utf-8"><title>Aurora captions</title>',
    '<style>html,body{margin:0;padding:0;background:#000}',
    '#root{position:relative;width:' + STAGE_WIDTH + 'px;height:' + STAGE_HEIGHT + 'px;overflow:hidden}',
    '#source{position:absolute;inset:0;width:100%;height:100%;object-fit:contain;background:#000}',
    '#stage{position:absolute;inset:0;width:100%;height:100%;display:block}',
    '</style>',
    '</head>',
    '<body>',
    '<div id="root" data-composition-id="' + COMPOSITION_ID + '" data-no-timeline data-start="0" data-duration="' + duration + '" data-width="' + STAGE_WIDTH + '" data-height="' + STAGE_HEIGHT + '">',
    '<video id="source" class="clip" src="' + mediaName + '" muted playsinline data-start="0" data-duration="' + duration + '" data-track-index="0"></video>',
    '<canvas id="stage" class="clip" width="' + STAGE_WIDTH + '" height="' + STAGE_HEIGHT + '" data-start="0" data-duration="' + duration + '" data-track-index="1"></canvas>',
    '</div>',
    '<script id="aurora-cues" type="application/json">' + payload + '</script>',
    '<script>',
    'const cues = JSON.parse(document.getElementById("aurora-cues").textContent);',
    'const ctx = document.getElementById("stage").getContext("2d");',
    'function frame(t){ ctx.clearRect(0,0,' + STAGE_WIDTH + ',' + STAGE_HEIGHT + '); const cue = cues.find((c)=>t>=c.start&&t<=c.end); if(cue){ ctx.font="48px sans-serif"; ctx.textAlign="center"; ctx.textBaseline="middle"; const width=ctx.measureText(cue.text).width; ctx.fillStyle="rgba(0,0,0,0.6)"; ctx.fillRect((640-width/2)-16,600,width+32,64); ctx.fillStyle="#fff"; ctx.fillText(cue.text,640,632);} }',
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
  const durationSeconds = await probeVideoDuration(broker, attachment);
  const projectDirectory = path.join(broker.context.outputRoot, '.multica', 'captions-' + newId());
  fs.mkdirSync(projectDirectory, { recursive: true, mode: 0o700 });
  const mediaName = mediaNameFor(attachment);
  const mediaPath = path.join(projectDirectory, mediaName);
  fs.copyFileSync(attachment.absolutePath, mediaPath);
  fs.chmodSync(mediaPath, 0o600);
  const composition = path.join(projectDirectory, 'index.html');
  writePrivateFile(composition, buildCaptionComposition({ cues, mediaName, durationSeconds }));
  const name = randomArtifactName(args.output_name, 0, '.mp4');
  const target = outputPathFor(broker, name);
  // HyperFrames joins the -c value with the project directory (the child cwd)
  // before reading it, so an absolute -c resolves to a doubled path such as
  // captions-<id>/.../captions-<id>/index.html. Pass the entry file relative to
  // that directory and let the cwd supply the project root.
  const compositionEntry = path.relative(projectDirectory, composition);
  await broker.processRunner.run({
    command: 'hyperframes',
    args: ['render', '-c', compositionEntry, '-o', target],
    cwd: projectDirectory,
    timeoutMs: broker.config.renderTimeoutMs,
  });
  broker.manifest.addFile({ id: 'primary-1', path: target, name, kind: 'video', role: 'primary', format: 'mp4', mimeType: 'video/mp4' });
  broker.manifest.write();
  return { tool: 'aurora.render_video_captions', artifacts: [{ id: 'primary-1', kind: 'video', role: 'primary', name }] };
}
