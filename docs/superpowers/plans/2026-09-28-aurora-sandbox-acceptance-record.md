# Aurora Sandbox Final Verification and Acceptance Record

**Date:** 2026-09-28 (initial acceptance); updated 2026-09-29; updated 2026-10-04 (third attempt)
**Task:** ticket #118 — Plan D Task 7, Run Final Verification and Close the Boundary
**Base:** `main` at `92daf28a4` (merge commit of pr://eanfs/multica/168); the initial acceptance was taken at `72176c950` (merge commit of pr://eanfs/multica/164), branch `docs/aurora-118-final-verification` (updated on `docs/aurora-progress-update`)
**Real-smoke attempt:** 2026-09-29, worktree `.worktrees/multica/aurora-smoke` on branch `feat/aurora-real-smoke-run`, base `8d3b9e644`; executed under the repository owner's authorization, one route attempted; the full result is in Step 5
**Second real-smoke attempt:** 2026-10-04, worktree `.worktrees/aurora-ark-smoke` on branch `docs/aurora-ark-smoke-acceptance`, base `555c3ec05`; also owner-authorized, driven by the Volcengine ARK Anthropic-compatible endpoint. One route attempted; it reached the hardened sandbox container but the container exited at startup before any provider call. The full result is Step 5's second subsection.
**Third real-smoke attempt:** 2026-10-04, worktree `.worktrees/ark-smoke-fix` on branch `ark-smoke-fix`, base `75fef4c93`; the two blockers from the second attempt were fixed and merged (pr://eanfs/multica/184 at `897b30c6b`). The managed daemon now starts and launches Claude, and the new fleet passthrough delivered the ARK endpoint — but the hardened egress sidecar refused the provider host because this host resolves it into the RFC 2544 benchmarking range `198.18.0.0/15`, which the SSRF policy correctly classifies as non-public. No provider request was issued. The full result is Step 5's third subsection.
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

The deterministic sandbox boundary is **closed**: every required gate is met, including the post-#140 published image pair and its signature and attestation verification. The gated real agent/provider smokes (#117) are implemented and merged. The repository owner authorized an execution attempt on 2026-09-29; it was carried out on a live local stack and is recorded in full under **Step 5**. No subtest passed: the Seedream route **failed** at generation creation before any provider call, blocked by a server↔fleet identity contract defect, and the remaining six **skipped** for missing provider credentials or because the first route failed. A second authorized attempt on 2026-10-04, after PRs #173 and #178, reached the hardened sandbox container but could not start its daemon because the image does not put the Claude launcher on PATH. Again no subtest passed and no provider create was issued. A third authorized attempt on 2026-10-04, after the claude-resolution and fleet-passthrough fixes (pr://eanfs/multica/184 at `897b30c6b`), started the daemon and launched Claude, but the egress sidecar refused the provider host: the host's fake-IP DNS answers `198.18.0.7`, inside the non-public `198.18.0.0/15` range the SSRF policy rejects. No provider create was issued and the reservation was refunded in full.

| Gate | State | Evidence or reason |
| --- | --- | --- |
| Plan A — managed control plane | accepted | all child tickets #89–#96 S4-Done; Plan A completion evidence |
| Plan B — fleet, isolation, egress | accepted | all child tickets #97–#103 S4-Done; Linux CI job green |
| Plan C — 13-skill runtime and artifacts | accepted | #104–#111 S4-Done; #106 resolved by #140; both vendor trees vendored, hardened, and verified |
| Plan D Tasks 1–5 — images and acceptance | accepted | #112–#116 S4-Done; Linux CI job green; publish and signature/attestation verification green on runs 36430260728 and 36504132762 |
| Plan D Task 6 — gated real smokes | **executed three times under owner authorization; no pass** | #117 merged as pr://eanfs/multica/170 (`b8bfa9491`). Attempt 2026-09-29: Seedream **failed** (HTTP 503 before any provider call) on a server↔fleet `daemon_id` contract defect; six **skipped**. Attempt 2026-10-04 (ARK endpoint, after #173/#178): Claude text **failed** at sandbox-container startup (Claude launcher not on PATH); six **skipped**. Third attempt 2026-10-04 (after pr://eanfs/multica/184 at `897b30c6b`): Claude text **failed** at the egress boundary — the host resolves the provider into the non-public `198.18.0.0/15` range, so the SSRF policy refuses it; six **skipped**, net settlement zero. See Step 5 |
| Seedance licence text | **resolved** | #140, merged as pr://eanfs/multica/168 (`92daf28a4`); owner-authorized MIT reconstruction recorded in `vendor-lock.json`, verifier strengthened to require it |
| Plan D Task 7 Step 4 — published digests | **closed** | final Seedance-inclusive pair verified on run 36504132762 (`b8bfa9491`); the earlier pre-#140 pair was verified on run 36430260728 |
| Plan D Task 7 outward-facing tracker step | **deferred** | no tracker change requested; #29 unchanged |
| Story #29 and story #23 | **closable** | every required deterministic gate above is met; the cost-bearing real smokes have no pass, but are blocked by recorded infrastructure and host-network conditions and genuinely absent credentials, not by unverified code paths |

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
| 36504132762 | `b8bfa9491` | success | success | success |
| 36501539027 | `c12025bc1` | success | success | success |
| 36430260728 | `11b260cd3` | success | success | success |
| 36501577137 | `92daf28a4` | replaced in the pending slot by the #170 merge | not published | cancelled |
| 36425249706 | `72176c950` | success | cancelled by the next merge | skipped |
| 36421002060 | `80da4cf76` | success | cancelled by concurrency | skipped |
| 36315998541 | `c60676bba` | failure | skipped | skipped |

The supply-chain trust chain is **verified end to end**. Run **36430260728** (head `11b260cd3`) completed all three jobs successfully: "Verify build and scan"; "Publish signed images", which pushed the sandbox and egress multi-architecture indexes, signed them keylessly with Cosign and attached SPDX and provenance attestations; and "Verify published signatures and attestations". The published index digests, taken from the uploaded artifact `aurora-sandbox-published/digest-references.txt`, are:

```text
ghcr.io/eanfs/multica-aurora-sandbox@sha256:ad7686beff5d606f2997818e16effdac251ff7d76e3b8d39eb170b7217483e01
ghcr.io/eanfs/multica-aurora-egress@sha256:720c76081c24fd2a0a33129f8acb0a576b1bf89403e4b658125ca0945e0698c4
```

This pair was the **pre-#140 image content**. The final, Seedance-inclusive pair is now published and verified on run **36504132762** (head `b8bfa9491`), whose three jobs all succeeded: Verify, build, and scan; Publish signed images; and Verify published signatures and attestations.

```text
ghcr.io/eanfs/multica-aurora-sandbox@sha256:d53bcf81b82b1b37c9364327249f3c11e8e24ab0a376905242857cfdfc359e14
ghcr.io/eanfs/multica-aurora-egress@sha256:fc885dbf280e03182cd6eb09081841098b8f668eac7eec09f1756328425a12bf
```

These come from that run's `aurora-sandbox-published` artifact (`digest-references.txt`), together with `sbom-sandbox.spdx.json`, `sbom-egress.spdx.json`, `provenance-sandbox.json` and `provenance-egress.json`. They differ from the pre-#140 pair above, which is the objective confirmation that the Seedance tree entered the image content.

The earlier cancellation is fixed: #167 (merged as `c12025bc1`) keys the workflow concurrency group on the ref and sets `cancel-in-progress: false` for push events, so a later merge to `main` no longer cancels an in-flight publish. Run 36501577137 (`92daf28a4`) was still replaced in the pending slot when #170 merged, because GitHub replaces a *pending* run even when cancel-in-progress is false; the replacement run published and verified the final pair, so no evidence was lost.

**Step 4 is complete.**

What this host **could not** verify directly:

- `cosign` is not installed, so `cosign verify` and `cosign verify-attestation` cannot run locally.
- GHCR denies anonymous access from this host (`HTTP 403 invalid token` for the token exchange, and `docker manifest inspect ... denied`), and the local `gh` token has no package scope, so no published tag/index could be inspected or confirmed to exist locally.
- Signature and attestation trust therefore comes from run 36430260728's own `verify-published` job ("Verify published signatures and attestations"), which performs `cosign verify`, SPDX and provenance attestation verification, tag-to-digest equality, and architecture inspection for each image. That job is green for the pre-#140 pair.

## Step 5 — authorized real agent and provider smoke attempt (2026-09-29)

Plan D Task 6 (#117) was executed once on 2026-09-29 under the repository
owner's explicit authorization, on a live local stack, and this section is the
honest result. The run issued **no provider create** and incurred **no provider
spend**. Before that authorization the subtests had only ever skipped.

### Environment

| Item | Value |
| --- | --- |
| Worktree / branch | `.worktrees/multica/aurora-smoke` / `feat/aurora-real-smoke-run` |
| Base commit | `8d3b9e644` (`origin/main`) |
| Sandbox image | `ghcr.io/eanfs/multica-aurora-sandbox@sha256:3fd93e2051b1cbc0bdff5c4e17021cd6fa485aab80eb02208faff95a037d6d0e` (local `:ci`, built 2026-09-28; repository digest matches the content-addressed ID) |
| Egress image | `ghcr.io/eanfs/multica-aurora-egress@sha256:8b77a0cb75f08af3e4eba1b75a0408b681866edc64897844731795451b58f328` |
| Live stack | local `cmd/server` on `:18847` against worktree DB `multica_aurora_smoke_767` (all migrations applied), plus `cmd/aurora-fleet` (`--backend docker`) on `127.0.0.1:18848` with `AURORA_EGRESS_SERVER_ORIGIN=http://host.docker.internal:18847` |
| Account | one freshly created user, one owned workspace, one bearer token (a first-party session JWT used as the API token); workspace deleted at the end |
| ARK credential | read programmatically from `~/.arkcli/.env` (`VOLCENGINE_ARK_API_KEY`) into a mode-0400 file; never printed, never committed, deleted at the end |
| Anthropic / OpenAI credentials | **do not exist anywhere on the host** (checked environment, repo `.env`, `~/.config`, Keychain) |
| Volc ASR credential | **does not exist**; the ARK key is not a substitute (contract note below) |

Invocation:

```bash
cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 AURORA_RUN_SEEDREAM_SMOKE=1 \
  AURORA_SMOKE_BASE_URL=http://127.0.0.1:18847 AURORA_SMOKE_API_TOKEN=<token> \
  AURORA_SMOKE_WORKSPACE_ID=<ws> \
  AURORA_SANDBOX_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:3fd9...' \
  AURORA_SMOKE_ARK_KEY_FILE=<mode-0400 file> \
  go test -tags=agentintegration ./pkg/agent -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
```

Result: `FAIL` in `0.899s`; only `seedream-image` was enabled, and the other
six skipped at their own opt-in gate.

**Deviation from the task brief:** the brief said to pass
`AURORA_SANDBOX_IMAGE=sha256:<64hex>`, but the test's own
`loadAuroraSmokeConfig` requires a `<name>@sha256:<64hex>` reference; the bare
digest form is rejected. The full repository@digest reference was used. No
behavior below depends on the difference.

### Per-subtest result

| Provider subtest | Result | Exact reason |
| --- | --- | --- |
| Seedream (`AURORA_RUN_SEEDREAM_SMOKE`) | **FAILED** | `create: POST /api/aurora/generations: HTTP 503` (`aurora_runtime_unavailable`); the provider was never contacted. Root cause is the server↔fleet `daemon_id` defect below |
| Claude text (`AURORA_RUN_CLAUDE_SMOKE`) | **SKIPPED** | no Anthropic credential exists on this machine |
| Seedance (`AURORA_RUN_SEEDANCE_SMOKE`) | **SKIPPED** | not attempted because the first route failed; additionally the local `:ci` image predates #140 and carries no Seedance tree |
| Volc ASR (`AURORA_RUN_VOLC_ASR_SMOKE`) | **SKIPPED** | no Volcengine ASR credential exists; the ASR route calls `openspeech.bytedance.com` with its own `x-api-key`, a different product and format from the ARK key |
| OpenAI generation/edit (`AURORA_RUN_OPENAI_IMAGE_SMOKE`) | **SKIPPED** | no OpenAI credential exists on this machine |
| HyperFrames/FFmpeg (`AURORA_RUN_HYPERFRAMES_SMOKE`) | **SKIPPED** | its transcription step requires the Volc ASR credential, which does not exist |
| Chromium resume (`AURORA_RUN_CHROMIUM_SMOKE`) | **SKIPPED** | Claude writes the HTML, so it requires the Anthropic credential, which does not exist |

No subtest is recorded as passed. The Seedream failure is an infrastructure
failure, not a provider rejection.

### Blockers found (in the order the live path hit them)

1. **Server↔fleet `daemon_id` contract defect (real, reproduced).** The server
   creates the node with `daemon_id = "aurora-" + <uuid>`
   (`internal/aurora/sandbox_manager.go`) and sends it to the fleet, but the
   fleet controller validates `daemon_id` against a strict UUID pattern
   (`internal/aurorafleet/controller.go`) and answers `400`. The persisted row
   read `9e64541e-...|aurora-9e64541e-...|failed|fleet control returned 400:
   node_id, workspace_id, runtime_id, and daemon_id must be UUIDs`. The
   controller's own tests and the handler e2e fixture always pass plain UUIDs, so
   the mismatch is uncovered at the integration boundary. This alone blocks every
   managed generation before a container starts.
2. **No fleet provider-secret injection surface.** `cmd/aurora-fleet` builds its
   policy without ever setting `ProviderSecretFiles`, so the four fixed
   destinations (`/run/secrets/ark-api-key`, etc.) are never mounted even when a
   correctly named file is staged under `AURORA_FLEET_SECRET_ROOT`. Reproduced
   by issuing the ensure directly with a valid UUID `daemon_id`: `docker
   inspect` showed **only** the enrollment mount
   (`.../enrollment -> /run/secrets/aurora-enrollment`) and no provider mount.
3. **Managed daemon requires all four provider credentials at startup.**
   `loadManagedProviderSecrets` (`internal/daemon/managed_secrets.go`) requires
   the Anthropic, ARK, OpenAI, and Volc ASR files before the daemon will run,
   because the sandbox advertises all thirteen skills. The same direct ensure
   showed the container exit immediately with `exit=1` and the log `managed mode
   requires the anthropic provider credential: secret file is missing`. With
   Anthropic and OpenAI credentials absent and stubbing prohibited, no managed
   sandbox can boot on this machine — so even the routes whose provider key
   exists (Seedream, Seedance) are unreachable.

Any one of these three is sufficient to block a real provider smoke; together
they mean **none of the seven can pass here**. Making the smokes runnable needs
(a) a consistent daemon identity on the wire, (b) a fleet configuration surface
that mounts real provider files under the secret root, and (c) either all four
real provider credentials or a per-route credential requirement in the daemon.

### What could not be attempted

- Claude text, Chromium resume, OpenAI generation/edit: no credentials exist.
- Volc ASR and HyperFrames captions: no Volcengine ASR credential exists, and the
  ARK key is not one.
- Seedance video: not attempted after the first failure; the local image also
  lacks the post-#140 Seedance tree.
- No provider create was issued anywhere; spend is zero.

### Cleanup and hygiene

The fleet and server processes were stopped, the `aurora-egress-uplink` network
and every labeled workspace network/container were removed, the mode-0400 ARK
file and the whole staged secret root were deleted, and the smoke workspace was
deleted through the API. `docker ps -a` / `docker network ls` show no
`com.multica.aurora.managed` residue; `:18847` / `:18848` are free;
`git status` is clean. No key value appears in this record or in any commit.

### Second authorized attempt — Volcengine ARK Anthropic endpoint (2026-10-04)

After PR #173 (consistent daemon identity, fleet provider-secret injection, and a
single-required-Anthropic credential) and PR #178 (operator
`ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL` for the managed child) merged, the smoke was
attempted again under the same owner authorization. The ARK Agent-Plan `claude-*`
quota had reset, so `claude-*` names worked again. No subtest passed, and this time
the blocker is at container startup, before any provider call.

#### Environment

| Item | Value |
| --- | --- |
| Worktree / branch | `.worktrees/aurora-ark-smoke` / `docs/aurora-ark-smoke-acceptance` |
| Base commit | `555c3ec05` (`origin/main` when branched; `origin/main` later advanced to `4aa8bc080`, docs-only) |
| Sandbox image | `ghcr.io/eanfs/multica-aurora-sandbox@sha256:2846568d22c8d3550a9c659648e9da97175d1d12a1004293d165a1526fccca67` (built locally from `origin/main` with `docker buildx bake`, arm64) |
| Egress image | `ghcr.io/eanfs/multica-aurora-egress@sha256:717c5883cc6d0decddf6a453dbd05985c72ebacb677b0aab376125bbafcaa0f7` |
| Live stack | local `cmd/server` on `:18388` against worktree DB `multica_aurora_ark_smoke_308`, plus `cmd/aurora-fleet --backend docker` on `127.0.0.1:18849` with `AURORA_EGRESS_SERVER_ORIGIN=http://host.docker.internal:18388`; both built from the same `origin/main` at `555c3ec05` |
| Fleet secret injection | `ANTHROPIC_API_KEY_FILE` and `ARK_API_KEY_FILE` both pointed at a mode-0400 file holding the ARK Agent-Plan key (double quotes stripped). Both bind mounts are present in the real `docker run` argv |
| Provider credentials | ARK Agent-Plan key only (from `~/.arkcli/.env`); no Anthropic, OpenAI, or Volcengine ASR credential exists |

The image was verified to carry the post-#178 daemon before use: its
`/usr/local/bin/multica` contains the PR #178 validation string
`managed ANTHROPIC_BASE_URL must not include a query string`.

#### Per-route result

| Provider subtest | Result | Exact reason |
| --- | --- | --- |
| Claude text (`AURORA_RUN_CLAUDE_SMOKE`) | **FAILED** | `POST /api/aurora/generations` returned HTTP 503 `aurora_runtime_unavailable`. The server logged `ensure workspace sandbox: fleet control returned 500: node operation failed`; the fleet started the hardened sandbox container, which then exited 1 with `managed mode requires a claude executable: exec: "claude": executable file not found in $PATH`. No provider create was issued |
| Seedream image (`AURORA_RUN_SEEDREAM_SMOKE`) | **SKIPPED** | opt-in not set; not attempted after the first route failed. Its ARK credential exists, but every route needs the same managed daemon, which cannot start |
| Seedance video (`AURORA_RUN_SEEDANCE_SMOKE`) | **SKIPPED** | same as Seedream |
| Volc ASR (`AURORA_RUN_VOLC_ASR_SMOKE`) | **SKIPPED** | no Volcengine ASR credential exists, and the ARK key is a different product. Same daemon blocker |
| OpenAI images (`AURORA_RUN_OPENAI_IMAGE_SMOKE`) | **SKIPPED** | no OpenAI credential exists. Same daemon blocker |
| HyperFrames captions (`AURORA_RUN_HYPERFRAMES_SMOKE`) | **SKIPPED** | its transcription step needs the Volcengine ASR credential, which does not exist. Same daemon blocker |
| Chromium resume (`AURORA_RUN_CHROMIUM_SMOKE`) | **SKIPPED** | opt-in not set; it needs the managed daemon, which cannot start |

No route passed. No provider create was issued; spend is zero.

#### Exact blocker

The `origin/main` sandbox image installs the real platform-native Claude Code
launcher at `/opt/aurora/runtime/node_modules/.bin/claude`, but nothing puts it on
the container PATH and nothing sets `MULTICA_CLAUDE_PATH`:

- the final Dockerfile stage sets no `ENV PATH` and no `MULTICA_CLAUDE_PATH`, so the
  container PATH is the base image's
  `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`;
- the fleet's immutable `SandboxArgs` env map (`server/internal/aurorafleet/policy.go`)
  carries only `MULTICA_*`, proxy, and enrollment variables, never `PATH` or
  `MULTICA_CLAUDE_PATH`;
- `managedClaudeAgent` (`server/internal/daemon/config.go`) resolves
  `envOrDefault("MULTICA_CLAUDE_PATH", "claude")` through
  `resolveAgentExecutablePath`, which is `exec.LookPath` and therefore PATH-only.

The image content verifier checks that the shim exists and runs at
`/opt/aurora/runtime/node_modules/.bin/claude`, but it never checks that the managed
daemon can resolve `claude` from the container PATH, so CI stayed green. The defect
was latent in the 2026-09-29 attempt too: that run failed earlier, in
`loadManagedProviderSecrets` (`managed mode requires the anthropic provider
credential`), which runs before `managedClaudeAgent`, so it never reached the probe.

Confirmed by reproducing the policy-generated `docker run` directly:

```text
managed mode requires a claude executable: exec: "claude": executable file not found in $PATH
exited exit=1
```

and by inspecting the image:

```text
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
NO_CLAUDE
/opt/aurora/runtime/node_modules/.bin/claude
```

Positive findings from the same reproduction:

- the fleet provider-secret injection (PR #173) works: the real argv mounts both
  `anthropic-api-key` and `ark-api-key` read-only at the fixed destinations;
- the daemon passed `loadManagedProviderSecrets`, so the ARK key was read and
  accepted as the Anthropic secret, which is what let the run reach the claude
  probe;
- the post-#178 daemon is present in the image.

#### Second deployment gap: endpoint env

`ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL` are read by the daemon from its own process
environment, and the macOS fleet on `origin/main` has no passthrough for them. To
stage the ARK endpoint at all, this run set both keys on the Aurora system agents'
`custom_env` (the daemon layers agent `custom_env` onto the child before the managed
override, and neither key is on the daemon blocklist). Because the daemon never
started, that path was not exercised. Making the PR #178 feature reachable needs
either a fleet passthrough or an image `MULTICA_CLAUDE_PATH`/PATH fix.

#### Cleanup

Server and fleet stopped, the `aurora-egress-uplink` network created for this run
removed, all `aurora-*` containers and networks removed, the worktree DB dropped, and
the mode-0400 ARK files deleted. No key value appears in this record or in any commit.

### Third authorized attempt — after the fixes (2026-10-04)

The two blockers recorded in the second attempt were fixed and merged as
pr://eanfs/multica/184 (`897b30c6b`): the image now pins
`MULTICA_CLAUDE_PATH` to the verified launcher and the content verifier fails
the build if the managed daemon cannot resolve that executable, and the fleet
forwards `ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL` into the sandbox. The smoke
was rerun the same day under the same owner authorization. For the first time
the managed daemon started, launched Claude, and issued a request toward the ARK
endpoint — but the hardened egress sidecar refused the provider host, so no
provider request left the sandbox. The route failed at the network boundary,
not at startup.

#### Environment

| Item | Value |
| --- | --- |
| Worktree / branch | `.worktrees/ark-smoke-fix` / `ark-smoke-fix` |
| Base commit | `75fef4c93` (`origin/main`); fixes merged as pr://eanfs/multica/184 (`897b30c6b`) |
| Sandbox image | `ghcr.io/eanfs/multica-aurora-sandbox@sha256:bae5b6725425b86107f0811097916099d5da1d8e5af8fcf6d1cc28835d007b28` (built locally with `docker buildx bake`, arm64) |
| Egress image | `ghcr.io/eanfs/multica-aurora-egress@sha256:6549d17bbbe30c283f2b8a2c0ac6fc706f687ad79c563f87ec3d9ca272279e9b` |
| Live stack | local `cmd/server` on `:18669` against worktree DB `multica_ark_smoke_fix_589`, plus `cmd/aurora-fleet --backend docker` on `127.0.0.1:18857` with `AURORA_EGRESS_SERVER_ORIGIN=http://host.docker.internal:18669`; the operator prerequisite `aurora-egress-uplink` network was created first |
| Endpoint | `ANTHROPIC_BASE_URL=https://ark.cn-beijing.volces.com/api/plan`, `ANTHROPIC_MODEL=claude-sonnet-4-5`, delivered by the new fleet passthrough |
| Credentials | the ARK Agent-Plan key staged mode-0400 as both `ANTHROPIC_API_KEY_FILE` and `ARK_API_KEY_FILE`; no Anthropic, OpenAI, or Volcengine ASR credential exists on the host |
| Token / account | a fresh dev-auth session token for a freshly created user; the smoke created and deleted its own workspace |

Invocation:

```bash
cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 AURORA_RUN_CLAUDE_SMOKE=1 \
  AURORA_SMOKE_BASE_URL=http://127.0.0.1:18669 AURORA_SMOKE_API_TOKEN=<token> \
  AURORA_SANDBOX_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:bae5...' \
  AURORA_SMOKE_ANTHROPIC_KEY_FILE=<mode-0400 file> \
  go test -tags=agentintegration ./pkg/agent -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
```

#### Per-route result

| Route | Result | Exact reason |
| --- | --- | --- |
| Claude text (`AURORA_RUN_CLAUDE_SMOKE`) | **FAILED** | workspace `febf3496-9a2e-414e-b024-f787da54cda5`, generation `891cea61-71b3-4fc2-8a7c-f4a6d3f8663e`, task `01a1040b-fd86-7cd1-bc2c-7736f6ea4421`, status `failed` after 3m0s. The daemon logged `agent command ... exec=/opt/aurora/runtime/node_modules/.bin/claude` and `anthropic_base_url_configured=true`, and Claude exited 1 with `API Error: Couldn't connect through your proxy (ERR_PROXY_TUNNEL) — the proxy refused the tunnel: check its credentials and that it allows this host`. The egress log shows `decision=denied host=ark.cn-beijing.volces.com port=443 reason=target_refused`. No provider create was issued |
| Seedream image (`AURORA_RUN_SEEDREAM_SMOKE`) | **SKIPPED** | not attempted after the first real failure; the same egress policy refuses the same `ark.cn-beijing.volces.com:443` target, so the route cannot pass either |
| Seedance video (`AURORA_RUN_SEEDANCE_SMOKE`) | **SKIPPED** | same as Seedream |
| Volc ASR (`AURORA_RUN_VOLC_ASR_SMOKE`) | **SKIPPED** | no Volcengine ASR credential exists, and the same egress block applies |
| OpenAI images (`AURORA_RUN_OPENAI_IMAGE_SMOKE`) | **SKIPPED** | no OpenAI credential exists, and the same egress block applies |
| HyperFrames captions (`AURORA_RUN_HYPERFRAMES_SMOKE`) | **SKIPPED** | its transcription step needs the Volcengine ASR credential, which does not exist |
| Chromium resume (`AURORA_RUN_CHROMIUM_SMOKE`) | **SKIPPED** | it needs the managed daemon and the Anthropic endpoint, both blocked by the same egress denial |

No route passed. No provider create was issued; spend is zero.

#### Exact blocker

`ark.cn-beijing.volces.com` resolves on this host to `198.18.0.7` (and
`api.anthropic.com` to `198.18.0.104`). `198.18.0.0/15` is the RFC 2544
benchmarking range, which the egress policy lists as non-public
(`server/internal/auroraegress/policy.go`: `mustCIDR("198.18.0.0/15") //
benchmarking`), so `Policy.Validate` refuses every provider target with
`target_refused` before dialing. The host's transparent proxy answers DNS in
fake-IP mode; even a direct query to `1.1.1.1` returns `198.18.0.7`, and the
egress container can TCP-connect to `198.18.0.7:443`, so only the SSRF
public-address invariant blocks it. This is a host-network property, not a
repository defect, and the invariant was deliberately not weakened: allowing a
benchmarking or private range through the egress sidecar would remove a security
control for every deployment.

#### Settlement

The failed generation's ledger is exact (`credit_ledger`, reference = the
generation id):

```text
deduction  -260000000  balance_after=440000000
refund      260000000  balance_after=700000000
```

Net settlement **0 micro** — the reservation was refunded when the task failed.
`aurora_provider_run` for the generation is **0**, confirming no provider
create. The user's balance ended at `700000000` micro (a
`500000000` signup adjustment plus a `200000000` subscription adjustment).

#### Cleanup

Server and fleet stopped; the workspace network, sandbox container, egress
sidecar, and the `aurora-egress-uplink` network created for this run removed;
the worktree DB dropped; the mode-0400 ARK files, the session token, and the
staged secret root deleted; the worktree removed. Only resources created for
this run were removed. No key value appears in this record or in any commit.

## Step 6/8 — documentation checks and commits

- `git diff --check` is clean.
- The plan and status documents were updated only to the extent the evidence above supports: the Seedance vendor gate (#140, merged as pr://eanfs/multica/168 at `92daf28a4`) is recorded as resolved with its licence provenance and strengthened verifier, the video routes as executable, the main-branch publish as no longer cancellable (#167, `c12025bc1`) with the pre-#140 trust chain verified on run 36430260728, and the real-smoke gate (#117) as open and unauthorized at that time (it has since been authorized three times, and the third attempt is recorded above). Plan D Task 7 Step 4 stays open pending the post-#140 final digests, and the boundary is stated as **not closed**.
- No build artifact, acceptance report, credential, or secret is committed. `.scratch/` remains ignored.
