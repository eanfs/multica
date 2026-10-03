# Repository Instructions

Multica is a task management platform where people and agents collaborate on issues. These instructions apply to all coding agents working in this repository.

## Scope and Reading Order

- Before changing `apps/mobile/`, also read [apps/mobile/AGENTS.md](apps/mobile/AGENTS.md), even if your tool does not load nested instructions automatically. Platform-specific sections below apply only to the named platform.
- For naming, translations, or Chinese UI/docs copy, read [conventions.mdx](apps/docs/content/docs/developers/conventions.mdx) and [conventions.zh.mdx](apps/docs/content/docs/developers/conventions.zh.mdx).
- Maintain shared rules here and mobile-specific rules in the mobile file. `CLAUDE.md` files only import them. Update instructions in the same change that alters the referenced workflow or boundary; do not add incident timelines, dependency version lists, or duplicate rules.

## Sharing Rules and Package Boundaries

| Location | Responsibility and constraints |
| --- | --- |
| `server/` | Go backend; Chi, sqlc, WebSocket |
| `packages/core/` | Headless logic, API client, Query hooks, shared Zustand stores. No UI libraries, `react-dom`, `localStorage`, or `process.env`; use `StorageAdapter` for persistence. |
| `packages/ui/` | UI primitives and shared styles. No business logic or `@multica/core` imports. |
| `packages/views/` | Shared web/desktop pages and business components. No store definitions, `next/*`, or `react-router-dom`; use `NavigationAdapter`, `useNavigation()`, and `<AppLink>`. |
| `packages/nextjs/` | Shared Next.js/browser app-shell helpers for web and Aurora. May depend on `@multica/core` and `next/*`; no business UI or app-specific behavior. |
| `apps/web/` | Next.js routes/layouts and web-only UI. App-specific framework integration stays here; shared Next.js app-shell helpers live in `packages/nextjs/`. |
| `apps/aurora/` | Next.js Aurora client: owns routes and Aurora-only UI/wiring; shares platform helpers through `packages/nextjs/`. |
| `apps/desktop/` | Electron and desktop-only UI/state. Application navigation goes through `apps/desktop/src/renderer/src/platform/`. |
| `apps/mobile/` | Independent Expo/React Native client: owns UI, state, hooks, providers, i18n, build, and release. Shares core types and pure utilities, including platform-independent schemas. |
| `apps/docs/` | Fumadocs documentation site |

- Dependency direction is `views -> core + ui` and `nextjs -> core`; core and ui remain independent. Shared packages export raw TypeScript compiled by consuming apps.
- Extract logic used by more than one app into the appropriate shared package: platform-independent logic belongs in `packages/core/`, while shared Next.js/browser app-shell helpers belong in `packages/nextjs/`. Keep app-specific framework and Electron APIs in the app layer; inject platform-specific UI through props/slots.
- Wire shared features into both web routes and the desktop router or overlay. Reuse existing guards/providers such as `DashboardGuard` in `packages/views/layout/`.
- Each workspace declares its directly imported external dependencies. Use `catalog:` for shared dependencies; mobile pins Expo/React Native dependencies in its own manifest.

## Development and Verification

Use `Makefile`, workspace `package.json` files, and `pnpm-workspace.yaml` for current commands and versions. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and worktree operations.

- Use the checkout's managed environment: `make up`, `make status`, `make down`. `make down` preserves data; `make destroy` removes the environment and its data.
- Worktrees share PostgreSQL but have isolated databases/ports. Use the environment scripts and `.env.worktree`; do not copy the main checkout's `.env` or manually create a database through an assumed PostgreSQL instance.
- Regenerate sqlc with `make sqlc` after SQL changes.
- Run the narrowest useful checks while iterating, then broaden when risk warrants it. Report what actually ran and any skipped checks.

Run these from the repository root:

| Scope | Checks |
| --- | --- |
| Frontend excluding mobile | `pnpm typecheck`, `pnpm lint`, `pnpm test` |
| Go backend | `make test` |
| End-to-end | `pnpm exec playwright test` |
| Combined web/backend verification | `make check` |
| Mobile | Commands in [apps/mobile/AGENTS.md](apps/mobile/AGENTS.md#verification) |

Root frontend commands and `make check` do not verify mobile. Docs-only changes can use link/reference checks and `git diff --check`; state that code tests were not run.

## State Rules

- TanStack Query owns API/server data. Zustand owns client state such as filters, drafts, modals, and tab layout; persist only durable preferences/drafts/layout, not server data or ephemeral UI state.
- Web/desktop shared stores live in `packages/core/`. Desktop platform stores remain in desktop; mobile stores remain in mobile. Do not define stores in `packages/views/`.
- On web/desktop, workspace identity is route-driven; platform mirrors exist only for request headers, storage namespaces, and reconnects. React Context is for platform plumbing, not a second server-state store.
- Among stores, only auth/workspace stores may call `api.*` directly; other server interactions belong in queries/mutations.
- Workspace-scoped query keys include `wsId`; account-level keys remain account-scoped. Hooks needing workspace context accept `wsId` unless guaranteed to run under its provider.
- Zustand selectors return stable references; use shallow comparison for allocated objects/arrays.
- WebSocket events patch or invalidate Query caches, not server payloads in Zustand. Clearing client-owned pointers is allowed with one responder and a self-initiated guard when this client can cause the event.
- Optimistic field patches require a predictable result, rare failure, trivial rollback, and staying on the current screen. Snapshot before patching, roll back on failure, and invalidate uncertain projections on settle.
- Create/delete/leave and confirmation flows await the server before navigation or cleanup; do not optimistically delete entities. Exceptions: the existing workspace-leave race noted under Desktop Rules, and mobile inbox mark-read as documented in its instructions.
- Message sends use visible pending state and retry on failure.

## API Compatibility

Installed desktop clients may talk to newer backends. Preserve response compatibility at the API boundary.

- UI-consumed JSON passes through a zod schema and `parseWithFallback`, not an `as T` cast. Web/desktop use `packages/core/api/schema.ts`; mobile uses its own request helpers.
- Provide defaults for optional fields and fallbacks for unknown server enums. Prefer explicit boolean checks; avoid tying critical affordances to a single backend flag when other contract signals are available.
- When adding/changing an endpoint, update its schema and malformed-response tests.

## Database and Migration Rules

- Do not add foreign keys, cascading deletes, or cascading updates. Validate relationships and clean up dependents in application code, using a transaction when the operation must be atomic.
- Every migration-created index, including indexes on new tables, uses `CREATE [UNIQUE] INDEX CONCURRENTLY`. Each concurrent index build gets its own single-statement migration file; the runner executes files outside an explicit transaction.
- Conditionally skipped migrations are still recorded in `schema_migrations`. Later DDL touching conditional objects must be idempotent (`IF EXISTS` / `IF NOT EXISTS`); document recovery if the missing object would break runtime behavior.

## Backend UUID Rules

In `server/internal/handler/`, distinguish UUID sources before using them in writes:

- UUID-or-human-readable resource params: resolve with loaders such as `loadIssueForUser`, `loadSkillForUser`, `loadAgentForUser`, or `requireDaemonRuntimeAccess`, then write using the resolved `entity.ID`.
- Pure UUID request input: `parseUUIDOrBadRequest(w, s, fieldName)`; return immediately when `ok=false`.
- Trusted sqlc/test-fixture round-trips: `parseUUID(s)`, which panics on invalid input.
- Outside handlers: `util.ParseUUID(s)` and check the error.

Workspace-scoped queries filter by `workspace_id`; membership gates access and `X-Workspace-ID` selects the workspace. Assignees are polymorphic: interpret `assignee_id` together with `assignee_type`.

## Desktop Rules

- Workspace session routes are tab destinations. Pre-workspace one-shot flows (create workspace, accept invite) use `WindowOverlay` in `apps/desktop/src/renderer/src/stores/window-overlay-store.ts`, not new routes. Stale workspace tabs heal by dropping stale tab groups.
- Workspace route layouts own `setCurrentWorkspace(slug, uuid)` from `@multica/core/platform`; leaving workspace context calls `setCurrentWorkspace(null, null)`.
- Cross-workspace navigation uses the adapter's `switchWorkspace(slug, targetPath)` flow; do not bypass it with direct router navigation.
- Workspace delete awaits the server. Existing workspace leave clears/navigates first to avoid the `member:removed` race; this is known debt in `packages/views/settings/components/workspace-tab.tsx`, not a pattern for new flows.
- Full-window views outside the dashboard shell mount `<DragStrip />` from `@multica/views/platform` as the first flex child. Interactive controls in the top 48px need `WebkitAppRegion: "no-drag"`.

## UI Copy

- Descriptions are optional and omitted by default. Do not restate titles, labels, values, statuses, or button actions. Add help only for a non-obvious choice, constraint, consequence, or next step; state each fact once beside the relevant control.
- Keep permissions, cost, destructive consequences, execution prerequisites, and error recovery visible when relevant. Put advanced usage and diagnostics in accessible, explicit help. Preserve labels and accessible names; do not move redundant prose wholesale into `sr-only` text.
- Review copy with its surrounding controls and all supported translations, including mobile's independent copy. Follow the UI copy rules in the existing conventions pages; a description prop is not a requirement to write a paragraph.

## Web/Desktop UI Rules

- For Button and Dialog usage, read `packages/ui/docs/button.md` and `packages/ui/docs/dialog.md`. These component contracts also power UI Lab documentation.

- Prefer existing shadcn/Base UI primitives. Add components with `pnpm ui:add <component>`.
- For `pnpm ui:add @reui/<name>`, decline overwrite prompts. Keep `REUI_LICENSE_KEY` in the environment, never in repo files. Adapt vendored primitives into `packages/ui/components/ui/` and compositions into `packages/views/`.
- Use shared semantic tokens in `packages/ui/styles/`. Typography uses the role-named `--text-*` scale in `packages/ui/styles/tokens.css`, not Tailwind's default size ramp.
- Selected states remain identifiable on hover. Handle overflow, long text, and scrolling deliberately; avoid unnecessary local state and dividers.

## Testing

- Tests live beside their implementation: shared logic in core, shared components in views, platform wiring in apps, E2E in `e2e/`, Go tests in server. Do not test shared behavior in app suites.
- Give each behavior one canonical test layer: helper tests own parsing/state matrices; component tests cover wiring, accessibility, happy paths, and named regressions. Prefer a failing regression test before behavioral fixes.
- DOM-free `.test.ts` files start with `// @vitest-environment node`; do not use it if it would silently switch the code under test to an SSR path.
- Views tests must not mock `next/*` or `react-router-dom`. Mock stores with their Zustand callable shape plus `getState`; mock API calls at `@multica/core/api`.
- E2E setup/teardown uses `TestApiClient`.
- DB-backed Go tests use `server/internal/testutil` fixtures (`dbfx.Issue`, `dbfx.Task`, `dbfx.Insert`) and `testutil.Call(h, req).Want(status).JSON(&out)`. Keep product assertions and case-specific diagnostics in the test, not fixture helpers.
- Default tests must not resolve or execute user-installed agent CLIs; pass test-created fake or missing executable paths. New default agent commands go in `scripts/agent-cli-command-names.txt`.
- Only run real-agent smoke tests when explicitly authorized. Gate them behind `agentintegration` and check `MULTICA_RUN_REAL_AGENT_SMOKE=1` before executable lookup/account access. Run the specific test: `(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration ./pkg/agent -run '<test-name>' -count=1 -v)`.

## Change and Delivery Rules

- `origin` (`eanfs/multica`) is the primary repository, not a fork; default PR, issue, merge, and push operations to it. `upstream` (`multica-ai/multica`) is only the upstream source; target it only when explicitly requested. Set `gh repo set-default origin` and use `--repo eanfs/multica` for GitHub operations. Unqualified PR/issue numbers refer to `origin`; use fully qualified internal links such as `pr://eanfs/multica/9` to avoid upstream resolution.
- Keep changes scoped; reuse existing patterns. Code comments are English.
- Do not add internal compatibility shims, dual writes, fallback paths, or legacy adapters unless requested. This does not relax API response compatibility above.
- New global pre-workspace routes use a single word or `/{noun}/{verb}`, not hyphenated root names. Update `server/internal/handler/reserved_slugs.json`, run `pnpm generate:reserved-slugs`, and commit `packages/core/paths/reserved-slugs.ts` when changing reserved slugs.
- Use atomic conventional commits and the repository PR template. For releases, follow [.github/RELEASING.md](.github/RELEASING.md); default to a patch bump unless specified otherwise.
- Open pull requests and issues against the `eanfs/multica` fork (https://github.com/eanfs/multica), not the upstream `multica-ai/multica` org; use `gh pr create --repo eanfs/multica`.

## Aurora Roadmap

Aurora (内容创作应用) is a new domain in this repo, built incrementally from plans under `docs/superpowers/plans/` (specs under `docs/superpowers/specs/`). The plans are the source of truth; scrum stories and tickets are tracked on the `eanfs/multica` fork.

- Implemented: Plan 1 (领域骨架, story #3), Plan 2 (积分账本, story #16), Plan 3.5 (进度与作品库 API, story #31), Plan 4 (前端 `apps/aurora`, story #35), and Plan 3 (执行层, story #23) except Task 6.
- **The Aurora safety and billing milestone is complete.** Plan safety's content moderation (story #61, PR #75) and Plan 5's subscriptions, Stripe billing, monthly settlement, signup bonus, local entitlement gates, and billing UI (story #62, PR #77; migrations `512`–`517`) are merged.
- The intended end-to-end product flow is: create generation → local entitlement check + allowance/reserve → enqueue → managed agent executes → moderated asset writeback + credit settlement/refund → subscription/usage management in `apps/aurora`. The managed sandbox work below is still required to make that execution path operational.
- **Plan 3 Task 6 (#29) remains open with `S2-InProgress`.** Its code-side launch narrowing (`MaxTurns`, reviewed Claude surface, fail-closed provider gating) is merged, and the repository-owned implementation of scoped enrollment, the real daemon claim set, workspace fleet lifecycle, isolation/egress, all 13 available skill routes, artifact staging, and signed multi-architecture images is delivered through Plans A–D (master plan plus four child plans under `docs/superpowers/plans/2026-09-25-aurora-*`), which supersede the previous external-infrastructure ownership boundary. The deterministic boundary is **closed** and verified: Plan D Task 7 Step 4 recorded the final Seedance-inclusive published pair on run 36504132762. The gated real agent/provider smokes (#117, merged as pr://eanfs/multica/170 at `b8bfa9491`) are implemented but have never been executed, because running them needs the repository owner's explicit authorization and provider credentials; on a push to `main` all seven skip, and they are recorded as skipped, never passed and never failed. The Seedance vendor half (#140, merged as pr://eanfs/multica/168 at `92daf28a4`) is resolved, so story #29 and story #23 are closable. Verified evidence is recorded in [`docs/superpowers/plans/2026-09-28-aurora-sandbox-acceptance-record.md`](docs/superpowers/plans/2026-09-28-aurora-sandbox-acceptance-record.md).
- **Plans A and B are implemented, merged, and accepted.** All Plan C leaf tasks are implemented, including both halves of Task 3 (#106): the Seedance and Seedream trees are vendored and hardened, the Volcengine patch series is reviewed, and the remaining skills, attachment rules, workflow briefs, create-once provider runs, sandbox MCP broker, artifact staging, transactional asset report, and the 13-route proof are merged. Plan D Tasks 1–5 are implemented: image input locks with a drift verifier, the sandbox and egress images, the image content verifier plus the containerized smoke, the supply-chain workflow (SBOM/provenance/signing), and the Linux/macOS acceptance scripts. On CI run 36422576420 the "Verify, build, and scan" job (ID 108928656275) is green, including the renamed `Run Linux sandbox security acceptance` step, Syft SBOMs, Trivy scans, and the `Enforce supply-chain release policy` step fixed by #162; the #140 run 36431624767 runs the Linux acceptance matrix twice via `AURORA_DOCKER_SECURITY_COUNT=2`. The sandbox image builds, passes its own content verifier, and both containerized tests pass on a Linux runner. The main-branch publish and its signature/attestation verification are verified end to end on run 36430260728 for the pre-#140 image pair, and #167 (`c12025bc1`) made the publish immune to cancellation by a later merge.
- **Outstanding sandbox work.** (1) #117 Plan D Task 6: the gated real agent and provider smokes are implemented and merged (pr://eanfs/multica/170 at `b8bfa9491`) but not authorized to run. No provider credentials or credential file exist, every provider subtest is recorded as skipped with its gate reason, and a push to `main` skips all seven of them; they remain unexecuted, never passed. Executing them needs the owner's explicit authorization plus the environment, variables and secrets named in that PR, and the Volcengine ARK `claude-*` monthly quota was exhausted when that was recorded but has since reset (verified 2026-10-04: `claude-*` names answer 200, routed to an ARK-hosted model), so the remaining work is deploy-side (rebuild the sandbox image, run a fleet with the endpoint configured), not quota; it is no longer an outstanding acceptance gate. (2) #118 Plan D Task 7: Step 4 is closed by the final Seedance-inclusive published pair on run 36504132762, so #29 and master-plan tasks #85–#88 are closable. (3) The final verified pair is `ghcr.io/eanfs/multica-aurora-sandbox@sha256:d53bcf81b82b1b37c9364327249f3c11e8e24ab0a376905242857cfdfc359e14` and `ghcr.io/eanfs/multica-aurora-egress@sha256:fc885dbf280e03182cd6eb09081841098b8f668eac7eec09f1756328425a12bf`, from run 36504132762 (`b8bfa9491`), alongside that run's published SBOMs and provenance; the earlier pre-#140 pair was verified on run 36430260728. Local `cosign` verification remains impossible on this macOS host (no `cosign`, and GHCR denies anonymous access), so trust comes from the run's own `verify-published` job. (4) The daemon now supports an operator-configured Anthropic-compatible endpoint for the managed Claude child (`ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL`, pr://eanfs/multica/178, merged), but it is not deployed: the sandbox image must be rebuilt to carry the new daemon and a fleet must run with both variables set. The AWS co-located stack runs backend, frontend, aurora and postgres only and has no sandbox fleet, so there is nothing to set them on yet. (5) The Aurora workflow's `Enforce supply-chain release policy` step is failing on `main` again: run 37127134369 blocks `CVE-2026-19534` and `CVE-2026-84961` (`undici 7.29.0`, HIGH) in the sandbox image, a newly published advisory against a pinned dependency and the same class as the earlier `fast-uri` advisories. This is open work, not a flake.
- **Volcengine ARK provider facts (verified).** For Agent Plan keys, the Anthropic-compatible Messages endpoint is `https://ark.cn-beijing.volces.com/api/plan` (`POST /v1/messages`) and the OpenAI-compatible chat endpoint is `https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions`; both answered HTTP 200 with model `ark-code-latest` (Messages accepts either `x-api-key` or `Authorization: Bearer`). The standard platform endpoint `/api/v3` rejects an Agent Plan key with 401, model names outside the plan's own list return 404 `UnsupportedModel`, and `claude-*` names map to an ARK-hosted model and answer 200 while the monthly quota is available, returning 429 `AccountQuotaExceeded` when it is exhausted. Values in `~/.arkcli/.env` are double-quoted: strip the surrounding quotes before use or ARK answers 401 `The API key format is incorrect`, and `arkcli auth status` cannot rotate or fetch the key while its SSO `refresh_token` is invalid.
- **AWS co-location operations.** The `/data/multica` stack (overlay `docker-compose.selfhost.aws.yml`, not committed to this repo) must always be driven with both it and `docker-compose.selfhost.yml`; the base file alone drops the AWS overlay and takes the app down. The domain map is `aod` = Aurora app, `mca` = multica web app, `mcapi` = backend API, `od` = open-design. The instance security group must allow the ALB on every published app port; a missing `:3008` ingress leaves open-design returning 504s while its container stays healthy. The deploy path must never run an untargeted `terraform apply`, because the remote state can carry a module absent from the repo.
- **`image-video` and `text-video` are executable as of #140** (merged as pr://eanfs/multica/168 at `92daf28a4`). The Seedance tree is vendored, patched, and verified, and the broker adapter resolves it, so the video routes no longer fail closed; the runtime suite went from 120 to 121 tests and the vendor security suite from 14 to 15, with `patches/0004-seedance-fail-closed-cli.patch` making a direct Seedance CLI run exit 2. The upstream package ships no LICENSE file, so the committed `LICENSE.upstream` is the standard MIT text carrying the `VolcEngine / AgentPlan` holder from the `SKILL.md` frontmatter `metadata.author: volcengine/agentplan`, byte-identical to the Seedream licence; `vendor-lock.json` records it as an owner-authorized reconstruction (2026-09-28, `provenance.kind: reconstructed-from-declared-license`, `upstream_ships_license: false`) rather than an upstream file, and the strengthened verifier rejects a lock claiming verified. This makes the routes executable; no real provider call has been exercised. The Seedream tree remains vendored and hardened. Slide or deck generation is not among the 13 skills and would need its own plan.
- Cloud entitlement `GateAurora*` integration remains deferred; self-hosted deployments are bounded by Plan 5's local monthly-generation and concurrency gates. Operational billing still requires server-side Stripe secrets and price IDs.
- Of the defects found while building Plan 4, OAuth login-CSRF (#53), the e2e workspace/baseline defects (#57, #58), the duplicated app modules (#54), the degraded-response fallback (#55) and the shared-core workspace-loss relocation (#56) are fixed.
