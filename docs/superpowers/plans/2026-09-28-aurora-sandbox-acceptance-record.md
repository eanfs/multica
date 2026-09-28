# Aurora Sandbox Final Verification and Acceptance Record

**Date:** 2026-09-28
**Task:** ticket #118 — Plan D Task 7, Run Final Verification and Close the Boundary
**Base:** `main` at `72176c950` (merge commit of pr://eanfs/multica/164), branch `docs/aurora-118-final-verification`
**Host:** macOS 27.0 (build 26A428), Darwin 27.0.0 arm64, Docker Desktop 29.8.0 (`aarch64`); no AppArmor and no Linux Docker Engine
**Tooling:** Node `v24.15.0`, pnpm `10.28.2`, local Go `1.26.3` (`server/go.mod` requires `1.26.6`), `gh` 2.92.0; `cosign` is not installed

This record is the final acceptance artifact referenced by
[`2026-09-25-aurora-sandbox-image-smoke.md`](2026-09-25-aurora-sandbox-image-smoke.md),
[`2026-09-22-aurora-sandbox-tool-surface.md`](2026-09-22-aurora-sandbox-tool-surface.md),
[`2026-09-25-aurora-managed-sandbox-control-plane.md`](2026-09-25-aurora-managed-sandbox-control-plane.md),
[`2026-09-25-aurora-sandbox-fleet-isolation.md`](2026-09-25-aurora-sandbox-fleet-isolation.md),
[`2026-09-25-aurora-sandbox-skill-runtime.md`](2026-09-25-aurora-sandbox-skill-runtime.md),
and [`2026-09-11-aurora-execution.md`](2026-09-11-aurora-execution.md).

## Boundary status

The overall sandbox boundary is **not closed**: only the gated real agent/provider smokes (#117) remain open. The Seedance vendor half (#140) is resolved.

| Gate | State | Evidence or reason |
| --- | --- | --- |
| Plan A — managed control plane | accepted | all child tickets #89–#96 S4-Done; Plan A completion evidence |
| Plan B — fleet, isolation, egress | accepted | all child tickets #97–#103 S4-Done; Linux CI job green |
| Plan C — 13-skill runtime and artifacts | accepted | #104–#111 S4-Done; #106 resolved by #140 |
| Plan D Tasks 1–5 — images and acceptance | accepted | #112–#116 S4-Done; Linux CI job green |
| Plan D Task 6 — gated real smokes | **open / skipped** | #117; unimplemented, unauthorized, no provider credentials |
| Seedance licence text | **resolved** | #140; owner-authorized MIT reconstruction recorded in `vendor-lock.json` |
| Plan D Task 7 outward-facing tracker step | **deferred** | no tracker change requested; #29 unchanged |
| Story #29 and story #23 | **open** | both `S2-InProgress` |

## Step 1 — lock, vendor, and runtime tests

### `node scripts/verify-aurora-volc-skills.mjs` — exit 0 (resolved #140)

```text
verified 2 vendored Volcengine skills against the audited source lock
EXIT=0
```

Both vendor trees are vendored and verified against the audited source lock. The
Seedance tree was re-fetched on 2026-09-28 with the pinned `skills@1.7.0` CLI;
its `computedHash` (`9aed265f…ca04eb`), `wellKnownDigest`
(`sha256:97bfafe2…b42d6dd`), package name, declared version `5.0.0`, and MIT
declaration all match the audited table. Upstream still ships no LICENSE file,
so the committed `LICENSE.upstream` is the standard MIT text carrying the
`volcengine/agentplan` copyright holder declared in the package metadata, and
`vendor-lock.json` records it as an owner-authorized reconstruction
(`status: authorized`, `provenance.kind:
reconstructed-from-declared-license`, 2026-09-28) rather than a byte-identical
upstream file. The verifier rejects a lock that presents the text as verified
against upstream.

### `node scripts/verify-aurora-sandbox-locks.mjs --workflow` — exit 0

```text
verified aurora sandbox workflow: 3 jobs, 27 action step(s), every action pinned to a commit SHA, digest-only fleet refs
EXIT=0
```

### `pnpm --dir deploy/aurora-sandbox/runtime test` — exit 0

```text
tests 121
pass 121
fail 0
skipped 0
```

This includes the 13-skill broker matrix (which now drives the real patched
Seedream and Seedance modules), the fake-provider contract tests for Seedream,
Seedance, Volc ASR, OpenAI, the local deterministic tools, manifest validation,
and 15 vendor security regressions, including the new Seedance direct-CLI
refusal. Resolving #140 added one regression over the previous run's 120. No
external provider or credential is touched.

## Step 2 — frontend and backend suites

| Command | Result | Notes |
| --- | --- | --- |
| `pnpm typecheck` | pass | 12/12 turbo tasks successful, 37.98s |
| `pnpm lint` | pass | 9/9 turbo tasks successful, 0 errors, 25 warnings |
| `pnpm test` | pass | 442 test files, 5449 tests passed, 9/9 turbo tasks successful |
| `make test` | **failed once, known flake** | 69 packages `ok`; `internal/daemon/repocache` failed |

The `make test` failure is the documented `repocache` stop-process-tree flake:

```text
--- FAIL: TestRepeatedCheckoutOnTaskBranchIsNoop (0.00s)
    --- FAIL: TestRepeatedCheckoutOnTaskBranchIsNoop/linked (1.74s)
        existing_checkout_test.go:214: CreateWorktree(task 1111..., fresh=false) failed: isolate checkout Git identity: stop process tree: operation not permitted
--- FAIL: TestFreshCheckoutDiscardsLocalWork (0.00s)
    --- FAIL: TestFreshCheckoutDiscardsLocalWork/isolated (1.48s)
        existing_checkout_test.go:280: CreateWorktree(task 1111..., fresh=false) failed: create isolated checkout: list local branches: stop process tree: operation not permitted
FAIL	github.com/multica-ai/multica/server/internal/daemon/repocache	28.440s
MAKE_TEST_EXIT=2
```

The exact focused rerun passes alone:

```text
cd server && go test ./internal/daemon/repocache -run '^(TestRepeatedCheckoutOnTaskBranchIsNoop|TestFreshCheckoutDiscardsLocalWork)$' -count=1
ok  	github.com/multica-ai/multica/server/internal/daemon/repocache	1.373s
FOCUSED_EXIT=0
```

The full suite was not rerun, and this record does not claim the full suite passed. No other package failed.

## Step 3 — Linux build, image, fake-pipeline, and security gates

These gates cannot run on this host. macOS Docker Desktop has no AppArmor and no Linux kernel isolation, so the Linux acceptance is **not evaluated here**.

**Cited CI evidence:** GitHub Actions run **36422576420** (workflow "Aurora Sandbox Supply Chain", `pull_request`, head `b0b160c6b`, PR #164), job **"Verify, build, and scan" (job ID 108928656275)**, conclusion **success**, 7m57s. The merge-commit push run **36425249706** (workflow "Aurora Sandbox Supply Chain", `push` to `main`, head `72176c950`) also completed its **"Verify, build, and scan" (ID 108937840351)** job successfully in 9m56s for this exact base commit.

Every step in that job is green, including:

| Step | What it proves |
| --- | --- |
| Install dependencies | `pnpm install --frozen-lockfile` |
| Verify sandbox locks | `node scripts/verify-aurora-sandbox-locks.mjs` |
| Verify workflow supply-chain policy | the `--workflow` mode |
| Verify vendor patches and licenses | the vendor security suite |
| Run focused Node runtime tests | the runtime suite above |
| Run focused Go secret-boundary tests | managed-secret scope |
| Build host-platform sandbox image | `linux/amd64` build without push |
| Build host-platform egress proxy image | `linux/amd64` build without push |
| Verify built image contents | content/history/user/secret verifier |
| Run Linux sandbox security acceptance | the Linux matrix on a real Linux runner |
| Generate SPDX SBOMs with Syft | repository, sandbox, and egress SBOMs |
| Scan filesystem / sandbox / egress with Trivy | vuln and secret scans |
| Enforce supply-chain release policy | the #162 fixed policy step |

The machine-readable acceptance report (`aurora-sandbox-acceptance/linux-acceptance.json` from run 36422576420) records:

```json
{
  "mode": "linux-security-acceptance",
  "security_acceptance_evaluated": true,
  "platform": {
    "os": "linux", "arch": "x86_64", "kernel": "6.17.0-1022-azure",
    "docker_client": "28.0.4", "docker_server": "28.0.4",
    "cgroup_version": "2", "cgroup_driver": "systemd", "apparmor": "enabled"
  },
  "images": {
    "sandbox": "sha256:cd4b372d6830414996992057e3bc9c9711e779a8af45457373b1fbb51e4f00a5",
    "egress": "sha256:53348e3959c36fa64ceded0857a8bffcd3ea48685d8c94e79591b1fd392e88a0",
    "pipeline": "sha256:b5149d180cd2154d4a5eefa2e3fe83ccd6b90ce116dddf524fb1222d99122e20"
  },
  "counts": { "pass": 9, "fail": 0, "skip": 0, "total": 9 },
  "result": "pass"
}
```

The nine passing checks cover `TestDockerSandboxLinuxSecurityBoundary` (inspect, adversarial probes, rollback after network/proxy/sandbox creation, delete) and `TestDockerSandboxFakeAuroraPipelines` (representative actual-container fake pipelines). The Trivy image scans in the same run report 0 CRITICAL and 0 HIGH findings and 0 secrets for both images, which is why the release-policy step passes without VEX waivers.

**Limitation:** that step runs the tagged matrix once (`AURORA_DOCKER_SECURITY_COUNT` defaults to 1), so the plan's "two consecutive Linux isolation passes" is **not evidenced by this CI run**. The macOS functional smoke (functional only, `FUNCTIONAL SMOKE ONLY` / `SECURITY ACCEPTANCE NOT EVALUATED`, `SMOKE_EXIT=0`) was run in #116 and is recorded in pr://eanfs/multica/164; it does not satisfy the Linux security gate.

## Step 4 — published supply-chain artifacts

The publish job only runs on a push to `main` (`.github/workflows/aurora-sandbox.yml`, `publish` job gated on `github.repository == 'eanfs/multica' && github.ref == 'refs/heads/main'`).

History of main-branch runs at the time of this record:

| Run | Head | Verify | Publish |
| --- | --- | --- | --- |
| 36425249706 | `72176c950` | success | **in progress** (first successful verify on main) |
| 36421002060 | `80da4cf76` | success | cancelled by concurrency |
| 36315998541 | `c60676bba` | failure | skipped |
| 36290984692 | `a33678efc` | failure | skipped |
| 36288232124 | `1976429e3` | failure | skipped |

So before this task, **no main-branch publish had completed**. The merge-commit run 36425249706 reached the publish job and was still building the multi-architecture indexes when this record was written. No digest, signature, or attestation is claimed here from that run.

What this host **could not** verify:

- `cosign` is not installed, so `cosign verify` and `cosign verify-attestation` cannot run locally.
- GHCR denies anonymous access from this host (`HTTP 403 invalid token` for the token exchange, and `docker manifest inspect ... denied`), and the local `gh` token has no package scope, so no published tag/index could be inspected or confirmed to exist.
- Therefore no signature was verified by this task. Trust for signatures and attestations must come from the run's own `verify-published` job ("Verify published signatures and attestations"), which performs `cosign verify`, SPDX and provenance attestation verification, tag-to-digest equality, and architecture inspection for each image.

## Step 5 — real agent and provider smoke status

Plan D Task 6 (#117) is **not implemented** in this tree: `server/pkg/agent/aurora_sandbox_smoke_test.go` does not exist, there are no `AURORA_RUN_*_SMOKE` opt-ins, no workflow-dispatch real-smoke jobs, and no `aurora-provider-smoke` environment. Running the plan's own ungated commands confirms that no real-smoke test is compiled or executed:

```text
cd server && go test ./pkg/agent -run AuroraSandboxRealProviderSmoke -count=1 -v
testing: warning: no tests to run
ok  	github.com/multica-ai/multica/server/pkg/agent	0.555s [no tests to run]

cd server && go test -tags=agentintegration ./pkg/agent \
  -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
testing: warning: no tests to run
ok  	github.com/multica-ai/multica/server/pkg/agent	0.519s [no tests to run]
```

No provider credentials exist in this environment, no credential file was created, and no real provider or agent was contacted.

| Provider subtest | Status | Gate reason |
| --- | --- | --- |
| Claude text (`AURORA_RUN_CLAUDE_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization; no credential file |
| Seedream (`AURORA_RUN_SEEDREAM_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization; no credential file |
| Seedance (`AURORA_RUN_SEEDANCE_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization; no credential file |
| Volc ASR (`AURORA_RUN_VOLC_ASR_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization; no credential file |
| OpenAI generation/edit (`AURORA_RUN_OPENAI_IMAGE_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization; no credential file |
| HyperFrames/FFmpeg (`AURORA_RUN_HYPERFRAMES_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization |
| Chromium resume (`AURORA_RUN_CHROMIUM_SMOKE`) | **SKIPPED** | #117 unimplemented; no explicit authorization |

These are recorded as **SKIPPED**, never as failure and never as success. The deterministic fake-provider matrix in Step 1 remains the required coverage.

## Step 6/8 — documentation checks and commits

- `git diff --check` is clean.
- The plan and status documents were updated only to the extent the evidence above supports; the Seedance vendor gate (#140) is recorded as resolved and the video routes as executable, while the real-smoke gate (#117) remains labeled and open and the boundary is stated as not closed. No real provider call was made for the video routes.
- No build artifact, acceptance report, credential, or secret is committed. `.scratch/` remains ignored.
