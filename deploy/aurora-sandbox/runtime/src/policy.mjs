// Fixed Aurora broker policy.
//
// Provider choice, model choice, origins, endpoints, and input/output rules are
// compiled constants. Nothing here is influenced by tool arguments, the prompt,
// or the model.

export const POLICY_VERSION = 'aurora-sandbox-skill-runtime/v1';

export const PROVIDER_ORIGINS = Object.freeze({
  ark: 'https://ark.cn-beijing.volces.com',
  volcAsr: 'https://openspeech.bytedance.com',
  openai: 'https://api.openai.com',
});

export const OPENAI_BASE_URL = 'https://api.openai.com/v1';

export const ARK_PATHS = Object.freeze({
  seedreamGenerate: '/api/plan/v3/images/generations',
  seedanceCreate: '/api/plan/v3/contents/generations/tasks',
  seedancePoll: '/api/plan/v3/contents/generations/tasks/',
});

export const ASR = Object.freeze({
  path: '/api/v3/auc/bigmodel/recognize/flash',
  resourceId: 'volc.bigasr.auc_turbo',
  modelName: 'bigmodel',
  successStatus: '20000000',
});

export const OPENAI_PATHS = Object.freeze({
  generate: '/v1/images/generations',
  edit: '/v1/images/edits',
});

export const PROVIDER_RUN_OPERATIONS = Object.freeze({
  seedream: 'seedream.generate',
  seedance: 'seedance.create',
  openaiGenerate: 'images.generate',
  openaiEdit: 'images.edit',
  asr: 'asr.recognize',
});

export const ALLOWED_MODELS = Object.freeze({
  seedream: Object.freeze(['doubao-seedream-5.0-lite', 'doubao-seedream-5.0-pro']),
  seedance: Object.freeze(['doubao-seedance-2.0', 'doubao-seedance-2.0-fast', 'doubao-seedance-2.0-mini', 'doubao-seedance-2.5']),
  openai: Object.freeze(['gpt-image-2.5-sunburst', 'gpt-image-2.5-flare']),
  asr: Object.freeze(['bigmodel']),
});

// Operator-selected deployment defaults. Tool input can never change these.
export const DEFAULT_MODELS = Object.freeze({
  seedream: 'doubao-seedream-5.0-pro',
  seedance: 'doubao-seedance-2.0',
  openai: 'gpt-image-2.5-sunburst',
  asr: 'bigmodel',
});

export const LIMITS = Object.freeze({
  maxContextBytes: 1024 * 1024,
  maxSecretBytes: 4096,
  maxPromptChars: 10000,
  maxArtifactNameChars: 128,
  maxProviderResponseBytes: 1024 * 1024,
  maxAsrResponseBytes: 1024 * 1024,
  maxDocumentChars: 200000,
  maxTextArtifactBytes: 2 * 1024 * 1024,
  maxManifestBytes: 1024 * 1024,
  maxArtifacts: 20,
  maxTotalArtifactBytes: 600 * 1024 * 1024,
  maxImageBytes: 25 * 1024 * 1024,
  maxDocumentBytes: 25 * 1024 * 1024,
  maxAudioBytes: 100 * 1024 * 1024,
  maxVideoBytes: 100 * 1024 * 1024,
  maxAudioSeconds: 2 * 60 * 60,
  maxSeedreamReferences: 14,
  maxCues: 2000,
});

const IMAGE_TYPES = Object.freeze({ png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg' });
const DOCUMENT_TYPES = Object.freeze({
  txt: 'text/plain',
  md: 'text/markdown',
  markdown: 'text/markdown',
  pdf: 'application/pdf',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
});
const AUDIO_TYPES = Object.freeze({
  wav: 'audio/wav',
  mp3: 'audio/mpeg',
  ogg: 'audio/ogg',
  opus: 'audio/opus',
});
const VIDEO_TYPES = Object.freeze({
  mp4: 'video/mp4',
  mov: 'video/quicktime',
  webm: 'video/webm',
});

const KIND_TYPES = Object.freeze({
  image: IMAGE_TYPES,
  document: DOCUMENT_TYPES,
  audio: AUDIO_TYPES,
  video: VIDEO_TYPES,
});

// The fixed route matrix. Every value is compiled; no user input is consulted.
export const SKILL_ROUTES = Object.freeze({
  poster: { route: 'seedream', tools: ['aurora.seedream_generate'], prompt: [1, 3000], attachments: { min: 0, max: 4, kinds: ['image'] } },
  'xhs-image': { route: 'seedream', tools: ['aurora.seedream_generate'], prompt: [1, 3000], attachments: { min: 0, max: 4, kinds: ['image'] } },
  'product-image': { route: 'openai-images', tools: ['aurora.openai_image'], prompt: [1, 3000], attachments: { min: 0, max: 4, kinds: ['image'] } },
  'text-image': { route: 'seedream', tools: ['aurora.seedream_generate'], prompt: [1, 3000], attachments: { min: 0, max: 0, kinds: [] } },
  'image-edit': { route: 'openai-images-edit', tools: ['aurora.openai_image'], prompt: [1, 3000], attachments: { min: 1, max: 4, kinds: ['image'] } },
  'id-photo': { route: 'local-id-photo', tools: ['aurora.id_photo'], prompt: [1, 3000], attachments: { min: 1, max: 1, kinds: ['image'] } },
  'image-video': { route: 'seedance', tools: ['aurora.seedance_generate'], prompt: [1, 3000], attachments: { min: 1, max: 1, kinds: ['image'] } },
  'text-video': { route: 'seedance', tools: ['aurora.seedance_generate'], prompt: [1, 3000], attachments: { min: 0, max: 0, kinds: [] } },
  'video-captions': { route: 'volcengine-asr-hyperframes', tools: ['aurora.volc_asr_transcribe', 'aurora.render_video_captions'], prompt: [1, 3000], attachments: { min: 1, max: 1, kinds: ['video'] } },
  'xhs-copy': { route: 'local-text', tools: ['aurora.read_document', 'aurora.write_text_artifact'], prompt: [1, 10000], attachments: { min: 0, max: 1, kinds: ['document'] } },
  resume: { route: 'local-resume', tools: ['aurora.render_resume', 'aurora.read_document'], prompt: [1, 10000], attachments: { min: 0, max: 1, kinds: ['document'] } },
  'document-summary': { route: 'local-text', tools: ['aurora.read_document', 'aurora.write_text_artifact'], prompt: [1, 10000], attachments: { min: 1, max: 1, kinds: ['document'] } },
  transcription: { route: 'volcengine-asr', tools: ['aurora.volc_asr_transcribe'], prompt: [1, 3000], attachments: { min: 1, max: 1, kinds: ['audio', 'video'] } },
});

export const AVAILABLE_SKILLS = Object.freeze(Object.keys(SKILL_ROUTES));

export const VENDOR_PRODUCERS = Object.freeze({
  seedream: Object.freeze({ id: 'byted-ark-seedream-skill', version: '4.0.0', tree_sha256: 'sha256:aac297142496fe2f07f3c4d9a8d792110c5bad871a515f72b1374bbde3a1fc0d' }),
  seedance: Object.freeze({ id: 'byted-ark-seedance-skill', version: '5.0.0', tree_sha256: null }),
  openai: Object.freeze({ id: 'openai-images', version: '7.23.0', tree_sha256: null }),
  asr: Object.freeze({ id: 'volcengine-asr', version: '1.0.0', tree_sha256: null }),
  local: Object.freeze({ id: 'multica-aurora-runtime', version: '1.0.0', tree_sha256: null }),
});

export function skillPolicy(skillId) {
  return SKILL_ROUTES[skillId] || null;
}

export function allowedToolsForSkill(skillId) {
  const policy = skillPolicy(skillId);
  return policy ? [...policy.tools] : [];
}

export function assertToolAllowed(skillId, toolName) {
  const tools = allowedToolsForSkill(skillId);
  if (!tools.includes(toolName)) {
    throw new Error(`tool ${toolName} is not allowed for skill ${skillId}`);
  }
  return true;
}

export function assertArtifactName(name) {
  if (typeof name !== 'string' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(name) || name.includes('..')) {
    throw new Error('artifact name is not permitted');
  }
  return name;
}

export function classifyAttachment({ name, mimeType }) {
  if (typeof name !== 'string' || typeof mimeType !== 'string') return null;
  const extension = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1).toLowerCase() : '';
  const normalizedMime = mimeType.split(';')[0].trim().toLowerCase();
  for (const [kind, table] of Object.entries(KIND_TYPES)) {
    if (table[extension] && table[extension] === normalizedMime) return kind;
  }
  return null;
}

export function kindLimit(kind) {
  if (kind === 'image') return LIMITS.maxImageBytes;
  if (kind === 'document') return LIMITS.maxDocumentBytes;
  if (kind === 'audio') return LIMITS.maxAudioBytes;
  if (kind === 'video') return LIMITS.maxVideoBytes;
  throw new Error('unsupported attachment kind');
}

export function producerForSkill(skillId) {
  const policy = skillPolicy(skillId);
  if (!policy) return VENDOR_PRODUCERS.local;
  if (policy.route === 'seedream') return VENDOR_PRODUCERS.seedream;
  if (policy.route === 'seedance') return VENDOR_PRODUCERS.seedance;
  if (policy.route === 'openai-images' || policy.route === 'openai-images-edit') return VENDOR_PRODUCERS.openai;
  if (policy.route === 'volcengine-asr' || policy.route === 'volcengine-asr-hyperframes') return VENDOR_PRODUCERS.asr;
  return VENDOR_PRODUCERS.local;
}

export function isUuid(value) {
  return typeof value === 'string' && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

const SECRET_KEY_RE = /^(api[_-]?key|authorization|password|secret|token|access[_-]?key)$/i;

export function redactString(value) {
  return String(value)
    .replace(/ark-[A-Za-z0-9._-]{4,}/g, 'ark-[redacted]')
    .replace(/mat-[A-Za-z0-9._-]{4,}/g, 'mat-[redacted]')
    .replace(/Bearer\s+[A-Za-z0-9._-]+/gi, 'Bearer [redacted]')
    .replace(/data:[a-z0-9.+-]+\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/=\s]+/gi, '[data-uri-redacted]')
    .replace(/([?&](?:X-Amz-Signature|Signature|Expires|OSSAccessKeyId|security-token)=)[^&\s]+/gi, '$1[redacted]');
}

export function sanitizeValue(value, depth = 0) {
  if (depth > 8) return '[truncated]';
  if (typeof value === 'string') return redactString(value);
  if (Array.isArray(value)) return value.map((item) => sanitizeValue(item, depth + 1));
  if (value && typeof value === 'object') {
    const out = {};
    for (const key of Object.keys(value)) {
      if (SECRET_KEY_RE.test(key)) out[key] = '[redacted]';
      else out[key] = sanitizeValue(value[key], depth + 1);
    }
    return out;
  }
  return value;
}

export function sanitizeError(error) {
  const message = error && error.message ? error.message : String(error);
  const sanitized = new Error(redactString(message));
  if (error && error.status !== undefined) sanitized.status = error.status;
  return sanitized;
}
