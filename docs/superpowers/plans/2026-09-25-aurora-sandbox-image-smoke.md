# Aurora Sandbox Image and Smoke Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Package the managed daemon and 13-skill runtime into reproducible, digest-only Linux images, publish SBOM/provenance/signatures, and prove the functional and security boundary with fake-by-default and explicitly gated real-provider smoke tests.

**Architecture:** A multi-stage Docker build compiles the Go daemon/proxy, installs exact Node runtime dependencies, applies the audited Volcengine patch series, and copies only runtime files into non-root final images based on digest-pinned Node/Debian inputs. CI validates source locks, builds `linux/amd64` and `linux/arm64`, scans, emits SPDX/provenance, signs immutable digests, and never exposes provider credentials. Linux acceptance runs the actual image through child plan B’s security harness; macOS Docker Desktop runs only a functional fake smoke; cost-bearing provider/Claude smokes live behind both the repository’s real-agent gate and provider-specific opt-ins.

**Tech Stack:** Docker Buildx/OCI, Node.js 22, Go 1.26.6, Claude Code 2.1.282, MCP SDK 1.30.1, OpenAI 7.23.0, HyperFrames 0.8.75, FFmpeg, ffprobe, Chromium, Poppler, ImageMagick, Syft, Trivy, Cosign, GitHub Actions

**Spec:** `docs/superpowers/specs/2026-09-11-aurora-content-creation-app-design.md`

## Global Constraints

- This plan is child plan D of `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md`; it integrates only merged and verified outputs from child plans A–C.
- Linux Docker Engine on `linux/amd64` and `linux/arm64` is the release target. Docker Desktop on macOS is a functional developer smoke only.
- Fleet execution always uses `repository/image@sha256:<digest>`. Tag-only, uppercase, truncated, or platform-manifest references are rejected.
- The sandbox final image runs as UID/GID `10001:10001`, declares no volume, exposes no public port, and contains no provider credential, enrollment token, daemon token, task token, user data, generated output, Git metadata, package-manager cache, or build secret.
- BuildKit secrets may authenticate private registries during build, but no secret may appear in `ARG`, `ENV`, `RUN` command text, layer history, OCI labels, SBOM properties, provenance parameters, or test logs.
- Runtime provider credentials are read-only host files mounted by fleet at fixed paths. The image and control API accept only paths, never raw values.
- Exact runtime secret paths are `/run/secrets/anthropic-api-key`, `/run/secrets/ark-api-key`, `/run/secrets/openai-api-key`, and `/run/secrets/volc-asr-api-key`.
- The daemon passes the Anthropic value only to the Claude child process. The MCP broker reads the other three files only in the corresponding provider adapter.
- The final image does not contain `skills`, `npx`, `pnpm`, `npm`, Git, curl, wget, SSH, or a general-purpose runtime package installer. Node remains only because the reviewed broker, provider adapters, Claude CLI, and HyperFrames need it.
- Default CI and local checks use fake providers/fake Claude and do not require account access or consume credits.
- Real-agent smoke requires `MULTICA_RUN_REAL_AGENT_SMOKE=1`, build tag `agentintegration`, a specific `go test -run` target, and one additional explicit variable for each paid provider route.
- Issue #29 and roadmap completion happen only after required functional, Linux security, and supply-chain gates pass; a successful macOS or paid-provider smoke alone is insufficient.

## Locked Inputs

The initial lock must contain these values exactly:

| Component | Locked value |
| --- | --- |
| Go toolchain | `1.26.6` |
| Go builder image | `golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36` |
| Node runtime image | `node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c` |
| Claude Code | `@anthropic-ai/claude-code@2.1.282` |
| MCP SDK | `@modelcontextprotocol/sdk@1.30.1` |
| OpenAI SDK | `openai@7.23.0` |
| HyperFrames npm | `hyperframes@0.8.75` |
| HyperFrames source | tag `v0.8.75`, commit `a95cb96a5dd3c1f7b31266a4b470590c86ad231f`, Apache-2.0 |
| Skills update CLI | `skills@1.7.0` (update script only, not final image) |
| Seedance source | child plan C vendor lock, version `5.0.0` |
| Seedream source | child plan C vendor lock, version `4.0.0` |
| Debian packages | `snapshot.debian.org` timestamp `20260925T000000Z`, exact versions recorded in `apt-packages.lock` |

A dependency update is a separate reviewed change that updates the lock, source inventory/SBOM, scans, and smoke evidence together.

## Runtime Package Set

The Debian snapshot lock contains only:

```text
ca-certificates
chromium
ffmpeg
fonts-noto-cjk
fonts-noto-color-emoji
imagemagick
libnss3
poppler-utils
tini
unzip
```

Do not install compilers, shells beyond the base image’s required `/bin/sh`, Python, Git, network download CLIs, or desktop services in the final stage. After installation, remove apt/dpkg caches and package-manager frontends not required for runtime. The container policy still prevents the model from invoking `/bin/sh` or Node directly.

## File Structure

### New files

- `deploy/aurora-sandbox/versions.json` — machine-readable exact tool/base/vendor versions and digests.
- `deploy/aurora-sandbox/apt-packages.lock` — Debian snapshot package/version/architecture lock.
- `deploy/aurora-sandbox/Dockerfile` — multi-stage managed sandbox image.
- `deploy/aurora-sandbox/Dockerfile.egress` — minimal egress proxy image.
- `deploy/aurora-sandbox/docker-bake.hcl` — amd64/arm64 targets, OCI metadata, cache policy, and output names.
- `deploy/aurora-sandbox/.dockerignore` — deny-all then allow exact build inputs.
- `deploy/aurora-sandbox/README.md` — build, local fake smoke, digest deployment, and platform boundary.
- `deploy/aurora-sandbox/fixtures/{input,image,audio,video,document}/**` — small redistributable fake/real smoke inputs.
- `scripts/update-aurora-sandbox-apt-lock.sh` — resolve exact package versions from the dated snapshot in a disposable build container.
- `scripts/verify-aurora-sandbox-locks.mjs` — cross-check Dockerfiles, package lock, Go, vendor lock, base digests, and apt lock.
- `scripts/verify-aurora-sandbox-image.sh` — inspect image content/history/user/size/secret absence and run in-image self-test.
- `server/internal/daemon/managed_secrets.go` / `_test.go` — fixed secret-file loading and child-process scoping.
- `server/pkg/agent/aurora_sandbox_smoke_test.go` — `agentintegration` real smoke orchestrator.
- `.github/workflows/aurora-sandbox.yml` — test, build, scan, SBOM, provenance, publish, and sign.
- `.github/aurora-sandbox-vex.json` — structured, expiring VEX entries for any accepted unfixed finding.

### Modified files

- `deploy/aurora-sandbox/runtime/package.json` — add exact Claude Code package and production scripts.
- `pnpm-lock.yaml` — lock runtime dependency graph.
- `server/internal/daemon/config.go` — fixed managed secret file paths and startup validation.
- `server/internal/daemon/daemon.go` — pass Anthropic secret only to Claude and provider paths only to MCP broker.
- `server/internal/aurorafleet/policy.go` / `_test.go` — mount exact provider secret files read-only without putting values in inspect output.
- `server/internal/aurorafleet/docker_integration_test.go` — execute release image and verify secret mount/value handling.
- `deploy/aurora-sandbox/docker-smoke.sh` — use the built release image for macOS fake smoke.
- `deploy/aurora-sandbox/docker-security-test.sh` — use the built release image for Linux acceptance.
- `AGENTS.md` — close the Aurora external-infrastructure boundary only after final acceptance.
- `docs/superpowers/plans/2026-09-11-aurora-execution.md` — mark Task 6 complete only after final acceptance.
- `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md` — record digest/evidence and completion state.

## Image Entrypoints

Sandbox command is fixed in the image:

```text
/usr/local/bin/multica daemon start \
  --managed \
  --foreground \
  --managed-enrollment-token-file=/run/secrets/aurora-enrollment
```

The fleet cannot override it. The image health check calls a small non-shell command:

```text
/usr/local/bin/multica daemon managed-healthcheck --url=http://127.0.0.1:19514/health --max-age=90s
```

Egress image command is fixed:

```text
/usr/local/bin/aurora-egress-proxy
```

---

### Task 1: Lock Every Image Input and Detect Drift

**Files:**
- Create: `deploy/aurora-sandbox/versions.json`
- Create: `deploy/aurora-sandbox/apt-packages.lock`
- Create: `scripts/update-aurora-sandbox-apt-lock.sh`
- Create: `scripts/verify-aurora-sandbox-locks.mjs`
- Modify: `deploy/aurora-sandbox/runtime/package.json`
- Modify: `pnpm-lock.yaml`

**Interfaces:**
- Consumes: The Locked Inputs table and child plan C vendor lock.
- Produces: One machine-checkable dependency/base/package lock consumed by Docker and CI.

- [ ] **Step 1: Write the drift verifier tests first**

Use temporary fixture locks to prove rejection of a mutable base tag, mismatched Go version, semver range, changed HyperFrames commit, missing vendor tree hash, unpinned apt package, changed snapshot date, and unexpected production dependency.

```js
await assert.rejects(() => verify({ nodeBase: "node:22-bookworm-slim" }), /digest/);
await assert.rejects(() => verify({ claudeCode: "^2.1.282" }), /exact version/);
```

- [ ] **Step 2: Run verifier tests and observe missing lock**

Run:

```bash
node --test scripts/verify-aurora-sandbox-locks.test.mjs
```

Expected: test import fails because the verifier/lock does not exist.

- [ ] **Step 3: Write `versions.json` from the exact table**

Include schema version, retrieval date `2026-09-25`, both base refs/digests, package versions, HyperFrames source tag/commit/license, skills updater version, vendor tree hashes imported from child plan C, and the Debian snapshot timestamp. Do not duplicate provider credentials or deployment values.

- [ ] **Step 4: Pin production Node dependencies**

Add `@anthropic-ai/claude-code: 2.1.282` to the sandbox runtime package. Keep every version exact and regenerate the root pnpm lock with the repository’s package manager. Verify HyperFrames’s installed package metadata declares Node `>=22` and Apache-2.0.

- [ ] **Step 5: Generate the Debian package lock in a disposable container**

The update script rewrites sources to:

```text
deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260925T000000Z bookworm main
deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20260925T000000Z bookworm-security main
```

It resolves the Runtime Package Set for `amd64` and `arm64`, records exact `package=version` plus repository/SHA-256, and fails if either architecture lacks the same functional package set. The normal Docker build consumes the committed lock and never asks apt for “latest.”

- [ ] **Step 6: Implement cross-lock verification**

The verifier compares Go `go.mod`, runtime `package.json`, `pnpm-lock.yaml`, Docker `FROM` refs, apt install arguments, vendor locks, and versions JSON. It rejects an executable vendor tree without license hash or a Dockerfile network fetch for skills/HyperFrames source.

- [ ] **Step 7: Run lock verification**

Run:

```bash
pnpm install --frozen-lockfile
node scripts/verify-aurora-sandbox-locks.mjs
node scripts/verify-aurora-volc-skills.mjs
```

Expected: all commands pass.

- [ ] **Step 8: Commit locks**

```bash
git add deploy/aurora-sandbox/versions.json deploy/aurora-sandbox/apt-packages.lock \
  scripts/update-aurora-sandbox-apt-lock.sh scripts/verify-aurora-sandbox-locks.mjs \
  scripts/verify-aurora-sandbox-locks.test.mjs deploy/aurora-sandbox/runtime/package.json \
  pnpm-lock.yaml
git commit -m "build(aurora): lock sandbox image inputs"
```

### Task 2: Build Minimal Non-Root Sandbox and Proxy Images

**Files:**
- Create: `deploy/aurora-sandbox/Dockerfile`
- Create: `deploy/aurora-sandbox/Dockerfile.egress`
- Create: `deploy/aurora-sandbox/docker-bake.hcl`
- Create: `deploy/aurora-sandbox/.dockerignore`
- Create: `server/internal/daemon/managed_secrets.go`
- Create: `server/internal/daemon/managed_secrets_test.go`
- Modify: `server/internal/daemon/config.go`
- Modify: `server/internal/daemon/daemon.go`
- Modify: `server/internal/aurorafleet/policy.go`
- Modify: `server/internal/aurorafleet/policy_test.go`

**Interfaces:**
- Consumes: Child plans A–C, version locks, source/patch locks, and fixed secret paths.
- Produces: `aurora-sandbox` and `aurora-egress` OCI images for two architectures.

- [ ] **Step 1: Write managed-secret tests first**

Cover missing, symlink, non-regular, group/world-readable, oversized, empty, newline-trimmed, and valid secret files. Assert only the Claude child environment receives `ANTHROPIC_API_KEY`; daemon logs, MCP environment, and Docker policy never contain its value.

- [ ] **Step 2: Run focused tests and observe missing loader/mounts**

Run:

```bash
cd server && go test ./internal/daemon ./internal/aurorafleet -run 'TestManagedSecret|TestProviderSecretMount' -count=1
```

Expected: missing loader and provider mounts.

- [ ] **Step 3: Implement fixed secret-file validation**

Managed startup requires enrollment and Anthropic files. Ark, OpenAI, and ASR files are required because all 13 skills are advertised; a missing provider file makes readiness false and returns a provider-specific startup error without printing a path value or secret. Files must be regular, non-symlink, no larger than 16 KiB, and have no group/other permission bits.

Pass only:

```text
ANTHROPIC_API_KEY=<value>       to Claude child
ARK_API_KEY_FILE=/run/secrets/ark-api-key             to MCP broker
OPENAI_API_KEY_FILE=/run/secrets/openai-api-key       to MCP broker
VOLC_ASR_API_KEY_FILE=/run/secrets/volc-asr-api-key   to MCP broker
```

The model-visible context contains none of these names or values.

- [ ] **Step 4: Create a multi-stage sandbox Dockerfile**

Stages:

1. Go builder from the locked Go digest compiles static `multica` for target architecture with VCS metadata disabled and runs focused Go tests.
2. Node dependency stage from the locked Node digest runs `pnpm fetch --frozen-lockfile`, then offline production install for the runtime package.
3. Vendor stage verifies upstream locks, applies patch files to a separate runtime tree, reruns vendor security tests, and writes patched-tree hash.
4. Final stage from the locked Node digest installs exact apt-lock versions from the dated snapshot, copies daemon/broker/node modules/templates/patched vendor/policies, removes package managers/caches, creates UID/GID 10001, and sets root-owned non-writable permissions.

Use `COPY --chown=0:0`; no secret `ARG` or `ENV`; set `USER 10001:10001`, `WORKDIR /workspace`, fixed `ENTRYPOINT`, fixed `CMD`, and the non-shell health check.

- [ ] **Step 5: Create a minimal egress Dockerfile**

Copy only the static Go proxy binary and CA certificates into a distroless/static or scratch-compatible final image, run as 10001, and use the fixed proxy command. If CA files require a Debian final stage, pin that base by digest in `versions.json` and subject it to the same scan/SBOM rules.

- [ ] **Step 6: Add deny-all build context and bake targets**

`.dockerignore` starts with `**` and re-allows only exact Go module/build inputs, runtime package/lock, vendor locks/trees/patches, policies, and Docker files. Bake targets define `sandbox` and `egress` for `linux/amd64,linux/arm64`, OCI source/revision/licenses labels, deterministic epoch, registry cache, provenance `mode=max`, and SBOM enabled.

- [ ] **Step 7: Mount provider files without exposing values**

Fleet policy accepts host paths only from its configured secret root and mounts them read-only to the four fixed destinations. The API cannot choose those paths. `docker inspect` shows destinations/source paths but no values; image history contains neither.

- [ ] **Step 8: Build and run in-image self-tests**

Run:

```bash
docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl sandbox egress --load
node scripts/verify-aurora-sandbox-locks.mjs
```

For a multi-platform builder that cannot `--load` both platforms, build/load the host platform first, then run a separate `--platform linux/amd64,linux/arm64 --output=type=oci` build. Expected: both target builds pass.

- [ ] **Step 9: Commit images**

```bash
git add deploy/aurora-sandbox/Dockerfile deploy/aurora-sandbox/Dockerfile.egress \
  deploy/aurora-sandbox/docker-bake.hcl deploy/aurora-sandbox/.dockerignore \
  server/internal/daemon/managed_secrets.go server/internal/daemon/managed_secrets_test.go \
  server/internal/daemon/config.go server/internal/daemon/daemon.go \
  server/internal/aurorafleet/policy.go server/internal/aurorafleet/policy_test.go
git commit -m "build(aurora): add managed sandbox images"
```

### Task 3: Verify Image Contents and Run Containerized Fake End-to-End Smoke

**Files:**
- Create: `scripts/verify-aurora-sandbox-image.sh`
- Create: `deploy/aurora-sandbox/fixtures/input/image/reference.png`
- Create: `deploy/aurora-sandbox/fixtures/input/audio/short.wav`
- Create: `deploy/aurora-sandbox/fixtures/input/video/short.mp4`
- Create: `deploy/aurora-sandbox/fixtures/input/document/resume.md`
- Modify: `server/internal/aurorafleet/docker_integration_test.go`
- Modify: `deploy/aurora-sandbox/docker-security-test.sh`

**Interfaces:**
- Consumes: Actual host-platform release images and child plans B/C fake servers.
- Produces: Content verification and actual-container generation evidence without external providers.

- [ ] **Step 1: Write a failing image verifier**

The script accepts sandbox/proxy digest refs and asserts:

- configured user/entrypoint/health check/architecture;
- image size under 4 GiB per architecture;
- required binaries and fixed versions;
- vendor patched-tree hash matches lock;
- no forbidden package manager/download/SSH/Git binary;
- no writable root-owned runtime directory for UID 10001;
- no token/key pattern in files, config, labels, environment, or `docker history --no-trunc`;
- no `.git`, npm/pnpm cache, temporary vendor update project, test credentials, or generated artifact.

- [ ] **Step 2: Run it against a pre-image state**

Run:

```bash
scripts/verify-aurora-sandbox-image.sh \
  multica-aurora-sandbox:local multica-aurora-egress:local
```

Expected before final image adjustments: FAIL on the first unmet content invariant.

- [ ] **Step 3: Add redistributable minimal fixtures**

Generate deterministic tiny image/audio/video/document fixtures, record license/source in `deploy/aurora-sandbox/fixtures/README.md`, and add SHA-256 values. Fixtures contain no person, voice identity, customer data, or proprietary content.

- [ ] **Step 4: Run representative pipelines inside the actual container**

Extend the `auroradocker` integration to start fake Anthropic, Ark, OpenAI, ASR, storage, and moderation endpoints behind the egress policy and execute:

- `xhs-image` through fake Seedream/import/manifest/report;
- `text-video` through fake Seedance create/poll/import;
- `video-captions` through fake ASR plus real HyperFrames/FFmpeg;
- `resume` through fake Claude plus real Chromium PDF;
- one provider failure proving refund/no fallback.

The 13-route pure broker/server matrix remains child plan C’s canonical breadth test; this task proves the high-risk binaries and network/artifact seams in the real image.

- [ ] **Step 5: Run image and Linux container verification**

Run on Linux:

```bash
scripts/verify-aurora-sandbox-image.sh \
  multica-aurora-sandbox:local multica-aurora-egress:local
cd server && AURORA_RUN_DOCKER_SECURITY_TEST=1 go test -tags=auroradocker ./internal/aurorafleet \
  -run 'TestDockerSandboxLinuxSecurityBoundary|TestDockerSandboxFakeAuroraPipelines' -count=1 -v
```

Expected: verifier and both integration tests pass; no external hostname is contacted.

- [ ] **Step 6: Commit verification and fixtures**

```bash
git add scripts/verify-aurora-sandbox-image.sh deploy/aurora-sandbox/fixtures \
  server/internal/aurorafleet/docker_integration_test.go \
  deploy/aurora-sandbox/docker-security-test.sh
git commit -m "test(aurora): smoke the sandbox image"
```

### Task 4: Publish SBOM, Provenance, Scan Results, and Signatures

**Files:**
- Create: `.github/workflows/aurora-sandbox.yml`
- Create: `.github/aurora-sandbox-vex.json`
- Modify: `scripts/verify-aurora-sandbox-locks.mjs`
- Modify: `deploy/aurora-sandbox/README.md`

**Interfaces:**
- Consumes: Tasks 1–3 and GitHub’s OIDC/GHCR permissions.
- Produces: Multi-architecture GHCR digests, SPDX JSON, SLSA provenance, Trivy reports, and Cosign keyless signatures/attestations.

- [ ] **Step 1: Add a workflow-policy test**

The verifier must parse workflow YAML and reject mutable action tags, unpinned base images, missing least-privilege permissions, secret interpolation into build args, tag-only fleet examples, missing scan/SBOM/provenance/sign steps, or `pull_request_target`.

- [ ] **Step 2: Run the verifier and observe missing workflow**

Run:

```bash
node scripts/verify-aurora-sandbox-locks.mjs --workflow
```

Expected: FAIL with `missing aurora sandbox workflow`.

- [ ] **Step 3: Add PR verification jobs**

On pull requests touching sandbox/runtime/fleet files:

1. checkout with credentials disabled;
2. verify locks/vendor/licenses/patches;
3. run Go and Node focused tests;
4. build host-platform sandbox/proxy without push;
5. run image verifier and fake container smoke on Linux;
6. generate SPDX JSON with Syft;
7. scan filesystem and image with Trivy;
8. upload reports as workflow artifacts with a 30-day retention.

Pin every GitHub Action to a full commit SHA. The update PR body may comment the human-readable release tag, but workflow syntax uses only SHAs.

- [ ] **Step 4: Add protected-main publish jobs**

On push to `main` in `eanfs/multica` only, grant `packages:write`, `id-token:write`, `attestations:write`, and `contents:read`; build/push both architectures to:

```text
ghcr.io/eanfs/multica-aurora-sandbox
ghcr.io/eanfs/multica-aurora-egress
```

Generate OCI index digests, BuildKit provenance `mode=max`, SPDX JSON, and GitHub artifact attestations. Install Cosign from a SHA-pinned action, sign each index digest keylessly, and attach SBOM/provenance attestations. Tags may aid discovery but deployment output and docs print only digest refs.

- [ ] **Step 5: Define vulnerability release policy**

Fail on any secret finding and any unfixed Critical vulnerability. A High vulnerability blocks unless `.github/aurora-sandbox-vex.json` names the exact CVE, package/version, image, status (`not_affected` or `fixed`), technical justification, approver, issue URL, and an expiry no more than 30 days away. The verifier rejects expired, wildcard, or missing-field entries.

- [ ] **Step 6: Verify signatures and attestations in CI**

After push, run `cosign verify`, `cosign verify-attestation --type spdxjson`, and provenance verification against the exact digest and repository identity. The job fails if a tag resolves to a different digest or either architecture is absent.

- [ ] **Step 7: Lint and validate workflow locally**

Run:

```bash
node scripts/verify-aurora-sandbox-locks.mjs --workflow
pnpm exec prettier --check .github/workflows/aurora-sandbox.yml deploy/aurora-sandbox/README.md
```

Expected: both pass.

- [ ] **Step 8: Commit supply-chain workflow**

```bash
git add .github/workflows/aurora-sandbox.yml .github/aurora-sandbox-vex.json \
  scripts/verify-aurora-sandbox-locks.mjs deploy/aurora-sandbox/README.md
git commit -m "ci(aurora): publish signed sandbox images"
```

### Task 5: Run Linux Acceptance and macOS Functional Smoke

**Files:**
- Modify: `deploy/aurora-sandbox/docker-security-test.sh`
- Modify: `deploy/aurora-sandbox/docker-smoke.sh`
- Modify: `deploy/aurora-sandbox/README.md`

**Interfaces:**
- Consumes: A locally built or pulled digest-specified image pair.
- Produces: Platform-labeled acceptance reports with no provider spend.

- [ ] **Step 1: Make both scripts require digest refs**

Reject tags and require `AURORA_SANDBOX_IMAGE` plus `AURORA_EGRESS_IMAGE` with full SHA-256. Record Docker client/server versions, kernel, architecture, cgroup mode, AppArmor status, and image digests before tests.

- [ ] **Step 2: Run the complete Linux acceptance matrix**

On Linux Docker Engine:

```bash
AURORA_SANDBOX_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:<verified-index-digest>' \
AURORA_EGRESS_IMAGE='ghcr.io/eanfs/multica-aurora-egress@sha256:<verified-index-digest>' \
AURORA_RUN_DOCKER_SECURITY_TEST=1 \
  deploy/aurora-sandbox/docker-security-test.sh
```

The script selects the platform manifest but records both index and platform digests. Expected: child plan B’s isolation/network tests and Task 3’s actual-container fake pipelines pass twice; no labeled resource remains.

- [ ] **Step 3: Run the macOS Docker Desktop smoke**

On macOS:

```bash
AURORA_SANDBOX_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:<verified-index-digest>' \
AURORA_EGRESS_IMAGE='ghcr.io/eanfs/multica-aurora-egress@sha256:<verified-index-digest>' \
  deploy/aurora-sandbox/docker-smoke.sh
```

Expected: managed enroll/claim/fake `xhs-image`/artifact/complete/idle/delete passes and output contains `FUNCTIONAL SMOKE ONLY` plus explicit `AppArmor/cgroup security acceptance not evaluated`.

- [ ] **Step 4: Preserve machine-readable reports**

Write sanitized JSON reports under `.scratch/aurora-sandbox-acceptance/` with platform, image digests, test names/counts, duration, pass/fail/skip reason, and no tokens/URLs/prompts. Do not commit local reports; CI uploads its copies.

- [ ] **Step 5: Commit script refinements**

```bash
git add deploy/aurora-sandbox/docker-security-test.sh \
  deploy/aurora-sandbox/docker-smoke.sh deploy/aurora-sandbox/README.md
git commit -m "test(aurora): codify sandbox acceptance"
```

### Task 6: Add Explicitly Gated Real Agent and Provider Smokes

**Files:**
- Create: `server/pkg/agent/aurora_sandbox_smoke_test.go`
- Modify: `.github/workflows/aurora-sandbox.yml`
- Modify: `deploy/aurora-sandbox/README.md`
- Modify: `scripts/agent-cli-command-names.txt`

**Interfaces:**
- Consumes: Signed digest images, a live test Multica stack, test workspace, provider secret files, and explicit opt-ins.
- Produces: Cost-bearing route evidence that cannot run in default tests.

- [ ] **Step 1: Add the repository-required real-agent gate first**

Use build tag and runtime check:

```go
//go:build agentintegration

func TestAuroraSandboxRealProviderSmoke(t *testing.T) {
    if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
        t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1")
    }
    // subtests each require their own AURORA_RUN_*_SMOKE=1
}
```

The test must check gates before executable lookup, Docker image pull, credential-file read, account access, or network call.

- [ ] **Step 2: Add provider-specific subtests and opt-ins**

| Subtest | Additional variable | Minimal operation |
| --- | --- | --- |
| Claude text | `AURORA_RUN_CLAUDE_SMOKE=1` | one short `xhs-copy` artifact |
| Seedream | `AURORA_RUN_SEEDREAM_SMOKE=1` | one low-count 2K image |
| Seedance | `AURORA_RUN_SEEDANCE_SMOKE=1` | one shortest supported low-resolution video |
| Volc ASR | `AURORA_RUN_VOLC_ASR_SMOKE=1` | transcribe committed short WAV |
| OpenAI generation/edit | `AURORA_RUN_OPENAI_IMAGE_SMOKE=1` | one generation and one edit fixture |
| HyperFrames/FFmpeg | `AURORA_RUN_HYPERFRAMES_SMOKE=1` | deterministic caption render from fixture |
| Chromium resume | `AURORA_RUN_CHROMIUM_SMOKE=1` | deterministic PDF from fixture data |

A subtest with a missing opt-in is skipped before checking its secret. There is no “run all providers” implicit default.

- [ ] **Step 3: Exercise the full managed path**

Each enabled subtest creates a fresh workspace/generation through the public Aurora API, waits conditionally for terminal status, verifies expected artifact kind/hash/nonzero size and exact credit settlement, then deletes test objects/node. It must use the signed digest image and must not call vendor scripts directly on the host.

- [ ] **Step 4: Sanitize and bound real-smoke output**

Logs contain test name, provider/model ID, Multica generation/task IDs, duration, status, byte count, and cost/credit delta. They do not contain prompts, user document text, keys, task tokens, signed URLs, full provider responses, or artifact bytes. Apply a 35-minute outer deadline and stop after one provider create per operation.

- [ ] **Step 5: Add workflow-dispatch-only real smoke jobs**

The workflow accepts boolean inputs per subtest, maps only selected environment secrets into temporary mode-`0400` files, requires a protected `aurora-provider-smoke` environment, disables pull-request invocation, and uploads sanitized reports. The job command is the repository-approved specific test:

```bash
cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 \
  go test -tags=agentintegration ./pkg/agent \
  -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
```

- [ ] **Step 6: Prove default and ungated behavior**

Run without the build tag:

```bash
cd server && go test ./pkg/agent -run AuroraSandboxRealProviderSmoke -count=1 -v
```

Expected: no matching real-smoke test is compiled and no account/network access occurs.

Run with tag but without global opt-in:

```bash
cd server && go test -tags=agentintegration ./pkg/agent \
  -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
```

Expected: one immediate skip before Docker/credential/executable lookup.

Do not run with `MULTICA_RUN_REAL_AGENT_SMOKE=1` during implementation unless the user explicitly authorizes cost/account access at that time.

- [ ] **Step 7: Commit gated smokes**

```bash
git add server/pkg/agent/aurora_sandbox_smoke_test.go \
  .github/workflows/aurora-sandbox.yml deploy/aurora-sandbox/README.md \
  scripts/agent-cli-command-names.txt
git commit -m "test(aurora): gate real provider smoke"
```

### Task 7: Run Final Verification and Close the Boundary

**Files:**
- Modify after evidence: `AGENTS.md`
- Modify after evidence: `docs/superpowers/plans/2026-09-11-aurora-execution.md`
- Modify after evidence: `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md`
- Modify after evidence: the four child plan checkboxes

**Interfaces:**
- Consumes: Every child-plan acceptance artifact and the signed image digest.
- Produces: Repository documentation that accurately states what passed and what operational checks remain.

- [ ] **Step 1: Run lock, vendor, and runtime tests**

```bash
node scripts/verify-aurora-volc-skills.mjs
node scripts/verify-aurora-sandbox-locks.mjs --workflow
pnpm --dir deploy/aurora-sandbox/runtime test
```

Expected: all pass with no external provider call.

- [ ] **Step 2: Run frontend and backend suites**

```bash
pnpm typecheck
pnpm lint
pnpm test
make test
```

Expected: all pass. If the known `repocache/TestGitEnv` environment leak or deadline flakes occur under full load, preserve the failure, run the exact focused test in the managed worktree environment, and report both; do not rewrite history or claim the full suite passed.

- [ ] **Step 3: Run build, image, fake pipeline, and Linux security gates**

Run the exact Tasks 2–5 commands on Linux using digest refs. Expected: both architectures build; content verifier passes; 13-route fake matrix passes; representative actual-container pipelines pass; isolation test passes twice; cleanup leaves no managed resources.

- [ ] **Step 4: Verify published supply-chain artifacts**

For each sandbox/proxy index digest, preserve successful output from Cosign signature verification, SPDX attestation verification, provenance verification, Trivy policy, and architecture manifest inspection.

- [ ] **Step 5: Record optional real-smoke status truthfully**

List each provider subtest as passed, failed, or skipped with its exact gate reason. Paid-provider smokes are operational validation and cannot replace deterministic fake tests; lack of explicit authorization must be recorded as skipped, not failure and not success.

- [ ] **Step 6: Update repository status only after required gates**

Change the roadmap and Plan 3 Task 6 from partial to complete only when lock/vendor tests, all default suites, image build/content, all-13 fake matrix, actual-container fake pipelines, Linux security acceptance, and supply-chain verification pass. Record the accepted sandbox/proxy digest refs and date; do not embed credentials or local machine paths.

- [ ] **Step 7: Ask before outward-facing tracker changes**

After repository evidence is committed, obtain explicit user confirmation before editing or closing issue #29. If approved, update `eanfs/multica` with the digest and acceptance summary, then apply the project’s status-label lifecycle. Do not target `multica-ai/multica`.

- [ ] **Step 8: Run documentation checks and commit status**

```bash
git diff --check
```

Expected: exit 0.

```bash
git add AGENTS.md docs/superpowers/plans/2026-09-11-aurora-execution.md \
  docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md \
  docs/superpowers/plans/2026-09-25-aurora-managed-sandbox-control-plane.md \
  docs/superpowers/plans/2026-09-25-aurora-sandbox-fleet-isolation.md \
  docs/superpowers/plans/2026-09-25-aurora-sandbox-skill-runtime.md \
  docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md
git commit -m "docs(aurora): close sandbox runtime milestone"
```

## Plan D Completion Evidence

Preserve and link from the final acceptance record:

- exact multi-architecture sandbox/proxy index and platform digests;
- lock and vendor verification output;
- image content/history/secret scan output;
- SPDX SBOMs and provenance;
- Trivy and VEX policy output;
- Cosign signature and attestation verification;
- 13-route fake matrix and actual-container fake pipeline results;
- two consecutive Linux isolation passes and clean resource listing;
- macOS functional smoke with its non-security disclaimer;
- default frontend/backend suite outputs;
- real-smoke pass/fail/skip table with authorization reasons;
- confirmation that issue #29 was not changed without explicit user approval.
