// Hardened Volcengine ASR tool with FFmpeg extraction and bounded base64 streaming.

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { ASR, LIMITS, PROVIDER_RUN_OPERATIONS, PROVIDER_ORIGINS, assertToolAllowed, sanitizeError } from '../policy.mjs';
import { outputPathFor, randomArtifactName, secretValue, newId, safeUnlink, writePrivateFile } from './common.mjs';
import { readBoundedJson } from '../transport.mjs';

const SUPPORTED_VIDEO_CODECS = ['h264', 'hevc', 'vp8', 'vp9', 'av1'];

function audioFormatFor(attachment) {
  if (attachment.mimeType === 'audio/wav') return 'wav';
  if (attachment.mimeType === 'audio/mpeg') return 'mp3';
  if (attachment.mimeType === 'audio/ogg' || attachment.mimeType === 'audio/opus') return 'ogg';
  return 'wav';
}

async function extractBoundedAudio(broker, attachment) {
  const probe = await broker.processRunner.run({
    command: 'ffprobe',
    args: ['-v', 'error', '-print_format', 'json', '-show_format', '-show_streams', attachment.absolutePath],
    timeoutMs: broker.config.processTimeoutMs,
  });
  let info;
  try {
    info = JSON.parse(probe.stdout);
  } catch {
    throw new Error('ffprobe returned invalid JSON');
  }
  const duration = Number(info && info.format ? info.format.duration : NaN);
  if (Number.isFinite(duration) && duration > LIMITS.maxAudioSeconds) {
    throw new Error('video exceeds the two hour audio cap');
  }
  const streams = Array.isArray(info && info.streams) ? info.streams : [];
  const audio = streams.find((stream) => stream.codec_type === 'audio');
  const video = streams.find((stream) => stream.codec_type === 'video');
  if (!video) throw new Error('input has no video stream');
  if (!audio) throw new Error('video has no audio stream');
  if (!SUPPORTED_VIDEO_CODECS.includes(String(video.codec_name || '').toLowerCase())) {
    throw new Error('unsupported video codec for audio extraction');
  }
  const output = path.join(broker.context.outputRoot, '.multica', 'asr-' + newId() + '.wav');
  fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 });
  await broker.processRunner.run({
    command: 'ffmpeg',
    args: ['-y', '-i', attachment.absolutePath, '-vn', '-ac', '1', '-ar', '16000', '-f', 'wav', output],
    timeoutMs: broker.config.processTimeoutMs,
  });
  return { audioPath: output, format: 'wav', cleanup: () => safeUnlink(output) };
}

export async function volcAsrTranscribe(broker, args) {
  assertToolAllowed(broker.context.skillId, 'aurora.volc_asr_transcribe');
  const attachment = broker.context.requireAttachment(args.attachment_id, ['audio', 'video']);
  let audioPath = attachment.absolutePath;
  let format = audioFormatFor(attachment);
  let cleanup = null;
  if (attachment.kind === 'video') {
    const extracted = await extractBoundedAudio(broker, attachment);
    audioPath = extracted.audioPath;
    format = extracted.format;
    cleanup = extracted.cleanup;
  }

  const operation = PROVIDER_RUN_OPERATIONS.asr;
  const model = broker.models.asr;
  const begun = await broker.providerRun.begin({
    provider: 'volcengine-asr',
    operation,
    model,
    args: { attachmentId: attachment.id },
  });
  if (!begun.create_allowed) {
    if (cleanup) cleanup();
    throw new Error('ASR provider run create lease was not granted');
  }

  try {
    const bytes = fs.readFileSync(audioPath);
    if (bytes.length > LIMITS.maxAudioBytes) throw new Error('audio input exceeds the 100 MiB cap');
    const body = {
      user: { uid: broker.context.taskId },
      audio: { format, data: bytes.toString('base64') },
      request: { model_name: ASR.modelName, enable_punc: true, enable_itn: true },
    };
    const response = await broker.transport.providerFetch(PROVIDER_ORIGINS.volcAsr + ASR.path, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'x-api-key': secretValue(broker, 'volcAsr'),
        'x-api-resource-id': ASR.resourceId,
        'x-api-request-id': crypto.randomUUID(),
        'x-api-sequence': '-1',
      },
      body: JSON.stringify(body),
    });
    const status = response.headers.get('x-api-status-code');
    const parsed = await readBoundedJson(response, LIMITS.maxAsrResponseBytes);
    if (status !== ASR.successStatus) {
      throw new Error('Volcengine ASR failed with status ' + String(status));
    }
    const transcript = parsed && parsed.result && typeof parsed.result.text === 'string' ? parsed.result.text : null;
    if (transcript === null) throw new Error('Volcengine ASR returned no transcript text');
    const name = randomArtifactName(args.output_name, 0, '.txt');
    const target = outputPathFor(broker, name);
    writePrivateFile(target, transcript);
    // video-captions chains ASR into the caption renderer: the transcript is a
    // supporting artifact and the renderer publishes the final manifest with
    // the primary video. transcription terminates at ASR, so its transcript is
    // the primary output and is published immediately.
    const intermediate = broker.context.skillId === 'video-captions';
    const artifactId = intermediate ? 'transcript-1' : 'primary-1';
    const role = intermediate ? 'transcript' : 'primary';
    broker.manifest.addFile({ id: artifactId, path: target, name, kind: 'text', role, format: 'txt', mimeType: 'text/plain' });
    broker.manifest.setProviderRun({ provider: 'volcengine-asr', model, external_id: null });
    await broker.providerRun.finish(operation, 'succeeded');
    if (!intermediate) broker.manifest.write();
    if (cleanup) cleanup();
    return { tool: 'aurora.volc_asr_transcribe', operation, model, text: transcript, artifacts: [{ id: artifactId, kind: 'text', role, name }] };
  } catch (error) {
    if (cleanup) cleanup();
    await broker.providerRun.finish(operation, 'failed', 'provider_failed');
    throw sanitizeError(error);
  }
}
