import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import * as verifier from './verify-aurora-volc-skills.mjs';

test('owner exception retains full source, patch and approval verification', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'volc-exception-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  fs.cpSync(verifier.VENDOR_DIR, root, { recursive: true });
  const run = () => spawnSync(process.execPath, [fileURLToPath(new URL('./verify-aurora-volc-skills.mjs', import.meta.url))], {
    encoding: 'utf8', env: { ...process.env, AURORA_VOLC_VENDOR_DIR: root },
  });
  const baseline = run();
  assert.equal(baseline.status, 0, baseline.stderr);
  assert.match(baseline.stderr, /WARNING.*license evidence is missing/);
  for (const relative of ['byted-ark-seedance-skill/scripts/seedance.js', 'patches/0004-seedance-fail-closed-cli.patch']) {
    const file = path.join(root, relative);
    const original = fs.readFileSync(file);
    fs.appendFileSync(file, '\n// tampered\n');
    const result = run();
    assert.equal(result.status, 1, relative);
    assert.match(result.stderr, /mismatch/);
    fs.writeFileSync(file, original);
  }
  const file = path.join(root, 'vendor-lock.json');
  const lock = JSON.parse(fs.readFileSync(file, 'utf8'));
  delete lock.skills['byted-ark-seedance-skill'].license.exception;
  fs.writeFileSync(file, JSON.stringify(lock));
  const missing = run();
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /missing license file/);
});

const id = 'byted-ark-seedance-skill';
const exception = {
  status: 'accepted-risk', path: null, sha256: null,
  exception: {
    approved_by: 'eanfs', approved_on: '2026-09-28',
    issue: 'https://github.com/eanfs/multica/issues/140',
    version: '5.0.0', computed_hash: verifier.AUDITED[id].computedHash,
    scope: ['source-distribution', 'container-distribution'],
    reason: 'Owner accepts missing upstream MIT text and copyright evidence; this is not license verification.',
  },
};

test('accepts the explicit owner exception only for audited Seedance 5.0.0', () => {
  assert.equal(typeof verifier.isAcceptedLicenseException, 'function');
  assert.equal(verifier.isAcceptedLicenseException(id, exception), true);
});

test('rejects widening, incomplete approval and fabricated license evidence', () => {
  assert.equal(typeof verifier.isAcceptedLicenseException, 'function');
  assert.equal(verifier.isAcceptedLicenseException('byted-ark-seedream-skill', exception), false);
  for (const field of ['approved_by', 'approved_on', 'issue', 'version', 'computed_hash', 'scope', 'reason']) {
    const changed = structuredClone(exception);
    delete changed.exception[field];
    assert.equal(verifier.isAcceptedLicenseException(id, changed), false, field);
  }
  for (const [field, value] of [['version', '6.0.0'], ['computed_hash', 'wrong'], ['approved_by', 'unknown']]) {
    const changed = structuredClone(exception);
    changed.exception[field] = value;
    assert.equal(verifier.isAcceptedLicenseException(id, changed), false, field);
  }
  assert.equal(verifier.isAcceptedLicenseException(id, {...exception, sha256: 'sha256:' + 'a'.repeat(64)}), false);
  assert.equal(verifier.isAcceptedLicenseException(id, {...exception, status: 'verified'}), false);
});
