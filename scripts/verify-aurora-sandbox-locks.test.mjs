// Tests for scripts/verify-aurora-sandbox-locks.mjs.
//
// Every case mutates one audited lock input in a temporary fixture tree that
// shadows the committed lock, then proves the cross-lock verifier rejects the
// drift. The committed lock must verify cleanly.
//
// Run: node --test scripts/verify-aurora-sandbox-locks.test.mjs

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { REPO_ROOT, verify } from "./verify-aurora-sandbox-locks.mjs";

const VERSIONS = "deploy/aurora-sandbox/versions.json";
const RUNTIME_PACKAGE = "deploy/aurora-sandbox/runtime/package.json";
const APT_LOCK = "deploy/aurora-sandbox/apt-packages.lock";
const VENDOR_LOCK = "deploy/aurora-sandbox/vendor/volcengine/vendor-lock.json";
const DOCKERFILE = "deploy/aurora-sandbox/Dockerfile";

const NODE_REF = "node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c";

function readJson(rel) {
  return JSON.parse(fs.readFileSync(path.join(REPO_ROOT, rel), "utf8"));
}

function writeFixture(files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aurora-sandbox-locks-"));
  for (const [rel, content] of Object.entries(files)) {
    const abs = path.join(dir, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, typeof content === "string" ? content : JSON.stringify(content, null, 2) + "\n");
  }
  return dir;
}

function fixtureFrom(rel, mutate) {
  const value = readJson(rel);
  mutate(value);
  return writeFixture({ [rel]: value });
}

test("committed lock verifies cleanly", async () => {
  const summary = await verify();
  assert.match(summary, /verified aurora sandbox locks/);
});

test("rejects a mutable base image tag without a digest", async () => {
  await assert.rejects(() => verify({ nodeBase: "node:22-bookworm-slim" }), /digest/);
  await assert.rejects(() => verify({ goBuilder: "golang:1.26.6-bookworm" }), /digest/);
});

test("rejects a wrong base image digest", async () => {
  await assert.rejects(
    () => verify({ nodeBase: "node:22-bookworm-slim@sha256:" + "0".repeat(64) }),
    /locked digest/,
  );
});

test("rejects a semver range for a production dependency", async () => {
  await assert.rejects(() => verify({ claudeCode: "^2.1.282" }), /exact version/);
  await assert.rejects(() => verify({ claudeCode: "~2.1.282" }), /exact version/);
});

test("rejects a mismatched Go version", async () => {
  const root = fixtureFrom(VERSIONS, (v) => {
    v.go.toolchain = "1.25.0";
  });
  await assert.rejects(() => verify({ root }), /Go version/);
});

test("rejects a changed HyperFrames commit", async () => {
  const root = fixtureFrom(VERSIONS, (v) => {
    v.hyperframes_source.commit = "0".repeat(40);
  });
  await assert.rejects(() => verify({ root }), /HyperFrames commit/);
});

test("rejects a missing vendor tree hash", async () => {
  const root = fixtureFrom(VERSIONS, (v) => {
    v.vendor_trees["byted-ark-seedream-skill"].whole_tree_sha256 = null;
  });
  await assert.rejects(() => verify({ root }), /vendor tree hash/);
});

test("rejects an unpinned apt package in the lock", async () => {
  const root = fixtureFrom(APT_LOCK, (lock) => {
    lock.packages.tini.amd64.version = "";
  });
  await assert.rejects(() => verify({ root }), /unpinned apt package/);
});

test("rejects a changed Debian snapshot date", async () => {
  const root = fixtureFrom(VERSIONS, (v) => {
    v.debian_snapshot.timestamp = "20260926T000000Z";
  });
  await assert.rejects(() => verify({ root }), /snapshot date/);
});

test("rejects an unexpected production dependency", async () => {
  const root = fixtureFrom(RUNTIME_PACKAGE, (pkg) => {
    pkg.dependencies["left-pad"] = "1.0.0";
  });
  await assert.rejects(() => verify({ root }), /unexpected production dependency/);
});

test("rejects a missing locked production dependency", async () => {
  const root = fixtureFrom(RUNTIME_PACKAGE, (pkg) => {
    delete pkg.dependencies["@anthropic-ai/claude-code"];
  });
  await assert.rejects(() => verify({ root }), /missing @anthropic-ai\/claude-code/);
});

test("rejects an executable vendor tree without a license hash", async () => {
  const root = fixtureFrom(VENDOR_LOCK, (lock) => {
    lock.skills["byted-ark-seedream-skill"].license.sha256 = null;
    lock.skills["byted-ark-seedream-skill"].files.find((f) => f.path.endsWith("generate.js")).mode = "0755";
  });
  await assert.rejects(() => verify({ root }), /license hash/);
});

test("rejects a Dockerfile FROM with a mutable tag", async () => {
  const root = writeFixture({ [DOCKERFILE]: "FROM node:22-bookworm-slim\n" });
  await assert.rejects(() => verify({ root }), /digest/);
});

test("rejects a Dockerfile FROM with the wrong digest", async () => {
  const root = writeFixture({
    [DOCKERFILE]: "FROM node:22-bookworm-slim@sha256:" + "0".repeat(64) + "\n",
  });
  await assert.rejects(() => verify({ root }), /locked digest/);
});

test("rejects a Dockerfile apt install without exact versions", async () => {
  const root = writeFixture({
    [DOCKERFILE]:
      "FROM " + NODE_REF + "\nRUN apt-get update && apt-get install -y --no-install-recommends tini unzip\n",
  });
  await assert.rejects(() => verify({ root }), /unpinned apt package/);
});

test("rejects a Dockerfile network fetch for skills or HyperFrames source", async () => {
  const root = writeFixture({
    [DOCKERFILE]:
      "FROM " + NODE_REF + "\n" +
      "RUN curl -fsSL https://github.com/example/HyperFrames/archive/refs/tags/v0.8.75.tar.gz -o /tmp/hf.tgz\n",
  });
  await assert.rejects(() => verify({ root }), /network fetch/);
});
