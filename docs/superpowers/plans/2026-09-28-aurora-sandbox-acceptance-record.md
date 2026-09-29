# Aurora Sandbox Final Verification and Acceptance Record

**Date:** 2026-09-28 (initial acceptance); updated 2026-09-29
**Task:** ticket #118 — Plan D Task 7, Run Final Verification and Close the Boundary
**Base:** `main` at `92daf28a4` (merge commit of pr://eanfs/multica/168); the initial acceptance was taken at `72176c950` (merge commit of pr://eanfs/multica/164), branch `docs/aurora-118-final-verification` (updated on `docs/aurora-progress-update`)
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

The overall sandbox boundary is **not closed**: the gated real agent/provider smokes (#117) remain open, and Plan D Task 7 Step 4 stays open until the post-#140 main publish records its final image digests. The Seedance vendor half (#140) is resolved, and the main-branch supply-chain trust chain is verified for the pre-#140 image pair.

| Gate | State | Evidence or reason |
| --- | --- | --- |
| Plan A — managed control plane | accepted | all child tickets #89–#96 S4-Done; Plan A completion evidence |
| Plan B — fleet, isolation, egress | accepted | all child tickets #97–#103 S4-Done; Linux CI job green |
| Plan C — 13-skill runtime and artifacts | accepted | #104–#111 S4-Done; #106 resolved by #140; both vendor trees vendored, hardened, and verified |
| Plan D Tasks 1–5 — images and acceptance | accepted | #112–#116 S4-Done; Linux CI job green; publish and signature/attestation verification green on run 36430260728 |
| Plan D Task 6 — gated real smokes | **open / in progress / not authorized** | #117; no provider credentials or credential file; every provider subtest skipped with its gate reason |
| Seedance licence text | **resolved** | #140, merged as pr://eanfs/multica/168 (`92daf28a4`); owner-authorized MIT reconstruction recorded in `vendor-lock.json`, verifier strengthened to require it |
| Plan D Task 7 Step 4 — published digests | **open pending final digests** | pre-#140 pair verified on run 36430260728; the post-#140 publish will produce the final Seedance-inclusive digests |
| Plan D Task 7 outward-facing tracker step | **deferred** | no tracker change requested; #29 unchanged |
| Story #29 and story #23 | **open** | both `S2-InProgress` |

## Step 1 — lock, vendor, and runtime tests

### `node scripts/verify-aurora-volc-skills.mjs` — exit 0 (resolved #140)

```text
verified 2 vendored Volcengine skills against the audited source lock
EXIT=0
```

Both vendor trees are vendored and verified against the audited source lock. The
Seedance tree was re-fetched on 2026-09-28 with the pinned `skills@1.7.0` CLI,
and its fetch matched the audited lock before vendoring: `computedHash`
`9aed265f32003e867089212d831c1982e369fd766c02d8459770370377ca04eb`,
`wellKnownDigest`
`sha256:97bfafe21dfdd127cb7a040413ac2be5a0a816f75db1d049506dac222b42d6dd`,
package `byted-ark-seedance-skill`, declared version `5.0.0`, and an MIT
declaration in `SKILL.md`. Upstream still ships no LICENSE file, so the committed
`LICENSE.upstream` is the standard MIT text carrying the `VolcEngine / AgentPlan`
holder taken from the `SKILL.md` frontmatter
`metadata.author: volcengine/agentplan` (SHA-256
`8a32047e9ee5270ab9f49e5d293b650bc0253e05bff2d81787b3276ea0d75935`). That text
is byte-identical to the already-vendored Seedream licence because both are
published by the same holder with the same copyright line. `vendor-lock.json`
records the reconstruction explicitly — `provenance.kind:
reconstructed-from-declared-license`, `upstream_ships_license: false`, the
holder and its source, and the repository owner's authorization dated
2026-09-28 — and the verifier was **strengthened** to require that provenance
and to reject a lock that presents the text as verified against upstream.
Status, provenance, and hash mutations were each negative-tested to exit 1.

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
refusal. The suite went from 120 tests to 121 and from 14 to 15 vendor security
regressions, with zero skipped before and after. The Seedance tree ships
`patches/0004-seedance-fail-closed-cli.patch`, which makes running the Seedance
CLI directly exit 2. `image-video` and `text-video` are executable again because
the broker adapter resolves the vendored, patched Seedance module; no real
provider call has been exercised for the video routes, and no external provider
or credential is touched.

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

**#140 CI evidence:** GitHub Actions run **36431624767** (workflow "Aurora Sandbox Supply Chain") was green. It verified the vendored skills and the vendor patches and licences, built both images including the new Docker vendor stage, ran the built-image content verifier over both trees with the in-image self-test, ran the Linux sandbox security acceptance with `AURORA_DOCKER_SECURITY_COUNT=2` (the matrix twice), and passed the supply-chain release policy. One real defect was caught by CI on the way: the vendor lock recorded mode `0600` for the reconstructed Seedance licence while a fresh checkout produces `0644`; commit `b4222a9d6` fixed it by shipping `0644` and recording `0644`.

The cited run 36422576420 job's steps are green, including:

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

**Limitation:** run 36422576420 ran the tagged matrix once (`AURORA_DOCKER_SECURITY_COUNT` defaults to 1), so that run alone did not evidence the plan's "two consecutive Linux isolation passes". The #140 run 36431624767 now sets `AURORA_DOCKER_SECURITY_COUNT=2` and is green, which evidences the two passes on the Seedance-inclusive image. The macOS functional smoke (functional only, `FUNCTIONAL SMOKE ONLY` / `SECURITY ACCEPTANCE NOT EVALUATED`, `SMOKE_EXIT=0`) was run in #116 and is recorded in pr://eanfs/multica/164; it does not satisfy the Linux security gate.

## Step 4 — published supply-chain artifacts

The publish job only runs on a push to `main` (`.github/workflows/aurora-sandbox.yml`, `publish` job gated on `github.repository == 'eanfs/multica' && github.ref == 'refs/heads/main'`).

History of main-branch runs relevant to this record:

| Run | Head | Verify | Publish | Verify published |
| --- | --- | --- | --- | --- |
| 36501577137 | `92daf28a4` | queued behind #167's run | will produce the final post-#140 digests | pending |
| 36501539027 | `c12025bc1` | queued | will produce the final post-#140 digests | pending |
| 36430260728 | `11b260cd3` | success | success | success |
| 36425249706 | `72176c950` | success | cancelled by the next merge | skipped |
| 36421002060 | `80da4cf76` | success | cancelled by concurrency | skipped |
| 36315998541 | `c60676bba` | failure | skipped | skipped |

The supply-chain trust chain is **verified end to end**. Run **36430260728** (head `11b260cd3`) completed all three jobs successfully: "Verify build and scan"; "Publish signed images", which pushed the sandbox and egress multi-architecture indexes, signed them keylessly with Cosign and attached SPDX and provenance attestations; and "Verify published signatures and attestations". The published index digests, taken from the uploaded artifact `aurora-sandbox-published/digest-references.txt`, are:

```text
ghcr.io/eanfs/multica-aurora-sandbox@sha256:ad7686beff5d606f2997818e16effdac251ff7d76e3b8d39eb170b7217483e01
ghcr.io/eanfs/multica-aurora-egress@sha256:720c76081c24fd2a0a33129f8acb0a576b1bf89403e4b658125ca0945e0698c4
```

This pair is the **pre-#140 image content**; it does not yet include the Seedance tree, so it is not presented as the final artifact. Run 36425249706's publish had been cancelled by the next merge, which is why no signature result had ever been recorded before run 36430260728. That cancellation is fixed: #167 (merged as `c12025bc1`) keys the workflow concurrency group on the ref and sets `cancel-in-progress: false` for push events, so a later merge to `main` no longer cancels an in-flight publish. Merges #167 and #168 therefore triggered the new main runs **36501539027** (`c12025bc1`) and **36501577137** (`92daf28a4`, queued behind it thanks to #167); their publish will produce the final digests that include the Seedance tree. Step 4 stays open until those digests are recorded.

What this host **could not** verify directly:

- `cosign` is not installed, so `cosign verify` and `cosign verify-attestation` cannot run locally.
- GHCR denies anonymous access from this host (`HTTP 403 invalid token` for the token exchange, and `docker manifest inspect ... denied`), and the local `gh` token has no package scope, so no published tag/index could be inspected or confirmed to exist locally.
- Signature and attestation trust therefore comes from run 36430260728's own `verify-published` job ("Verify published signatures and attestations"), which performs `cosign verify`, SPDX and provenance attestation verification, tag-to-digest equality, and architecture inspection for each image. That job is green for the pre-#140 pair.

## Step 5 — real agent and provider smoke status

Plan D Task 6 (#117) is **open and in progress, and is not authorized to run**.
No provider credentials exist in this environment, no credential file was
created, and no real provider or agent was contacted. Every provider subtest
remains **SKIPPED** with its gate reason; no real smoke has passed, and none is
claimed here. The gated suite is designed to skip each subtest at its own
opt-in gate before any executable lookup, credential read, Docker pull, or
network call.

| Provider subtest | Status | Gate reason |
| --- | --- | --- |
| Claude text (`AURORA_RUN_CLAUDE_SMOKE`) | **SKIPPED** | #117 open and unauthorized; no credential file |
| Seedream (`AURORA_RUN_SEEDREAM_SMOKE`) | **SKIPPED** | #117 open and unauthorized; no credential file |
| Seedance (`AURORA_RUN_SEEDANCE_SMOKE`) | **SKIPPED** | #117 open and unauthorized; no credential file |
| Volc ASR (`AURORA_RUN_VOLC_ASR_SMOKE`) | **SKIPPED** | #117 open and unauthorized; no credential file |
| OpenAI generation/edit (`AURORA_RUN_OPENAI_IMAGE_SMOKE`) | **SKIPPED** | #117 open and unauthorized; no credential file |
| HyperFrames/FFmpeg (`AURORA_RUN_HYPERFRAMES_SMOKE`) | **SKIPPED** | #117 open and unauthorized |
| Chromium resume (`AURORA_RUN_CHROMIUM_SMOKE`) | **SKIPPED** | #117 open and unauthorized |

These are recorded as **SKIPPED**, never as failure and never as success. The deterministic fake-provider matrix in Step 1 remains the required coverage.

## Step 6/8 — documentation checks and commits

- `git diff --check` is clean.
- The plan and status documents were updated only to the extent the evidence above supports: the Seedance vendor gate (#140, merged as pr://eanfs/multica/168 at `92daf28a4`) is recorded as resolved with its licence provenance and strengthened verifier, the video routes as executable, the main-branch publish as no longer cancellable (#167, `c12025bc1`) with the pre-#140 trust chain verified on run 36430260728, and the real-smoke gate (#117) as open, in progress, and unauthorized. Plan D Task 7 Step 4 stays open pending the post-#140 final digests, and the boundary is stated as **not closed**.
- No build artifact, acceptance report, credential, or secret is committed. `.scratch/` remains ignored.
