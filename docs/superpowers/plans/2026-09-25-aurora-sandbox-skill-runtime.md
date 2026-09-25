# Aurora Sandbox Skill Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept the declared inputs for all 13 available Aurora skills, route each skill to a fixed reviewed tool chain, and deliver only validated, task-owned artifacts through an idempotent server path.

**Architecture:** Aurora requests upload workspace-owned attachments before enqueue, and the server validates a skill-specific input contract. The sandbox runs Claude with 13 small workflow briefs and a narrow MCP broker; the broker wraps hardened, source-locked Volcengine skills plus OpenAI Images, Volcengine ASR, HyperFrames, FFmpeg, Chromium, and deterministic local transforms without exposing Bash or arbitrary paths. Tools register create-once provider runs and write a versioned artifact manifest; the daemon validates local files or server-staged objects and reports only staging IDs, while the server moderates and commits assets transactionally and idempotently.

**Tech Stack:** Go 1.26.6, Chi, sqlc, PostgreSQL, TypeScript/Node.js 22, MCP SDK 1.30.1, official Volcengine AgentPlan skills, OpenAI 7.23.0, HyperFrames 0.8.75, FFmpeg/ffprobe, Chromium, Vitest/Node test runner, React/TanStack Query

**Spec:** `docs/superpowers/specs/2026-09-11-aurora-content-creation-app-design.md`

## Global Constraints

- This plan is child plan C of `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md`; child plan A’s managed daemon exists and child plan B’s proxy/filesystem policies are fixed before runtime integration.
- Cover exactly the 13 catalog entries whose current `available` value is true. Keep `avatar-video`, `ppt`, and `excel` unavailable.
- Use the fixed route matrix in the master plan. Provider choice is server-owned and keyed by skill ID; neither user prompt nor model can choose another provider.
- No provider fallback. A failed, unsupported, unavailable, timed-out, or ambiguous provider run fails the task and reaches the existing idempotent refund settlement.
- The model never receives a provider key, raw task/daemon token, provider signed URL, arbitrary filesystem path, API base override, model override, callback URL, provider list/delete operation, or shell command.
- General Bash, WebFetch, WebSearch, unrestricted Read/Write, runtime skill installation, user skill discovery, and arbitrary MCP configuration remain denied.
- Tool arguments use attachment IDs and controlled relative output names. The broker resolves those IDs through a runner-created input map and never accepts an absolute path or `..` segment.
- Provider operations are create-once per `(task_id, operation)`. An ambiguous run is failed/refunded rather than submitted twice.
- Official Volcengine skill source is copied with the user-requested `npx skills add` flow only during an explicit vendor update; Docker builds and runtime never fetch skills.
- Do not vendor the inspected Volcengine skills unchanged into the executable image. Seedream requires origin, SSRF, size, permission, logging, and credential hardening; Seedance requires task/path/model/state narrowing. Maintain the untouched upstream tree plus an auditable patch series.
- Volcengine media result URLs never leave the proxy allowlist through the sandbox. The broker passes them to a task-scoped Multica importer, which performs bounded SSRF-safe server-side retrieval into owned storage.
- Artifact discovery uses a broker-written manifest, never model prose, stdout scraping by the daemon, directory globbing, or “latest file” heuristics.
- Default tests use fake HTTP providers and fake agent executables. They make no paid/external call.
- UI changes apply to shared web/desktop Aurora views and the Aurora Next.js host; mobile remains out of scope.

## Audited Volcengine Source Lock

The official source was inspected after running the two requested installs in an isolated directory with `skills` 1.7.0.

| Field | Seedance | Seedream |
| --- | --- | --- |
| Skill ID | `byted-ark-seedance-skill` | `byted-ark-seedream-skill` |
| Declared version | `5.0.0` | `4.0.0` |
| Declared license | MIT in SKILL frontmatter; upstream package contains no LICENSE file | MIT; full text is in `references/LICENSE`, copyright 2026 VolcEngine / AgentPlan |
| Package name | skill directory identity; package metadata does not add a distinct published dependency | `ark-agentplan-seedream-skill` |
| Source | `skills.volces.com` | `skills.volces.com` |
| Source URL | `https://skills.volces.com/skills/volcengine/agentplan` | same |
| Source type | `well-known` | `well-known` |
| `computedHash` | `9aed265f32003e867089212d831c1982e369fd766c02d8459770370377ca04eb` | `e11bbd33031f6c2e975291d08cb7ab09a9468ec7442496744070fc54f12c4a94` |
| `wellKnownDigest` | `sha256:97bfafe21dfdd127cb7a040413ac2be5a0a816f75db1d049506dac222b42d6dd` | `sha256:ecd6b80fc5b2b10c6dc944fd8940126a289e9ffb7f6890672baaa9ece4410a2c` |
| Runtime dependency | Node built-ins only; Node `>=18` | Node built-ins only; Node `>=18` |

The upstream lock has no commit, signed release, timestamp, per-file hashes, tool version, or patch identity. Record `upstream_revision: null`; do not invent a revision. Distribution cannot proceed until the exact Seedance MIT license text is obtained from Volcengine and committed beside that vendor tree.

## Fixed Provider Contracts

### Seedream

- API origin: exactly `https://ark.cn-beijing.volces.com`.
- Endpoint: `POST /api/plan/v3/images/generations`.
- Credential: broker reads `/run/secrets/ark-api-key`; child process receives only `ARK_API_KEY` and cannot scan workstation config or generic key names.
- Allowed models: `doubao-seedream-5.0-lite`, `doubao-seedream-5.0-pro`.
- Routing uses the reviewed official capability logic; no arbitrary model ID or environment override.
- Inputs are task-authorized PNG/JPEG references converted to bounded Data URIs.
- The patched adapter returns provider result descriptors without downloading arbitrary URLs.

### Seedance

- API origin: exactly `https://ark.cn-beijing.volces.com`.
- Create endpoint: `POST /api/plan/v3/contents/generations/tasks`.
- Poll endpoint: `GET /api/plan/v3/contents/generations/tasks/{cgt-task-id}`.
- Credential: the same task-scoped Ark key file contract as Seedream.
- Default model: `doubao-seedance-2.0` (`doubao-seedance-2-0-260128`).
- Allowlist: `doubao-seedance-2.0`, `doubao-seedance-2.0-fast`, `doubao-seedance-2.0-mini`, and `doubao-seedance-2.5`. Do not enable the audited 1.5 alias because its declared name maps inconsistently to a 1.0 backend ID.
- The operator may select one allowlisted default at sandbox startup; prompt/model arguments cannot override it.
- Expose create and poll for the current server-recorded provider run only. Do not expose list, delete, callback URL, payload file, arbitrary task file, user override, saved preference, or saved credential operations.
- Poll up to the 30-minute Multica task deadline. A provider-pending result at deadline is failure, not successful completion.

### Volcengine ASR

- API origin: exactly `https://openspeech.bytedance.com`.
- Endpoint: `POST /api/v3/auc/bigmodel/recognize/flash`.
- Resource ID: exactly `volc.bigasr.auc_turbo`.
- Credential: current-console `X-Api-Key` read from `/run/secrets/volc-asr-api-key`; do not support legacy app/access key scanning in the sandbox.
- Request headers include a UUID `X-Api-Request-Id` and `X-Api-Sequence: -1`.
- Request body uses `audio.data` with streaming base64 encoding, `request.model_name=bigmodel`, punctuation and ITN enabled, and no callback.
- Accept WAV, MP3, or OGG/Opus input up to 100 MiB and two hours. Video is first converted to bounded mono audio by FFmpeg.

### OpenAI Images

- API origin: exactly `https://api.openai.com`.
- Endpoints: `/v1/images/generations` and `/v1/images/edits`.
- Credential: `/run/secrets/openai-api-key`.
- SDK: exact version `openai@7.23.0`.
- Allowed models: `gpt-image-2.5-sunburst` and `gpt-image-2.5-flare`; deployment chooses one default, and tool input cannot name a model.
- Request base64 output so no OpenAI result host must be added to sandbox egress.

## Input Rules

| Skill | Prompt | Attachments |
| --- | --- | --- |
| `poster` | Required, 1–3,000 chars | 0–4 images |
| `xhs-image` | Required, 1–3,000 chars | 0–4 images |
| `product-image` | Required, 1–3,000 chars | 0–4 images |
| `text-image` | Required, 1–3,000 chars | none |
| `image-edit` | Required, 1–3,000 chars | 1–4 images |
| `id-photo` | Required, 1–3,000 chars | exactly 1 image |
| `image-video` | Required, 1–3,000 chars | exactly 1 image |
| `text-video` | Required, 1–3,000 chars | none |
| `video-captions` | Required, 1–3,000 chars | exactly 1 video |
| `xhs-copy` | Required, 1–10,000 chars | 0–1 document |
| `resume` | Required, 1–10,000 chars | 0–1 document |
| `document-summary` | Required, 1–10,000 chars | exactly 1 document |
| `transcription` | Required, 1–3,000 chars | exactly 1 audio or video |

Per-file input caps: image 25 MiB, document 25 MiB, audio/video 100 MiB. Accepted images are PNG/JPEG; documents are UTF-8 text/Markdown, PDF, or DOCX; audio is WAV/MP3/OGG/Opus; video is MP4/MOV/WebM. Validate extension, sniffed MIME, ownership, workspace, and storage existence before provisioning or reserving credits.

## Artifact Manifest Contract

The broker atomically renames a bounded temporary file to `/workspace/output/.multica/aurora-artifacts.v1.json`:

```json
{
  "schema": "com.multica.aurora.artifacts",
  "version": 1,
  "task_id": "00000000-0000-0000-0000-000000000000",
  "skill_id": "xhs-image",
  "producer": {
    "id": "byted-ark-seedream-skill",
    "version": "4.0.0",
    "tree_sha256": "sha256:64-lowercase-hex"
  },
  "provider_run": {
    "provider": "volcengine-agentplan",
    "model": "doubao-seedream-5.0-pro",
    "external_id": null
  },
  "artifacts": [
    {
      "id": "primary-1",
      "source": {
        "type": "file",
        "relative_path": "artifacts/primary-1.png"
      },
      "name": "primary-1.png",
      "kind": "image",
      "role": "primary",
      "format": "png",
      "mime_type": "image/png",
      "size_bytes": 1234,
      "sha256": "sha256:64-lowercase-hex",
      "metadata": {}
    }
  ]
}
```

For a Volcengine URL imported by the server, `source` is `{ "type": "staged_object", "staging_id": "<uuid>" }`; no URL appears in the manifest. Maximum manifest size is 1 MiB; maximum artifacts is 20; maximum total size is 600 MiB. Per-artifact limits are 25 MiB for image/text/PDF and 500 MiB for video. At least one `role=primary` artifact whose kind matches the catalog output is mandatory.

## File Structure

### New files and directories

- `deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedance-skill/**` — untouched installed 5.0.0 tree.
- `deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedream-skill/**` — untouched installed 4.0.0 tree.
- `deploy/aurora-sandbox/vendor/volcengine/skills-lock.json` — upstream lock.
- `deploy/aurora-sandbox/vendor/volcengine/vendor-lock.json` — Multica source/file/license/patch inventory.
- `deploy/aurora-sandbox/vendor/volcengine/patches/*.patch` — explicit hardening changes.
- `scripts/update-aurora-volc-skills.sh` — isolated, pinned vendor update command.
- `scripts/verify-aurora-volc-skills.mjs` — deterministic file and patch verification.
- `deploy/aurora-sandbox/runtime/package.json` — exact runtime dependencies.
- `deploy/aurora-sandbox/runtime/src/server.mjs` — MCP entry point.
- `deploy/aurora-sandbox/runtime/src/policy.mjs` — fixed skill routes and attachment/output rules.
- `deploy/aurora-sandbox/runtime/src/task-context.mjs` — bounded runner-provided context and authorized path mapping.
- `deploy/aurora-sandbox/runtime/src/provider-run.mjs` — begin/record/finish create-once calls.
- `deploy/aurora-sandbox/runtime/src/manifest.mjs` — atomic v1 manifest writer.
- `deploy/aurora-sandbox/runtime/src/tools/{seedream,seedance,openai-images,volc-asr,documents,id-photo,hyperframes,resume,text-artifact}.mjs` — narrow tools.
- `deploy/aurora-sandbox/runtime/test/*.test.mjs` — offline provider/security/manifest/13-skill tests.
- `server/internal/aurora/workflows/*.md` — one reviewed workflow brief per available skill.
- `server/internal/aurora/workflows.go` / `_test.go` — embed and map workflow content.
- `server/internal/aurora/execution_policy.go` / `_test.go` — Go mirror of fixed route/input/output policy.
- Migrations `527`–`537` — provider-run, artifact staging, and asset idempotency/metadata.
- `server/pkg/db/queries/aurora_provider_run.sql` — create-once provider state.
- `server/pkg/db/queries/aurora_artifact_staging.sql` — task-owned staged object state.
- `server/internal/handler/aurora_provider_run.go` / `_test.go` — task-token provider-run API.
- `server/internal/handler/aurora_artifact_upload.go` / `_test.go` — local stream upload and remote URL import.
- `server/internal/daemon/aurora_manifest.go` / `_test.go` — safe manifest/file validation.

### Modified files

- `server/internal/aurora/catalog.go` / `_test.go` — executable input/output policy association and strict format rejection.
- `server/internal/aurora/agents.go` / `_test.go` — seed canonical workflow content/tool requirements.
- `server/internal/handler/aurora.go` / `_test.go` — accept/validate `attachmentIds` and enqueue them.
- `server/internal/handler/aurora_artifact.go` / `_test.go` — staged-ID report, moderation, transaction, idempotency, cleanup.
- `server/internal/handler/file.go` only if shared streaming helpers are extracted; do not weaken chat upload authorization.
- `server/internal/daemon/types.go` — replace artifact URL trust with staging identity/metadata.
- `server/internal/daemon/client.go` — upload/import/provider-run/report clients.
- `server/internal/daemon/daemon.go` — context file, MCP wiring, manifest collect/upload/report.
- `server/internal/daemon/aurora_tool_surface.go` / `_test.go` — allow only named Aurora MCP tools.
- `packages/core/aurora/types.ts`, `schema.ts`, `api.ts`, `mutations.ts`, tests — attachment request and response schema updates.
- `packages/views/aurora/generation-composer.tsx` / `.test.tsx` — upload/select/remove accessible inputs.
- `packages/views/i18n/locales/en/aurora.json` and `zh/aurora.json` — concise attachment/error copy.
- `server/cmd/migrate/main.go` — mark every new concurrent-index migration non-transactional.

---

### Task 1: Accept and Validate Skill-Specific Attachments

**Files:**
- Modify: `server/internal/aurora/catalog.go`
- Modify: `server/internal/aurora/catalog_test.go`
- Create: `server/internal/aurora/execution_policy.go`
- Create: `server/internal/aurora/execution_policy_test.go`
- Modify: `server/internal/handler/aurora.go`
- Modify: `server/internal/handler/aurora_test.go`
- Modify: `packages/core/aurora/types.ts`
- Modify: `packages/core/aurora/schema.ts`
- Modify: `packages/core/aurora/api.ts`
- Modify: `packages/core/aurora/mutations.ts`
- Modify: `packages/core/aurora/api.test.ts`
- Modify: `packages/views/aurora/generation-composer.tsx`
- Modify: `packages/views/aurora/generation-composer.test.tsx`
- Modify: `packages/views/i18n/locales/en/aurora.json`
- Modify: `packages/views/i18n/locales/zh/aurora.json`

**Interfaces:**
- Consumes: Existing authenticated file upload and `EnqueueQuickCreateTask(..., attachmentIDs)`.
- Produces: `CreateAuroraGenerationRequest{skillId,prompt,attachmentIds}`, catalog `attachment_rules`, and `ValidateSkillInputs`.

- [ ] **Step 1: Write the server policy matrix test**

```go
func TestValidateSkillInputsMatrix(t *testing.T) {
    cases := []struct {
        skill string
        kinds []string
        want  bool
    }{
        {"text-image", nil, true},
        {"text-image", []string{"image"}, false},
        {"image-edit", []string{"image"}, true},
        {"image-edit", nil, false},
        {"image-video", []string{"image"}, true},
        {"video-captions", []string{"video"}, true},
        {"document-summary", []string{"document"}, true},
        {"transcription", []string{"audio"}, true},
        {"transcription", []string{"video"}, true},
    }
    for _, tc := range cases { /* call exact validator and assert */ }
}
```

Add cases for all 13 skills, too many files, mixed kinds, unavailable skills, MIME/extension mismatch, missing storage object, and foreign workspace/owner.

- [ ] **Step 2: Write UI/API failures first**

Test that `attachmentIds` serialize, image/document/audio/video accept filters reflect the selected skill, required attachments disable submit, selected files can be removed, upload failure preserves prompt, and success submits only uploaded IDs. Read `packages/ui/docs/button.md` before modifying Button usage.

- [ ] **Step 3: Run focused tests and observe missing attachment support**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora ./internal/handler -run 'TestValidateSkillInputs|TestCreateAuroraGeneration.*Attachment' -count=1
pnpm --filter @multica/core test -- aurora
pnpm --filter @multica/views test -- generation-composer
```

Expected: server and frontend tests fail because generation requests currently contain only `skillId` and `prompt`.

- [ ] **Step 4: Implement one canonical execution policy**

Define exact Go types:

```go
type AttachmentKind string
type AttachmentConstraint struct {
    Kinds    []AttachmentKind
    Min      int
    Max      int
    MaxBytes int64
}
type SkillExecutionPolicy struct {
    SkillID       string
    Route         string
    Attachments   []AttachmentConstraint
    OutputKinds   []string
    RequiredTools []string
}

func ExecutionPolicy(skillID string) (SkillExecutionPolicy, bool)
func ValidateSkillInputs(policy SkillExecutionPolicy, files []db.Attachment) error
```

An `AttachmentConstraint` counts all listed alternative kinds together; transcription therefore uses `{Kinds:[audio,video], Min:1, Max:1}`. Expose the same policy subset on each catalog response as `attachment_rules: [{kinds,min,max,max_bytes}]`. Add `AuroraSkillAttachmentRule` to `packages/core/aurora/types.ts`, parse the field through zod with a default empty array for older servers, and make the composer derive `accept`, required state, and limits from that parsed response rather than a second skill-ID switch.

Populate all 13 routes from the master matrix. Change `AssetKind` to return `(string, bool)` and reject an unlisted format instead of silently using the primary output.

- [ ] **Step 5: Extend the generation request and enqueue call**

Decode with a body limit and reject more than four IDs or duplicate IDs. Resolve every pure UUID with `parseUUIDOrBadRequest`, load files by workspace/user, sniff stored MIME metadata, run `ValidateSkillInputs`, then pass the validated IDs to the final `EnqueueQuickCreateTask` argument instead of `nil`. Complete validation before sandbox ensure, generation insert, or reserve.

- [ ] **Step 6: Add accessible upload controls**

Use the existing authenticated upload API and render one file input whose `accept` and label derive from the policy. Display file name, formatted size, upload state, and a remove button. Do not add descriptions that restate the label. Disable submit while required uploads are missing or any upload is pending/failed. Keep prompt and successful selections on submission failure.

- [ ] **Step 7: Run focused tests**

Run the three commands from Step 3 again. Expected: all selected tests pass.

- [ ] **Step 8: Commit attachment support**

```bash
git add server/internal/aurora/catalog.go server/internal/aurora/catalog_test.go \
  server/internal/aurora/execution_policy.go server/internal/aurora/execution_policy_test.go \
  server/internal/handler/aurora.go server/internal/handler/aurora_test.go \
  packages/core/aurora packages/views/aurora/generation-composer.tsx \
  packages/views/aurora/generation-composer.test.tsx packages/views/i18n/locales/en/aurora.json \
  packages/views/i18n/locales/zh/aurora.json
git commit -m "feat(aurora): accept skill input attachments"
```

### Task 2: Seed 13 Canonical Workflow Briefs and a Narrow Tool Surface

**Files:**
- Create: `server/internal/aurora/workflows.go`
- Create: `server/internal/aurora/workflows_test.go`
- Create: `server/internal/aurora/workflows/poster.md`
- Create: `server/internal/aurora/workflows/xhs-image.md`
- Create: `server/internal/aurora/workflows/product-image.md`
- Create: `server/internal/aurora/workflows/text-image.md`
- Create: `server/internal/aurora/workflows/image-edit.md`
- Create: `server/internal/aurora/workflows/id-photo.md`
- Create: `server/internal/aurora/workflows/image-video.md`
- Create: `server/internal/aurora/workflows/text-video.md`
- Create: `server/internal/aurora/workflows/video-captions.md`
- Create: `server/internal/aurora/workflows/xhs-copy.md`
- Create: `server/internal/aurora/workflows/resume.md`
- Create: `server/internal/aurora/workflows/document-summary.md`
- Create: `server/internal/aurora/workflows/transcription.md`
- Modify: `server/internal/aurora/agents.go`
- Modify: `server/internal/aurora/agents_test.go`
- Modify: `server/internal/daemon/aurora_tool_surface.go`
- Modify: `server/internal/daemon/aurora_tool_surface_test.go`

**Interfaces:**
- Consumes: Task 1’s policy/tool names.
- Produces: `Workflow(skillID) (string, bool)` and a per-task allowlist containing only tools required by that route.

- [ ] **Step 1: Write completeness and denial tests**

Assert every available catalog skill has exactly one workflow, unavailable skills have none, every referenced tool appears in policy, and no workflow contains shell commands, URLs, API keys, provider selection, model selection, or dynamic package installation.

```go
func TestAvailableSkillsHaveCanonicalWorkflows(t *testing.T)
func TestUnavailableSkillsHaveNoWorkflow(t *testing.T)
func TestWorkflowToolsMatchExecutionPolicy(t *testing.T)
func TestAuroraSurfaceDeniesGeneralPurposeTools(t *testing.T)
```

- [ ] **Step 2: Run tests and observe missing workflow bundle**

Run:

```bash
cd server && go test ./internal/aurora ./internal/daemon -run 'TestAvailableSkillsHaveCanonicalWorkflows|TestUnavailableSkills|TestWorkflowTools|TestAuroraSurface' -count=1
```

Expected: missing workflow functions/files.

- [ ] **Step 3: Write each workflow as an exact procedure**

Each Markdown brief states input IDs, the ordered MCP calls, required primary outputs, and failure behavior. Examples:

```text
poster -> aurora.seedream_generate -> require primary image
video-captions -> aurora.volc_asr_transcribe -> aurora.render_video_captions -> require primary video + transcript
resume -> optional aurora.read_document -> aurora.render_resume -> require PDF + Markdown
```

The brief must tell Claude to stop on any tool error, never retry a create tool, and never claim completion without the required artifact IDs returned by tools.

- [ ] **Step 4: Embed and seed canonical content**

Use `//go:embed workflows/*.md`; map by exact filename/skill ID. `EnsureSystemAgents` creates/updates the system skill content from the embedded workflow and records required MCP tool names. Do not load vendor `SKILL.md` files into the model prompt.

- [ ] **Step 5: Build the per-task allowlist**

The daemon derives allowed tools from trusted task `skill_id` plus `ExecutionPolicy`; it never trusts a tool list sent in prompt/context. Always deny Bash, browser/network tools, arbitrary file tools, generic MCP config, and vendor script execution.

- [ ] **Step 6: Run workflow tests**

Run the Step 2 command. Expected: all selected tests pass.

- [ ] **Step 7: Commit workflows**

```bash
git add server/internal/aurora/workflows server/internal/aurora/workflows.go \
  server/internal/aurora/workflows_test.go server/internal/aurora/agents.go \
  server/internal/aurora/agents_test.go server/internal/daemon/aurora_tool_surface.go \
  server/internal/daemon/aurora_tool_surface_test.go
git commit -m "feat(aurora): define canonical skill workflows"
```

### Task 3: Vendor and Harden the Official Volcengine Skills

**Files:**
- Create: `scripts/update-aurora-volc-skills.sh`
- Create: `scripts/verify-aurora-volc-skills.mjs`
- Create: `deploy/aurora-sandbox/vendor/volcengine/skills-lock.json`
- Create: `deploy/aurora-sandbox/vendor/volcengine/vendor-lock.json`
- Create: `deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedance-skill/**`
- Create: `deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedream-skill/**`
- Create: `deploy/aurora-sandbox/vendor/volcengine/patches/*.patch`
- Create: `deploy/aurora-sandbox/runtime/test/volc-vendor-security.test.mjs`

**Interfaces:**
- Consumes: The exact audited source-lock table above.
- Produces: Untouched upstream trees, deterministic inventory, license evidence, and a patch series that yields sandbox-safe adapters.

- [ ] **Step 1: Write the verifier before copying source**

The verifier must reject wrong skill ID/package name/version/license, lock digest mismatch, missing/extra file, changed bytes/mode, unsorted inventory, missing patch digest, non-applying patch, and missing Seedance license text. It recomputes a deterministic whole-tree SHA-256 using the same sorted path/content approach as `server/pkg/skillbundle/hash.go`.

- [ ] **Step 2: Run the verifier against an empty vendor directory**

Run:

```bash
node scripts/verify-aurora-volc-skills.mjs
```

Expected: FAIL with `missing vendor lock`.

- [ ] **Step 3: Add the isolated update script with the requested commands**

The script creates a temporary HOME/project and runs exactly the pinned equivalents:

```bash
npx --yes skills@1.7.0 add https://skills.volces.com/skills/volcengine/agentplan \
  -s byted-ark-seedance-skill --agent claude-code --copy --yes
npx --yes skills@1.7.0 add https://skills.volces.com/skills/volcengine/agentplan \
  -s byted-ark-seedream-skill --agent claude-code --copy --yes
```

It verifies the two audited `computedHash` and `wellKnownDigest` values before replacing the vendor trees. It records retrieval time, `skills` version, source fields, `upstream_revision: null`, canonical skill ID, package name, declared version, SPDX license, per-file path/bytes/mode/SHA-256, whole-tree hash, license hash, patch hashes, reviewer, and security-policy version.

- [ ] **Step 4: Obtain the missing Seedance license before distribution**

Request the exact MIT license text/copyright for Seedance 5.0.0 from the official Volcengine source owner and store it as `byted-ark-seedance-skill/LICENSE.upstream`. The verifier requires its SHA-256 in `vendor-lock.json`. If exact license evidence is unavailable, stop this task and do not publish or embed the Seedance tree in an image.

- [ ] **Step 5: Preserve upstream trees byte-for-byte**

Copy the installed directories without editing them. Run both official offline suites from the vendored directories:

```bash
node --test deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedance-skill/tests/*.test.js
node --test deploy/aurora-sandbox/vendor/volcengine/byted-ark-seedream-skill/tests/*.test.js
```

Expected: both suites pass without credentials or network.

- [ ] **Step 6: Add explicit hardening patches**

Patch output must:

- accept only broker-supplied `ARK_API_KEY`, never argv/generic env/workstation config;
- reject all credential saving and backup/config mutation;
- force exact Ark HTTPS origin, no port/query/fragment/userinfo;
- disable Seedance list/delete/callback/payload-file/task-file/local arbitrary paths/preferences/overrides;
- restrict Seedance model IDs to this plan’s allowlist and omit the inconsistent 1.5 alias;
- use task-scoped state/output paths and UUID filenames with mode `0600`;
- return machine JSON only, sanitize keys/Data URIs/signed URLs/errors, and never log prompt text;
- avoid direct output URL download; return a narrow provider result descriptor to the broker;
- require synchronous polling semantics and nonzero failure for missing task ID, timeout, or missing output;
- cap response/SSE buffers and input Data URIs;
- emit producer metadata for the manifest.

Do not expose the patched CLI directly to Claude.

- [ ] **Step 7: Add security regressions for the patched tree**

Test official-origin lock, HTTP rejection, generic credential rejection, workstation config non-access, no-save behavior, model allowlist, path containment, callback/list/delete absence, buffer caps, token redaction, missing-output failure, UUID output naming, and mode `0600`.

- [ ] **Step 8: Verify source plus patches**

Run:

```bash
node scripts/verify-aurora-volc-skills.mjs
node --test deploy/aurora-sandbox/runtime/test/volc-vendor-security.test.mjs
```

Expected: both commands pass and make no network call.

- [ ] **Step 9: Commit vendor provenance separately**

```bash
git add scripts/update-aurora-volc-skills.sh scripts/verify-aurora-volc-skills.mjs \
  deploy/aurora-sandbox/vendor/volcengine \
  deploy/aurora-sandbox/runtime/test/volc-vendor-security.test.mjs
git commit -m "build(aurora): vendor hardened Volcengine skills"
```

### Task 4: Persist Create-Once Provider Runs

**Files:**
- Create: `server/migrations/527_aurora_provider_run.up.sql` / `.down.sql`
- Create: `server/migrations/528_aurora_provider_run_id_idx.up.sql` / `.down.sql`
- Create: `server/migrations/529_aurora_provider_run_primary_key.up.sql` / `.down.sql`
- Create: `server/migrations/530_aurora_provider_run_task_operation_idx.up.sql` / `.down.sql`
- Create: `server/migrations/531_aurora_provider_run_external_idx.up.sql` / `.down.sql`
- Create: `server/pkg/db/queries/aurora_provider_run.sql`
- Create: `server/internal/handler/aurora_provider_run.go`
- Create: `server/internal/handler/aurora_provider_run_test.go`
- Modify: `server/cmd/migrate/main.go`
- Modify: `server/cmd/server/router.go`
- Modify: `server/internal/daemon/client.go`

**Interfaces:**
- Consumes: Task-scoped `mat_` authentication and Aurora task/generation context.
- Produces: begin, record external ID, finish, and load provider-run operations with one create lease.

- [ ] **Step 1: Write create-once state tests**

Cover first begin, repeated same fingerprint, conflicting fingerprint, crash-before-external-ID ambiguity, external ID record once, conflicting external ID, completion, failure, foreign task, and non-Aurora task.

- [ ] **Step 2: Run tests and observe missing persistence**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run TestAuroraProviderRun -count=1
```

Expected: missing endpoint/query types.

- [ ] **Step 3: Add the provider-run table without inline indexes**

Migration 527:

```sql
CREATE TABLE aurora_provider_run (
    id uuid NOT NULL,
    generation_id uuid NOT NULL,
    task_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    provider text NOT NULL,
    operation text NOT NULL,
    model text NOT NULL,
    request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    external_id text,
    state text NOT NULL CHECK (state IN ('creating', 'submitted', 'succeeded', 'failed', 'ambiguous')),
    error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);
```

Migrations 528 and 530–531 create concurrent unique indexes on `id`, `(task_id, operation)`, and `(provider, external_id) WHERE external_id IS NOT NULL`. Migration 529 attaches the ID index as primary key. Register only the index migrations as non-transactional.

- [ ] **Step 4: Implement task-token endpoints**

Expose:

```text
POST /api/agent/tasks/{taskID}/aurora-provider-runs/begin
PUT  /api/agent/tasks/{taskID}/aurora-provider-runs/{operation}/external
PUT  /api/agent/tasks/{taskID}/aurora-provider-runs/{operation}/finish
GET  /api/agent/tasks/{taskID}/aurora-provider-runs/{operation}
```

The task-token identity must match path task/agent/workspace, task must be Aurora, provider/operation/model must match server policy, and request SHA-256 is over canonical tool arguments excluding credentials. First begin returns `create_allowed=true`. A retry in `submitted` returns its external ID for polling. A retry in `creating` without external ID returns `state=ambiguous` and `create_allowed=false`; it never authorizes a second billable create.

- [ ] **Step 5: Add client calls for the broker-facing local bridge**

The daemon exposes these operations to the MCP process through task-scoped context/token files; the model sees only tool success/failure. Never put the `mat_` token into tool arguments or model-visible context.

- [ ] **Step 6: Generate and test**

Run:

```bash
make sqlc
source .env.worktree
cd server && go test ./internal/handler -run TestAuroraProviderRun -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit provider-run idempotency**

```bash
git add server/migrations/527_aurora_provider_run.* \
  server/migrations/528_aurora_provider_run_id_idx.* \
  server/migrations/529_aurora_provider_run_primary_key.* \
  server/migrations/530_aurora_provider_run_task_operation_idx.* \
  server/migrations/531_aurora_provider_run_external_idx.* \
  server/pkg/db/queries/aurora_provider_run.sql server/pkg/db/generated \
  server/internal/handler/aurora_provider_run.go server/internal/handler/aurora_provider_run_test.go \
  server/internal/daemon/client.go server/cmd/migrate/main.go server/cmd/server/router.go
git commit -m "feat(aurora): persist create-once provider runs"
```

### Task 5: Build the Narrow MCP Broker and Offline Provider Adapters

**Files:**
- Create: `deploy/aurora-sandbox/runtime/package.json`
- Create: `deploy/aurora-sandbox/runtime/src/server.mjs`
- Create: `deploy/aurora-sandbox/runtime/src/policy.mjs`
- Create: `deploy/aurora-sandbox/runtime/src/task-context.mjs`
- Create: `deploy/aurora-sandbox/runtime/src/provider-run.mjs`
- Create: `deploy/aurora-sandbox/runtime/src/manifest.mjs`
- Create: `deploy/aurora-sandbox/runtime/src/tools/*.mjs`
- Create: `deploy/aurora-sandbox/runtime/test/provider-*.test.mjs`
- Modify: `package.json`
- Modify: `pnpm-workspace.yaml`
- Modify: `pnpm-lock.yaml`

**Interfaces:**
- Consumes: Task context file, secret files, provider-run API, hardened vendor adapters, and fixed policy.
- Produces: MCP methods `aurora.seedream_generate`, `aurora.seedance_generate`, `aurora.openai_image`, `aurora.volc_asr_transcribe`, `aurora.read_document`, `aurora.id_photo`, `aurora.render_video_captions`, `aurora.render_resume`, and `aurora.write_text_artifact`.

- [ ] **Step 1: Declare exact dependencies and schemas**

Use exact versions, no ranges:

```json
{
  "private": true,
  "type": "module",
  "dependencies": {
    "@modelcontextprotocol/sdk": "1.30.1",
    "hyperframes": "0.8.75",
    "openai": "7.23.0",
    "zod": "catalog:"
  },
  "scripts": {
    "test": "node --test test/*.test.mjs"
  }
}
```

Add `deploy/aurora-sandbox/runtime` to `pnpm-workspace.yaml`. Use direct exact versions for the runtime-only MCP, HyperFrames, and OpenAI dependencies; use the repository’s existing `catalog:` entry for shared `zod`, with the frozen root lock providing the exact installed version. Keep the official Volcengine vendor trees dependency-free.

- [ ] **Step 2: Write fake-provider contract tests first**

For each tool, start a local fake server and assert exact method/path/headers/body, fixed model/resource, proxy use, timeouts, size limits, sanitized errors, no redirect to an unapproved origin, create-once calls, and manifest/staging output. Include a test proving one provider failure causes no call to another fake provider.

- [ ] **Step 3: Run broker tests and observe missing implementation**

Run:

```bash
pnpm --dir deploy/aurora-sandbox/runtime test
```

Expected: test modules fail to import missing broker/tool files.

- [ ] **Step 4: Implement bounded task context and secret access**

The runner writes a mode-`0400` JSON context containing task/generation/workspace/skill IDs, prompt, authorized attachment ID-to-relative-path/MIME/size mapping, output root, server origin, and task-token file path. Reject unknown fields, files over 1 MiB, symlinks, wrong task IDs, paths outside `/workspace/input`, unsupported attachment kinds, and mismatches with the fixed policy. Secret helper accepts only compiled file paths and trims at most 4 KiB; it never reads generic environment variables or home config.

- [ ] **Step 5: Implement provider adapters without fallback**

- Seedream/Seedance invoke only patched modules with argv arrays and a minimal child environment. The Ark key is in child environment, never argv; stdout/stderr are bounded and sanitized.
- Seedance begins a provider run before create, records `cgt-...` immediately, and resumes polling an existing submitted run rather than creating again.
- OpenAI uses the exact configured model and `b64_json`; generation/edit is selected by server policy for `product-image` and fixed edit for `image-edit`.
- ASR streams base64 from the authorized audio file, uses the fixed flash endpoint/resource ID, and validates `X-Api-Status-Code=20000000` plus bounded JSON.
- FFmpeg extraction validates codecs/duration with ffprobe before ASR; process trees are terminated on timeout.

- [ ] **Step 6: Implement deterministic local tools**

- `read_document`: bounded UTF-8/Markdown read, `pdftotext` for PDF, and fixed DOCX ZIP/XML extraction; return at most 200,000 Unicode characters.
- `id_photo`: fixed ImageMagick/FFmpeg-backed center crop, resize, color-space normalization, and solid-background padding; no model/provider call and no arbitrary expression/filter arguments.
- `render_video_captions`: fixed HyperFrames composition template plus FFmpeg; input is transcript/cue JSON and authorized video ID, never arbitrary source code.
- `render_resume`: fixed escaped HTML template from structured resume sections, Chromium PDF print with network disabled, plus Markdown source.
- `write_text_artifact`: UTF-8 Markdown/text only, max 2 MiB, controlled names.

Every producer calls the manifest library; the model cannot author manifest JSON.

- [ ] **Step 7: Implement Volcengine output import calls**

When hardened Seedream/Seedance returns a provider URL, validate HTTPS syntax and pass it immediately to the task-scoped server importer from Task 6. Do not fetch it in the sandbox, return it to the model, print it, or persist it in local state. Convert the importer response into a `staged_object` manifest source.

- [ ] **Step 8: Run offline broker tests**

Run:

```bash
pnpm install --frozen-lockfile
pnpm --dir deploy/aurora-sandbox/runtime test
```

Expected: all tests pass against local fakes; outbound test hooks record no unknown host.

- [ ] **Step 9: Commit the broker**

```bash
git add deploy/aurora-sandbox/runtime package.json pnpm-workspace.yaml pnpm-lock.yaml
git commit -m "feat(aurora): add narrow sandbox media tools"
```

### Task 6: Stage Local and Remote Artifacts Under Task Ownership

**Files:**
- Create: `server/migrations/532_aurora_artifact_staging.up.sql` / `.down.sql`
- Create: `server/migrations/533_aurora_artifact_staging_id_idx.up.sql` / `.down.sql`
- Create: `server/migrations/534_aurora_artifact_staging_primary_key.up.sql` / `.down.sql`
- Create: `server/migrations/535_aurora_artifact_staging_task_artifact_idx.up.sql` / `.down.sql`
- Create: `server/migrations/536_aurora_asset_metadata.up.sql` / `.down.sql`
- Create: `server/migrations/537_aurora_asset_manifest_idx.up.sql` / `.down.sql`
- Create: `server/pkg/db/queries/aurora_artifact_staging.sql`
- Create: `server/internal/handler/aurora_artifact_upload.go`
- Create: `server/internal/handler/aurora_artifact_upload_test.go`
- Modify: `server/cmd/migrate/main.go`
- Modify: `server/cmd/server/router.go`

**Interfaces:**
- Consumes: Task-token identity, storage service, SSRF-safe URL screening, and artifact limits.
- Produces: task-owned staging rows from streaming upload or controlled remote import.

- [ ] **Step 1: Write upload/import security tests first**

Cover local stream success, foreign task/workspace, non-Aurora task, duplicate artifact ID, MIME mismatch, over-size stream, short/long declared size, hash mismatch, URL credentials, HTTP URL, private/direct/mixed DNS, redirect revalidation, excessive redirects, excessive response bytes, unsupported MIME, timeout, and cleanup after storage/DB failure.

- [ ] **Step 2: Run focused tests and observe missing endpoint**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run 'TestAuroraArtifactUpload|TestAuroraArtifactImport' -count=1
```

Expected: missing routes/types.

- [ ] **Step 3: Add staging and asset metadata schema**

Migration 532 creates `aurora_artifact_staging` without inline indexes: ID, task/generation/workspace IDs, manifest artifact ID, storage object key, name, kind, role, format, MIME, size, SHA-256, metadata JSONB, source type (`upload|provider_import`), status (`staged|committed|deleted`), and timestamps. Migrations 533/535 create concurrent unique ID and `(task_id, manifest_artifact_id)` indexes; 534 attaches the primary key.

Migration 536 adds nullable `manifest_artifact_id`, `name`, `mime_type`, `size_bytes`, `sha256`, `role`, and `metadata jsonb NOT NULL DEFAULT '{}'` to `aurora_asset`. Migration 537 adds `CREATE UNIQUE INDEX CONCURRENTLY aurora_asset_generation_manifest_uidx ON aurora_asset(generation_id, manifest_artifact_id) WHERE manifest_artifact_id IS NOT NULL`.

- [ ] **Step 4: Implement streaming local upload**

Expose `POST /api/agent/tasks/{taskID}/aurora-artifacts/upload` as multipart metadata + file. Authenticate the task token, validate server policy, stream through SHA-256 and byte limiter into storage, sniff the first bounded bytes, and insert staging only after storage succeeds. On DB failure, delete the object. Never read a 500 MiB video fully into memory.

- [ ] **Step 5: Implement SSRF-safe provider import**

Expose `POST /api/agent/tasks/{taskID}/aurora-artifacts/import` with bounded JSON containing manifest artifact metadata and one HTTPS source URL. Resolve and pin public DNS, reject private/special/mixed answers, revalidate at most five redirects, strip Authorization/cookies, stream with per-kind cap, sniff MIME, hash, upload into Multica storage, then return staging ID/metadata. Do not persist the provider URL or include it in logs/errors.

- [ ] **Step 6: Generate and run tests**

Run:

```bash
make sqlc
source .env.worktree
cd server && go test ./internal/handler -run 'TestAuroraArtifactUpload|TestAuroraArtifactImport' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit artifact staging**

```bash
git add server/migrations/532_aurora_artifact_staging.* \
  server/migrations/533_aurora_artifact_staging_id_idx.* \
  server/migrations/534_aurora_artifact_staging_primary_key.* \
  server/migrations/535_aurora_artifact_staging_task_artifact_idx.* \
  server/migrations/536_aurora_asset_metadata.* \
  server/migrations/537_aurora_asset_manifest_idx.* \
  server/pkg/db/queries/aurora_artifact_staging.sql server/pkg/db/generated \
  server/internal/handler/aurora_artifact_upload.go \
  server/internal/handler/aurora_artifact_upload_test.go \
  server/cmd/migrate/main.go server/cmd/server/router.go
git commit -m "feat(aurora): stage task-owned artifacts"
```

### Task 7: Validate Manifests and Report Assets Transactionally

**Files:**
- Create: `server/internal/daemon/aurora_manifest.go`
- Create: `server/internal/daemon/aurora_manifest_test.go`
- Modify: `server/internal/daemon/types.go`
- Modify: `server/internal/daemon/client.go`
- Modify: `server/internal/daemon/daemon.go`
- Modify: `server/internal/handler/aurora_artifact.go`
- Modify: `server/internal/handler/aurora_artifact_test.go`
- Modify: `server/internal/service/aurora_completion.go`

**Interfaces:**
- Consumes: Task context, v1 manifest, Task 6 upload/import APIs, storage-owned staging rows, content moderation, and existing completion/refund logic.
- Produces: `CollectAuroraArtifacts` and idempotent all-or-nothing asset report.

- [ ] **Step 1: Write adversarial manifest tests**

Cover malformed/oversized JSON, schema/version/task/skill/producer mismatch, no primary output, duplicate ID/path/name, absolute/traversal/volume/NUL path, symlink, hard link, device/socket/directory, changed-after-open/hash race, size/hash mismatch, MIME/extension/format mismatch, excessive file/count/total, unsupported metadata depth/size, foreign staging ID, and correct mixed local/staged manifest.

- [ ] **Step 2: Write report idempotency tests**

Prove identical retry creates no duplicate, conflicting retry fails closed, batch DB failure leaves no partial assets, moderation failure commits none and deletes staging objects, and successful report marks all staging rows committed in one transaction.

- [ ] **Step 3: Run tests and observe current URL-trusting behavior**

Run:

```bash
source .env.worktree
cd server && go test ./internal/daemon ./internal/handler -run 'TestAuroraManifest|TestReportTaskArtifacts' -count=1
```

Expected: failures because the daemon has no collector and the handler accepts arbitrary `media_url` and inserts rows one by one.

- [ ] **Step 4: Implement safe local file collection**

Read at most 1 MiB after atomic manifest publication. Open the artifact root directory once; resolve each file with no-follow/openat semantics where supported, reject links/non-regular files and `nlink != 1`, confirm containment from the opened descriptor, stream-recompute size/SHA-256/MIME, and upload through Task 6. Use the manifest only to identify expected files and metadata; never trust its hash/size as proof.

- [ ] **Step 5: Replace URL artifacts with staging identity**

Change daemon wire type to:

```go
type TaskArtifact struct {
    ManifestArtifactID string         `json:"manifest_artifact_id"`
    StagingID          string         `json:"staging_id"`
    Name               string         `json:"name"`
    Kind               string         `json:"kind"`
    Role               string         `json:"role"`
    Format             string         `json:"format"`
    MIMEType           string         `json:"mime_type"`
    SizeBytes          int64          `json:"size_bytes"`
    SHA256             string         `json:"sha256"`
    Metadata           map[string]any `json:"metadata,omitempty"`
}
```

Remove model/daemon-supplied `media_url` from the report contract. The server resolves storage URL/object ownership from staging ID.

- [ ] **Step 6: Make report all-or-nothing and idempotent**

Before the transaction, load all staging rows by task/generation/workspace, validate exact metadata, and moderate each stored object. In one transaction, insert/upsert every asset by `(generation_id, manifest_artifact_id)`, mark staging committed, and reject a conflicting existing hash/metadata. If moderation or transaction fails, delete uncommitted storage objects and route the task to failure/refund once.

- [ ] **Step 7: Integrate daemon completion ordering**

For Aurora tasks:

1. agent process exits successfully;
2. collect/validate manifest;
3. upload local files and resolve staged entries;
4. report artifacts and wait for success;
5. only then report task completion.

Missing/invalid manifest, missing required output, upload/import/report/moderation failure, or successful provider process with no artifact reports task failure and triggers the existing refund path. Clean `/workspace/input` and `/workspace/output` after terminal reporting.

- [ ] **Step 8: Run daemon/handler tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/daemon ./internal/handler ./internal/service -run 'TestAuroraManifest|TestReportTaskArtifacts|TestAurora.*Settlement' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 9: Commit manifest delivery**

```bash
git add server/internal/daemon/aurora_manifest.go server/internal/daemon/aurora_manifest_test.go \
  server/internal/daemon/types.go server/internal/daemon/client.go server/internal/daemon/daemon.go \
  server/internal/handler/aurora_artifact.go server/internal/handler/aurora_artifact_test.go \
  server/internal/service/aurora_completion.go
git commit -m "feat(aurora): validate and commit task artifacts"
```

### Task 8: Prove All 13 Routes with Fake Providers

**Files:**
- Create: `deploy/aurora-sandbox/runtime/test/skill-matrix.test.mjs`
- Create: `server/internal/handler/aurora_skill_matrix_test.go`
- Modify: `server/internal/handler/aurora_fleet_e2e_test.go`
- Modify: `server/internal/daemon/aurora_tool_surface_test.go`

**Interfaces:**
- Consumes: Tasks 1–7 and local fake provider/storage/moderation servers.
- Produces: Canonical route/output/failure matrix with no real provider or agent.

- [ ] **Step 1: Add the Node broker matrix**

For each available skill, execute its exact tool chain with authorized fixture inputs and local fakes, then validate the produced v1 manifest and provider request count. Assertions include:

```text
poster/xhs-image/text-image -> Ark Seedream only
product-image -> OpenAI generation with no image, OpenAI edit with image
image-edit -> OpenAI edit only
id-photo -> local tool only
image-video/text-video -> Ark Seedance create once + poll only
video-captions -> Volc ASR then HyperFrames/FFmpeg
xhs-copy/document-summary -> text artifact tool
resume -> fixed Chromium render tool
transcription -> Volc ASR only
```

- [ ] **Step 2: Add failure/no-fallback cases**

For every external route, make the selected fake return 401, 429, 500, malformed JSON, oversized payload, and timeout. Assert task failure and zero calls to every non-selected provider. Add Seedance ambiguous-create retry and prove it never submits twice.

- [ ] **Step 3: Add server fake lifecycle matrix**

Use `TestApiClient`, database fixtures, a fake managed runner, fake storage/import/moderation, and table cases to create each skill, assert attachments reach the queued task, report expected staging artifacts, reach terminal generation status, debit exact catalog credits on success, and refund exact reserve on failure.

- [ ] **Step 4: Run the all-skill matrix**

Run:

```bash
pnpm --dir deploy/aurora-sandbox/runtime test
source .env.worktree
cd server && go test ./internal/handler -run 'TestAuroraSkillMatrix|TestAuroraFleet' -count=1 -v
```

Expected: 13 success cases plus named failure cases pass; no external host is contacted.

- [ ] **Step 5: Run frontend, backend, and formatting verification**

Run from repository root:

```bash
pnpm typecheck
pnpm lint
pnpm test
make test
git diff --check
```

Expected: every command exits 0. Report any known unrelated backend full-suite failure and its exact focused rerun rather than claiming a clean suite.

- [ ] **Step 6: Verify forbidden surface and source locks**

Run:

```bash
node scripts/verify-aurora-volc-skills.mjs
rg -n 'Bash|WebFetch|WebSearch|skills add|--api-key|media_url' \
  server/internal/aurora/workflows deploy/aurora-sandbox/runtime/src \
  server/internal/daemon/aurora_tool_surface.go
```

Expected: vendor update commands appear only in the update script/plan, no workflow exposes forbidden tools, no credential is passed in argv, and the task artifact report contains no `media_url` input.

- [ ] **Step 7: Commit the matrix**

```bash
git add deploy/aurora-sandbox/runtime/test/skill-matrix.test.mjs \
  server/internal/handler/aurora_skill_matrix_test.go \
  server/internal/handler/aurora_fleet_e2e_test.go \
  server/internal/daemon/aurora_tool_surface_test.go
git commit -m "test(aurora): cover all available skill routes"
```

## Plan C Completion Evidence

Preserve:

- upstream and Multica vendor-lock verification output;
- exact Seedance license evidence;
- official offline Volcengine test output;
- patched security regression output;
- all provider fake request/route counts;
- 13-skill broker and server matrix output;
- manifest/report adversarial and idempotency output;
- frontend type/lint/test and backend test output;
- confirmation that no real agent/provider call, dynamic skill install, or unrestricted shell tool ran in default verification.
