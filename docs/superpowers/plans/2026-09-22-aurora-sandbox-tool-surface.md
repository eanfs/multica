# Aurora Sandbox Tool Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every currently available Aurora skill execute in a repository-owned, per-workspace managed sandbox with scoped enrollment, provider-specific tools, validated artifact delivery, enforced isolation, and reproducible image supply-chain controls.

**Architecture:** This document is the execution index for four independently reviewable child plans. The Multica server provisions one sandbox node per workspace through an authenticated fleet controller; the node exchanges a single-use enrollment secret for a workspace-scoped daemon credential, claims only its managed runtime, invokes Claude with a narrow MCP tool surface, and uploads validated manifest-declared artifacts. Docker isolation and an outbound proxy enforce the security boundary, while a digest-pinned multi-architecture image contains the daemon, reviewed provider adapters, pinned Volcengine skills, HyperFrames, FFmpeg, and Chromium.

**Tech Stack:** Go 1.26.6, Chi, sqlc, PostgreSQL, Docker Engine/Buildx, Node.js 22, Claude Code 2.1.282, MCP SDK 1.30.1, Volcengine Ark/Seedream/Seedance/ASR, OpenAI Images, HyperFrames 0.8.75, FFmpeg, Chromium, GitHub Actions, Syft, Trivy, Cosign

**Spec:** `docs/superpowers/specs/2026-09-11-aurora-content-creation-app-design.md`

## Global Constraints

- All runtime code, Dockerfiles, security policy, vendored provider skills, tests, and image workflows live in this `multica` repository; no implementation is delegated to `aurora-ai-agents` or a new repository.
- Linux Docker Engine is the security acceptance platform. Docker Desktop on macOS is a developer smoke platform and cannot satisfy the AppArmor, cgroup, or Linux-kernel acceptance gates.
- Provision exactly one sandbox node per workspace. A node claims exactly one managed runtime and executes at most one task at a time.
- The default idle TTL is 15 minutes, the hard node lifetime is 8 hours, and the task timeout is 30 minutes. All three are configurable only through fleet/server process configuration, not task input.
- Carrier identity and execution identity remain separate: the persisted runtime provider is `aurora_managed`; the only execution provider returned to the managed daemon is `claude`.
- Replace the shared `AURORA_SANDBOX_TOKEN` registration contract. Enrollment uses a single-use `mse_` bearer secret with a five-minute TTL and returns an existing `mdt_` daemon credential scoped to one workspace and daemon ID.
- Provider selection is deterministic by Aurora skill ID. There is no retry against a different provider and no silent provider fallback.
- General Bash, unrestricted filesystem tools, web browsing, dynamic package installation, and arbitrary MCP configuration remain unavailable to Aurora prompts. Reviewed scripts are exposed only through narrow MCP methods.
- The default test suite must use fake agent binaries and fake provider servers. It must not resolve or execute a user-installed Claude CLI or contact Anthropic, Volcengine, OpenAI, or other paid services.
- Real-agent/provider smoke is allowed only under the `agentintegration` build tag and `MULTICA_RUN_REAL_AGENT_SMOKE=1`, with explicit provider-specific opt-in variables.
- Provider credentials are never copied into image layers, Docker environment arguments, task payloads, logs, artifacts, or model-visible prompts. Runtime secrets enter as read-only files and are exposed only to the process that needs each credential.
- Input and output data are workspace-scoped. Attachment IDs, daemon claims, uploads, manifests, assets, and reports must all prove the same workspace and task relationship before writes.
- New database relationships have no foreign keys or cascading actions. Every migration-created index uses `CREATE [UNIQUE] INDEX CONCURRENTLY` in its own single-statement migration.
- API responses consumed by web/desktop code use zod and `parseWithFallback`; no response is asserted with `as T`.
- Repository code, comments, docs, test names, commits, workflow text, and image metadata remain in English.
- Issue #29 and the roadmap remain partial until every acceptance gate in this master plan passes on a Linux Docker host.

## Progress Status (2026-09-25)

- [x] Root-cause audit, provider decisions, official Volcengine skill audit, master plan, and four child plans are complete in pr://eanfs/multica/84.
- [ ] Plan A — scoped enrollment, daemon managed mode, claim-set installation, and execution-provider mapping.
- [ ] Plan B — workspace fleet lifecycle, hardened Docker policy, enforced egress, and Linux isolation acceptance.
- [ ] Plan C — attachment inputs, all 13 available skill routes, hardened provider tools, and artifact staging/reporting.
- [ ] Plan D — reproducible multi-architecture images, SBOM/provenance/signing, container smoke, and final acceptance.

Tracker issue #29 remains open with `S2-InProgress`. Planning completion is not implementation completion; the next executable frontier is Plan A Task 1.

---

## Why the Previous Boundary Is Replaced

The earlier version of this plan treated the sandbox image, container isolation, outbound policy, and HyperFrames smoke as external infrastructure. That boundary cannot produce an executable Aurora task:

1. `POST /api/daemon/managed/register` authenticates every workspace with one global secret, does not mint an `mdt_` credential, and does not bind the managed runtime to a daemon.
2. A real daemon builds its claim set from `runtimeIndex`; no managed bootstrap path inserts the returned runtime ID.
3. The server persists `provider=aurora_managed`, while the reviewed execution surface accepts `provider=claude`; passing the carrier provider through fails closed before Claude launches.
4. The fleet controller starts an image with labels and environment variables but does not assign a workspace, enforce a digest, mount a scoped secret, constrain the container, or provide enforced egress.
5. Aurora generation accepts no attachment IDs, so eight available skills cannot receive their declared image, video, audio, or document inputs.
6. `TaskResult.Artifacts` is never populated from local outputs; the existing user/chat upload path does not authorize Aurora quick-create tasks.
7. The existing fleet end-to-end test simulates daemon calls through HTTP handlers and therefore does not prove a real managed daemon can enroll, claim, execute, upload, and settle.

The four child plans below close those gaps inside this repository.

## Fixed Skill Routing Matrix

| Skill ID | Required/optional inputs | Execution route | Required output | Fallback |
| --- | --- | --- | --- | --- |
| `poster` | Prompt; 0–4 reference images | Volcengine Seedream | 1 image | None |
| `xhs-image` | Prompt; 0–4 reference images | Volcengine Seedream | 1–9 images | None |
| `product-image` | Prompt; 0–4 product/reference images | OpenAI Images generation when no image is supplied, OpenAI Images edit when images are supplied | 1–4 images | None |
| `text-image` | Prompt only | Volcengine Seedream | 1 image | None |
| `image-edit` | Prompt; 1–4 images | OpenAI Images edit | 1–4 images | None |
| `id-photo` | Prompt; exactly 1 image | Local deterministic image transform | 1 image | None |
| `image-video` | Prompt; exactly 1 image | Volcengine Seedance | 1 video | None |
| `text-video` | Prompt only | Volcengine Seedance | 1 video | None |
| `video-captions` | Prompt; exactly 1 video | Volcengine ASR, then HyperFrames/FFmpeg deterministic composition | 1 video and 1 transcript | None |
| `xhs-copy` | Prompt; 0–1 document | Claude text workflow | 1 Markdown text artifact | None |
| `resume` | Prompt; 0–1 document | Claude HTML workflow, then headless Chromium PDF render | 1 PDF and 1 Markdown text artifact | None |
| `document-summary` | Prompt; exactly 1 document | Claude text workflow | 1 Markdown text artifact | None |
| `transcription` | Prompt; exactly 1 audio or video file | Volcengine ASR | 1 transcript | None |

`avatar-video`, `ppt`, and `excel` stay unavailable. This work must not make them executable or change their catalog availability.

The old phrase “HyperFrames text-to-video smoke” is refined by this matrix: generative `text-video` is a Seedance route, while HyperFrames is validated with the deterministic `video-captions` composition. HyperFrames is never a fallback for failed Seedance generation.

## Runtime Data Flow

```text
Aurora UI
  -> upload input files as the signed-in workspace member
  -> POST /api/aurora/generations { skillId, prompt, attachmentIds }
  -> server validates skill-specific input rules
  -> server ensures one workspace node through fleet control API
  -> entitlement + reserve + generation + quick-create task

Fleet controller
  -> stores one-use mse_ token in a 0400 host file
  -> starts image@sha256 on an internal Docker network
  -> mounts only scoped secret files and writable tmpfs

Managed daemon
  -> exchanges mse_ token for mdt_ token + runtime + executionProvider=claude
  -> adds runtime ID to its in-memory claim set
  -> claims at most one task
  -> launches Claude with only Aurora workflow + narrow MCP tools
  -> provider tools write outputs and a versioned manifest under /workspace
  -> daemon validates paths, symlinks, sizes, MIME, hashes, counts, and formats
  -> daemon uploads each file with the task token
  -> daemon reports uploaded artifact descriptors
  -> existing server moderation writes assets and settles/refunds credits

Lifecycle
  -> heartbeat and task transitions update node activity
  -> reaper drains hard-expired nodes and removes idle nodes
  -> runtime becomes offline and daemon tokens are revoked on termination
```

## Child Plans and Dependency Graph

| Order | Plan | Deliverable | Depends on |
| --- | --- | --- | --- |
| A | `docs/superpowers/plans/2026-09-25-aurora-managed-sandbox-control-plane.md` | Scoped enrollment, daemon managed mode, claim-set installation, provider mapping, and a fake-runner lifecycle test | Existing Plan 3 execution APIs |
| B | `docs/superpowers/plans/2026-09-25-aurora-sandbox-fleet-isolation.md` | Workspace autoprovisioning, node lifecycle, hardened Docker backend, enforced egress proxy, Linux security tests | A |
| C | `docs/superpowers/plans/2026-09-25-aurora-sandbox-skill-runtime.md` | Attachment inputs, all 13 routes, reviewed tools, structured manifests, task-scoped upload, fake provider matrix | A; B policy contracts |
| D | `docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md` | Digest-pinned image, SBOM/signing workflow, Linux acceptance, macOS smoke, gated real-provider smoke | A, B, C |

Plans A and the pure Docker argument-policy portion of B may be developed in parallel. The B server integration starts only after A’s enrollment types are merged. Plan C may develop attachment UI/API and provider adapters in parallel after its route contract is fixed, but artifact reporting requires A’s managed daemon. Plan D integrates only merged outputs from A–C.

## Required Configuration Contract

| Variable | Process | Meaning | Default/requirement |
| --- | --- | --- | --- |
| `AURORA_FLEET_URL` | Multica server | Fleet controller base URL | Empty disables generation before credits are reserved |
| `AURORA_FLEET_CONTROL_TOKEN_FILE` | Multica server and fleet | Shared server-to-fleet control credential file | Required when fleet is enabled |
| `AURORA_SANDBOX_IMAGE` | Fleet | Sandbox OCI reference | Required and must contain `@sha256:` |
| `AURORA_SANDBOX_IDLE_TTL` | Multica server | Idle node retention | `15m` |
| `AURORA_SANDBOX_MAX_LIFETIME` | Multica server | Hard node lifetime | `8h` |
| `MULTICA_AGENT_TIMEOUT` | Managed daemon | Per-task deadline | `30m` |
| `AURORA_EGRESS_ALLOWED_HOSTS` | Egress proxy | Exact provider hosts added to compiled defaults | Empty adds no hosts |
| `AURORA_EGRESS_SERVER_ORIGIN` | Egress proxy | Exact Multica server origin, including port | Required |
| `ANTHROPIC_API_KEY_FILE` | Sandbox launcher | Claude credential file | Required for real execution |
| `ARK_API_KEY_FILE` | Provider MCP | Seedream/Seedance/ASR credential file | Required only for Volcengine routes |
| `OPENAI_API_KEY_FILE` | Provider MCP | OpenAI Images credential file | Required only for OpenAI routes |
| `VOLC_ASR_API_KEY_FILE` | Provider MCP | Volcengine ASR current-console API key file | Required only for transcription/caption routes |

There is no `AURORA_SANDBOX_TOKEN` after Plan A. Configuration must reject a literal provider secret value where a `*_FILE` path is required.

## Acceptance Gates

### Functional

- A fresh workspace generation causes exactly one sandbox node to be created and reused while healthy.
- The real managed-daemon code path exchanges one enrollment token, installs one runtime ID, claims with `max_tasks=1`, runs the fake agent, uploads a manifest-declared artifact, reports it, and reaches existing completion settlement.
- A consumed, expired, wrong-workspace, malformed, or revoked enrollment token is rejected without minting a daemon token or binding a runtime.
- The table-driven fake-provider end-to-end suite completes all 13 available skills and verifies every route in the fixed matrix.
- Input rules reject missing required files, wrong kinds, foreign-workspace files, too many files, and unavailable skills before reserving credits.
- A provider failure marks the task/generation failed and triggers the existing idempotent refund path; no other provider is contacted.
- A malformed or unsafe artifact manifest fails the task, uploads nothing unsafe, and triggers the refund path.
- Idle nodes stop after 15 minutes. Hard-expired nodes drain, stop after their active task exits or the 30-minute task deadline elapses, revoke credentials, and mark the runtime offline.

### Linux security

- The sandbox runs as UID/GID `10001:10001` with read-only root, all capabilities dropped, `no-new-privileges`, an explicit seccomp profile, an AppArmor profile, PID/CPU/memory/swap/open-file limits, and size-limited `noexec,nosuid,nodev` tmpfs mounts.
- The sandbox has no Docker socket, host PID/IPC/network namespace, privileged mode, device mount, writable bind mount, or provider secret in `docker inspect` environment output.
- The sandbox cannot contact the public internet, RFC1918/link-local/loopback addresses, cloud metadata endpoints, or an unlisted hostname directly or through the proxy.
- The sandbox can contact only the exact Multica server origin and the reviewed provider API hosts (`api.anthropic.com`, `ark.cn-beijing.volces.com`, `api.openai.com`, and `openspeech.bytedance.com`) through the egress proxy. Provider-generated media URLs are imported by the server and are never fetched by the sandbox.
- Artifact path traversal, absolute paths, symlinks, hard links escaping the workspace, MIME/extension mismatch, oversized output, excessive files, and SHA-256 mismatch are rejected.

### Supply chain

- The sandbox build uses digest-pinned multi-architecture base images and a dated Debian snapshot.
- Claude Code, MCP SDK, OpenAI SDK, HyperFrames, and the skills installer are exact versions in a committed lockfile.
- The two official Volcengine skills are copied into the repository during an explicit update task, carry source-lock metadata and SHA-256 inventory, and are never installed at runtime.
- CI builds `linux/amd64` and `linux/arm64`, scans the image, emits SPDX JSON and provenance, pushes by digest, and signs the digest with Cosign keyless signing.
- The fleet rejects a tag-only image reference.

### Smoke and cost controls

- Docker Desktop on macOS can enroll, claim, execute one fake `xhs-image` request, upload an image, and become idle; the report states that AppArmor and Linux cgroup enforcement were not accepted there.
- The gated real-provider workflow has separate Seedream, Seedance, Volcengine ASR, OpenAI generation/edit, Claude text, Chromium PDF, and HyperFrames caption-render jobs. A job runs only when its explicit opt-in variable and required secret files are present.
- No default test command contacts external providers or consumes credits.

---

### Task 1: Complete the Managed Control Plane

**Files:**
- Follow: `docs/superpowers/plans/2026-09-25-aurora-managed-sandbox-control-plane.md`
- Modify after child-plan acceptance: `docs/superpowers/plans/2026-09-11-aurora-execution.md`

**Interfaces:**
- Consumes: Existing daemon-token authentication, managed runtime rows, task claim/heartbeat/report APIs.
- Produces: `mse_` enrollment issuance/consumption, `ManagedEnrollmentResponse`, managed-daemon bootstrap, one-runtime claim set, and `execution_provider=claude`.

- [ ] **Step 1: Execute every unchecked task in child plan A in order**

Run the child plan with either required execution skill. Do not proceed on a partial test result.

- [ ] **Step 2: Run child plan A’s final verification block**

Expected: all focused Go tests, sqlc checks, and the fake-runner managed lifecycle pass without a real Claude executable.

- [ ] **Step 3: Record the narrower completion statement**

Update the old Plan 3 Task 6 status only to say that the managed control plane is complete and Plans B–D remain required. Do not mark issue #29 complete.

### Task 2: Complete Fleet Isolation and Lifecycle

**Files:**
- Follow: `docs/superpowers/plans/2026-09-25-aurora-sandbox-fleet-isolation.md`

**Interfaces:**
- Consumes: Child plan A’s enrollment issuer and managed node identity.
- Produces: authenticated fleet ensure/delete API, workspace node manager, hardened Docker policy, egress proxy, idle/hard-lifetime reaper.

- [ ] **Step 1: Execute every unchecked task in child plan B in dependency order**

The Docker argument-policy tasks may begin earlier, but server autoprovision integration consumes the merged Plan A types.

- [ ] **Step 2: Run child plan B’s final verification block on Linux Docker**

Expected: lifecycle, isolation, and network-denial tests pass. A macOS-only result does not satisfy this task.

### Task 3: Complete the 13-Skill Runtime and Artifact Path

**Files:**
- Follow: `docs/superpowers/plans/2026-09-25-aurora-sandbox-skill-runtime.md`

**Interfaces:**
- Consumes: Child plan A managed daemon and child plan B filesystem/network policy.
- Produces: attachment-aware generation requests, fixed skill routes, narrow provider tools, versioned artifact manifest, task-scoped upload, all-skill fake-provider test matrix.

- [ ] **Step 1: Execute every unchecked task in child plan C**

Keep official Volcengine scripts vendored and reviewed. Do not enable Bash or runtime package installation to accommodate a script.

- [ ] **Step 2: Run child plan C’s final verification block**

Expected: the all-13 fake matrix passes; route failures do not invoke another provider; unsafe manifests are rejected and refunded.

### Task 4: Build and Accept the Reproducible Image

**Files:**
- Follow: `docs/superpowers/plans/2026-09-25-aurora-sandbox-image-smoke.md`
- Modify after every gate passes: `AGENTS.md`
- Modify after every gate passes: `docs/superpowers/plans/2026-09-11-aurora-execution.md`
- Modify after every gate passes: this file

**Interfaces:**
- Consumes: Merged artifacts from child plans A–C.
- Produces: signed multi-architecture image digest, SBOM, provenance, Linux acceptance report, macOS development smoke, and gated real-provider smoke evidence.

- [ ] **Step 1: Execute every unchecked task in child plan D**

Do not use the real-provider smoke to substitute for fake deterministic tests or Linux isolation acceptance.

- [ ] **Step 2: Run the complete final acceptance matrix**

Run exactly the commands in Plan D. Capture the image digest, test counts, security inspection output, and each skipped gated smoke with its missing opt-in/secret reason.

- [ ] **Step 3: Close the roadmap boundary only after evidence exists**

Change Plan 3 Task 6 and the Aurora roadmap from partial to complete only when all functional, Linux security, supply-chain, and required fake-smoke gates in this master plan pass. Real paid-provider smoke may remain an explicitly recorded operational check only if every adapter has deterministic fake coverage and the release owner intentionally skipped cost-bearing validation.

## Out of Scope

- Enabling `avatar-video`, `ppt`, or `excel`.
- Cloud entitlement `GateAurora*` integration.
- Kubernetes, Firecracker, gVisor, or a multi-host fleet scheduler.
- More than one sandbox node per workspace or parallel execution within a node.
- Automatic provider fallback, provider load balancing, or model selection by the LLM.
- A user-facing provider picker or provider credential UI.
- Persisting server data in Zustand or moving Aurora UI into mobile.
- Treating Docker Desktop as proof of Linux kernel isolation.
