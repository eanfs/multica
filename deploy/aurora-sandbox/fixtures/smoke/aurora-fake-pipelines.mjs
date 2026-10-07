// Containerized fake end-to-end smoke for the managed Aurora sandbox image.
//
// This file is mounted read-only at /opt/aurora/smoke and executed inside the
// real sandbox image by /usr/local/bin/node (see deploy/aurora-sandbox/docker-smoke.sh,
// which runs it on Docker Desktop). The container runs with --network none,
// so the only reachable endpoints are the loopback fakes started below.
//
// The broker's provider transport allowlist is compiled to the real provider
// origins; every outbound call is injected through the broker fetch seam and
// rewritten to a local fake, so no DNS, TLS, or provider account is involved.
// HyperFrames, FFmpeg/ffprobe, Chromium, and the patched Seedream vendor module
// are the real binaries from the image.
//
// The four representative pipelines are xhs-image (fake Seedream + fake
// import/manifest/report), text-video (fake Seedance create/poll/import),
// video-captions (fake ASR + real HyperFrames/FFmpeg), and resume (faked Claude
// content supplied directly + real Chromium PDF). One provider failure proves
// the failed provider-run is settled once with no fallback create.

import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL } from "node:url";

const RUNTIME_SRC = "/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src";
const FIXTURES = "/opt/aurora/smoke/input";
const WORK = "/workspace";
const SERVER_ORIGIN = "https://multica.test";
const ARK_ORIGIN = "https://ark.cn-beijing.volces.com";
const OPENAI_ORIGIN = "https://api.openai.com";
const ASR_ORIGIN = "https://openspeech.bytedance.com";
const RESULT_HOST = "https://ark-result.test";

process.env.HOME = "/tmp";
process.env.PATH = "/opt/aurora/runtime/node_modules/.bin:" + (process.env.PATH || "/usr/local/bin:/usr/bin:/bin");

function check(condition, message) {
  if (!condition) throw new Error(message);
}

function uuid() {
  return crypto.randomUUID();
}

function startServer(handler) {
  return new Promise(function (resolve) {
    const calls = [];
    const server = http.createServer(async function (req, res) {
      const chunks = [];
      for await (const chunk of req) chunks.push(chunk);
      const body = Buffer.concat(chunks);
      const call = { method: req.method, url: req.url, headers: req.headers, body: body, bodyText: body.toString("utf8") };
      calls.push(call);
      try {
        await handler(req, res, call);
      } catch (error) {
        res.statusCode = 500;
        res.end(String(error && error.stack ? error.stack : error));
      }
    });
    server.listen(0, "127.0.0.1", function () {
      const address = server.address();
      resolve({ server: server, calls: calls, origin: "http://127.0.0.1:" + address.port });
    });
  });
}

function jsonResponse(res, status, body, headers) {
  res.writeHead(status, Object.assign({ "content-type": "application/json" }, headers || {}));
  res.end(JSON.stringify(body));
}

// --- fake Multica control plane, storage, and moderation -------------------
const control = await startServer(function (req, res, call) {
  const pathname = new URL(call.url, "http://control").pathname;
  if (pathname.endsWith("/aurora-provider-runs/begin")) {
    return jsonResponse(res, 200, { create_allowed: true, external_id: null, state: "creating" });
  }
  if (pathname.endsWith("/external")) {
    return jsonResponse(res, 200, { state: "submitted" });
  }
  if (pathname.endsWith("/finish")) {
    return jsonResponse(res, 200, { state: "settled" });
  }
  if (pathname.indexOf("/aurora-provider-runs/") !== -1) {
    return jsonResponse(res, 404, { error: "provider run not found" });
  }
  if (pathname === "/aurora/import") {
    return jsonResponse(res, 200, { staging_id: uuid(), size_bytes: 4096, sha256: "sha256:" + "1".repeat(64) });
  }
  if (pathname === "/aurora/moderation") {
    return jsonResponse(res, 200, { decision: "allow" });
  }
  return jsonResponse(res, 404, { error: "not found" });
});

// --- fake Volcengine Ark (Seedream + Seedance) -----------------------------
const arkState = { seedreamFail: false };
const ark = await startServer(function (req, res, call) {
  const pathname = new URL(call.url, "http://ark").pathname;
  if (pathname === "/api/plan/v3/images/generations") {
    if (arkState.seedreamFail) return jsonResponse(res, 500, { error: { code: "ProviderError", message: "seedream fixture failure" } });
    return jsonResponse(res, 200, { id: "seedream-" + uuid(), model: "doubao-seedream-5.0-pro", data: [{ url: RESULT_HOST + "/image-1.png" }] });
  }
  if (pathname === "/api/plan/v3/contents/generations/tasks" && req.method === "POST") {
    return jsonResponse(res, 200, { id: "seedance-task-1" });
  }
  if (pathname === "/api/plan/v3/contents/generations/tasks/seedance-task-1") {
    return jsonResponse(res, 200, { status: "succeeded", content: { video_url: RESULT_HOST + "/video-1.mp4" } });
  }
  return jsonResponse(res, 404, { error: "not found" });
});

// --- fake OpenAI (must never be called: proving no provider fallback) ------
const openai = await startServer(function (req, res) {
  return jsonResponse(res, 200, { data: [] });
});

// --- fake Volcengine ASR ---------------------------------------------------
const asr = await startServer(function (req, res) {
  return jsonResponse(res, 200, { result: { text: "fixture transcript" } }, { "x-api-status-code": "20000000" });
});

// --- fake Seedance vendor module (the real tree is blocked by the licence
// gate; the hardened adapter is exercised against this create/poll interface)
const fakeSeedance = {
  createTask: async function (options) {
    const response = await options.fetchImpl(ARK_ORIGIN + "/api/plan/v3/contents/generations/tasks", {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer " + options.env.ARK_API_KEY },
      body: JSON.stringify({ model: options.input.model, content: [{ type: "text", text: options.input.prompt }] }),
    });
    const parsed = await response.json();
    return { external_id: parsed.id };
  },
  pollTask: async function (options) {
    const response = await options.fetchImpl(ARK_ORIGIN + "/api/plan/v3/contents/generations/tasks/" + options.taskId, { method: "GET" });
    const parsed = await response.json();
    return { outputs: [{ kind: "video", url: parsed.content.video_url }] };
  },
};

// --- transport router: rewrite compiled provider origins to loopback fakes --
async function fakeFetch(url, init) {
  const target = new URL(url);
  let fake;
  if (target.origin === ARK_ORIGIN) fake = ark;
  else if (target.origin === OPENAI_ORIGIN) fake = openai;
  else if (target.origin === ASR_ORIGIN) fake = asr;
  else if (target.origin === SERVER_ORIGIN) fake = control;
  else throw new Error("unexpected outbound origin " + target.origin);
  return fetch(fake.origin + target.pathname + target.search, {
    method: (init && init.method) || "GET",
    headers: init && init.headers,
    body: init && init.body,
    redirect: "manual",
  });
}

function readManifest(outputRoot) {
  return JSON.parse(fs.readFileSync(path.join(outputRoot, ".multica/aurora-artifacts.v1.json"), "utf8"));
}

async function makeBroker(name, skillId, prompt, files, options) {
  const root = path.join(WORK, name);
  const inputRoot = path.join(root, "input");
  const outputRoot = path.join(root, "output");
  const secrets = path.join(root, "secrets");
  fs.mkdirSync(inputRoot, { recursive: true, mode: 0o700 });
  fs.mkdirSync(outputRoot, { recursive: true, mode: 0o700 });
  fs.mkdirSync(secrets, { recursive: true, mode: 0o700 });
  const secretPaths = {};
  const secretValues = { ark: "ark-fixture-value", openai: "openai-fixture-value", volcAsr: "asr-fixture-value", taskToken: "mtt-fixture-value" };
  for (const key of Object.keys(secretValues)) {
    const file = path.join(secrets, key);
    fs.writeFileSync(file, secretValues[key], { mode: 0o400 });
    fs.chmodSync(file, 0o400);
    secretPaths[key] = file;
  }
  const attachments = {};
  const attachmentIds = [];
  for (const file of files || []) {
    const target = path.join(inputRoot, file.rel);
    fs.mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 });
    fs.copyFileSync(path.join(FIXTURES, file.rel), target);
    const id = uuid();
    attachments[id] = { relative_path: file.rel, mime_type: file.mime, size_bytes: fs.statSync(target).size };
    attachmentIds.push(id);
  }
  const context = {
    schema: "com.multica.aurora.task-context",
    version: 1,
    task_id: uuid(),
    generation_id: uuid(),
    workspace_id: uuid(),
    skill_id: skillId,
    prompt: prompt,
    attachments: attachments,
    output_root: outputRoot,
    server_origin: SERVER_ORIGIN,
    task_token_file: secretPaths.taskToken,
  };
  const contextPath = path.join(root, "task-context.json");
  fs.writeFileSync(contextPath, JSON.stringify(context), { mode: 0o400 });
  fs.chmodSync(contextPath, 0o400);
  const module = await import(pathToFileURL(RUNTIME_SRC + "/server.mjs").href);
  const broker = module.createBroker({
    contextPath: contextPath,
    inputRoot: inputRoot,
    outputRoot: outputRoot,
    serverOrigin: SERVER_ORIGIN,
    secretPaths: secretPaths,
    allowedSecretPaths: Object.values(secretPaths),
    fetchImpl: fakeFetch,
    importer: async function (request) {
      const imported = await fakeFetch(SERVER_ORIGIN + "/aurora/import", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(request) });
      const parsed = await imported.json();
      await fakeFetch(SERVER_ORIGIN + "/aurora/moderation", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ staging_id: parsed.staging_id, kind: request.kind }) });
      return parsed;
    },
    vendor: options && options.vendor,
    pollIntervalMs: 1,
    pollTimeoutMs: 5000,
  });
  return { broker: broker, attachmentIds: attachmentIds, outputRoot: outputRoot };
}

async function main() {
  const completed = [];

  // xhs-image: fake Seedream generate -> fake import -> manifest.
  const xhs = await makeBroker("xhs-image", "xhs-image", "a deterministic fixture poster", [{ rel: "image/reference.png", mime: "image/png" }], null);
  const xhsResult = await xhs.broker.dispatch("aurora.seedream_generate", { prompt: "a deterministic fixture poster", attachment_ids: [xhs.attachmentIds[0]], output_name: "poster.png" });
  check(xhsResult.artifacts && xhsResult.artifacts.length >= 1, "xhs-image returned no artifact");
  const xhsManifest = readManifest(xhs.outputRoot);
  check(xhsManifest.artifacts.some(function (a) { return a.role === "primary" && a.kind === "image"; }), "xhs-image manifest has no primary image");
  completed.push("xhs-image");

  // text-video: fake Seedance create/poll -> fake import -> manifest.
  const video = await makeBroker("text-video", "text-video", "a deterministic fixture video", [], { vendor: { seedanceModule: fakeSeedance } });
  const videoResult = await video.broker.dispatch("aurora.seedance_generate", { prompt: "a deterministic fixture video", output_name: "clip.mp4" });
  check(videoResult.artifacts && videoResult.artifacts.length >= 1, "text-video returned no artifact");
  const videoManifest = readManifest(video.outputRoot);
  check(videoManifest.artifacts.some(function (a) { return a.role === "primary" && a.kind === "video"; }), "text-video manifest has no primary video");
  completed.push("text-video");

  // video-captions: fake ASR + real ffprobe/ffmpeg, then real HyperFrames render.
  const captions = await makeBroker("video-captions", "video-captions", "caption the fixture video", [{ rel: "video/short.mp4", mime: "video/mp4" }], null);
  const captionsId = captions.attachmentIds[0];
  const asrResult = await captions.broker.dispatch("aurora.volc_asr_transcribe", { attachment_id: captionsId, output_name: "transcript.txt" });
  check(asrResult.text === "fixture transcript", "ASR transcript did not match the fake provider response");
  await captions.broker.dispatch("aurora.render_video_captions", { attachment_id: captionsId, cues: [{ start: 0, end: 1, text: "fixture caption" }], output_name: "captioned.mp4" });
  const captionsManifest = readManifest(captions.outputRoot);
  check(captionsManifest.artifacts.some(function (a) { return a.role === "primary" && a.kind === "video"; }), "video-captions manifest has no primary video");
  check(captionsManifest.artifacts.some(function (a) { return a.role === "transcript"; }), "video-captions manifest has no transcript artifact");
  completed.push("video-captions");

  // resume: faked Claude sections + real Chromium PDF.
  const resume = await makeBroker("resume", "resume", "render the fixture resume", [{ rel: "document/resume.md", mime: "text/markdown" }], null);
  await resume.broker.dispatch("aurora.render_resume", {
    sections: {
      name: "Jordan Example",
      title: "Fixture Engineer",
      contact: ["jordan@example.invalid"],
      summary: "Synthetic resume fixture.",
      experience: [{ role: "Engineer", company: "Example Co", dates: "2020-2024", bullets: ["Built deterministic fixtures"] }],
      skills: ["Go", "Docker"],
    },
    output_name: "resume",
  });
  const resumeManifest = readManifest(resume.outputRoot);
  check(resumeManifest.artifacts.some(function (a) { return a.role === "primary" && a.format === "pdf"; }), "resume manifest has no primary PDF");
  const pdf = path.join(resume.outputRoot, "artifacts", "resume.pdf");
  check(fs.existsSync(pdf) && fs.statSync(pdf).size > 0, "resume PDF is missing or empty");
  check(fs.readFileSync(pdf).slice(0, 4).toString("latin1") === "%PDF", "resume output is not a PDF");
  completed.push("resume");

  // provider failure: one failed settlement, no fallback provider create.
  arkState.seedreamFail = true;
  const controlBefore = control.calls.length;
  const arkBefore = ark.calls.length;
  const openaiBefore = openai.calls.length;
  const failing = await makeBroker("provider-failure", "xhs-image", "a deterministic fixture poster", [], null);
  let rejected = false;
  try {
    await failing.broker.dispatch("aurora.seedream_generate", { prompt: "a deterministic fixture poster", output_name: "poster.png" });
  } catch (error) {
    rejected = true;
  }
  check(rejected, "the failing Seedream provider did not reject the tool call");
  const controlCalls = control.calls.slice(controlBefore);
  const begins = controlCalls.filter(function (c) { return c.url.endsWith("/begin"); });
  const finishes = controlCalls.filter(function (c) { return c.url.endsWith("/finish"); });
  check(begins.length === 1, "expected exactly one provider-run begin, got " + begins.length);
  check(finishes.length === 1, "expected exactly one provider-run settlement, got " + finishes.length);
  check(finishes[0].bodyText.indexOf("failed") !== -1, "provider-run settlement did not record a failed state (refund trigger)");
  check(openai.calls.length === openaiBefore, "a fallback OpenAI provider call was made after the failure");
  check(ark.calls.length - arkBefore === 1, "the failure attempted more than one Ark create");
  check(!fs.existsSync(path.join(failing.outputRoot, ".multica/aurora-artifacts.v1.json")), "the failed pipeline wrote a manifest");
  completed.push("provider-failure-refund-no-fallback");

  console.log("AURORA_SMOKE_RESULT " + JSON.stringify({ ok: true, pipelines: completed }));
}

try {
  await main();
  // The loopback fake servers keep the event loop alive, so a successful run
  // would otherwise hang the container instead of reporting a clean exit.
  // Flush the success marker, then exit for real.
  await new Promise(function (resolve) { process.stdout.write("", resolve); });
  process.exit(0);
} catch (error) {
  console.error("AURORA_SMOKE_FAILURE " + (error && error.message ? error.message : String(error)));
  process.exit(1);
}
