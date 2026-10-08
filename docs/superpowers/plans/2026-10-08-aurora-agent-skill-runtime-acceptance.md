# INCOMPLETE / NOT ACCEPTED — Aurora agent + skill runtime

Task 7 record for [eanfs/multica#205](https://github.com/eanfs/multica/issues/205), 2026-10-08. **Documentation delivered; operational acceptance not achieved.** Code evidence is not a live round trip. No real provider row below is PASS.

## Snapshot and authority

- Historical tested code HEAD: `29cfe0ad8450ab662fa66f12922c138943decbb0`, before the original documentation-only commit. The controller evidence below remains attached to that snapshot, not to later source fixes or a built image.
- Branch: `chore/aurora-remove-sandbox-202`; worktree: `/tmp/multica-issue-202`.
- M0 existing image digest: **NOT SELECTED / NOT OBSERVED**. M4 unified image digest: **NOT BUILT / NOT SELECTED / NOT OBSERVED** in this acceptance run. No historical digest is substituted.
- The [current plan](<2026-10-08-aurora-agent-skill-runtime.md>), Task 7 brief and continuation rulings supersede stale issue-snapshot names: use `AURORA_RUNTIME_IMAGE`, `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL`; direct node networking, not an egress sidecar or allowlist.
- Tasks 6/6b code was approved. This does not establish DB, migration, API or browser behavior. The catalog has 16 entries: 13 available carriers; `avatar-video`, `ppt`, `excel` remain unavailable and hidden. See [catalog](<../../../server/internal/aurora/catalog.go>). Visibility of 13 agents is **not execution of 13 workflows**.
- [#199](https://github.com/eanfs/multica/issues/199) remains waived: only [text-image](<../../../server/internal/aurora/workflows/text-image.md>) has the executable-document rewrite; **12 rewrites remain missing**. The waiver permits source cleanup, not a claim that those workflows work.

## Authorization and environment

Controller status: **NOT AUTHORIZED / NOT USABLE YET**. Its read-only investigation recorded a stopped shared PostgreSQL container, no reachable database on port 5432, and no registered worktree environment. The startup/isolated-testing authorization request timed out without consent. The final preflight found no `.env.worktree`; no credentials were inspected.

No DB-backed TestMain was executed against default localhost. No migration, DB provisioning, API/Web/Fleet startup, Docker lifecycle, real agent CLI lookup/execution, provider request, AWS operation or spend was performed for Task 7. Optional gated tests were not invoked: an unset opt-in is not proof that package TestMain is safe. No new authorization flags or live-looking fixtures were added.

## Existing code verification, not live acceptance

These are **controller-recorded results at the code HEAD above**, not Task 7 reruns. The controller ran full frontend checks once; Task 7 does not repeat them. The CLI guard is [go-test-with-agent-cli-guard.sh](<../../../scripts/go-test-with-agent-cli-guard.sh>).

| Command / evidence | Actual result | What it proves / does not prove |
| --- | --- | --- |
| Guarded root `pnpm typecheck`, `pnpm lint`, `pnpm test` | Each exit 0; combined job `bash-189` exit 0 | Frontend code checks; no live browser or mobile acceptance |
| From server: `go build ./...` | Exit 0 in `bash-190` | Build only |
| Guarded `go test -exec /usr/bin/true ./...` | Exit 0 in `bash-190`, **COMPILE ONLY** | Neither TestMain nor test bodies executed; not a passing backend suite |
| Guarded `go test ./internal/fleet/model ./internal/fleet/docker ./internal/daemon -count=1` | All three hermetic suites passed; combined `bash-190` exit 0 | Actual test execution with guarded CLI surface; not real Docker/provider acceptance |
| Root `test ! -e .env.worktree && GIT_WORK_TREE=/tmp/multica-issue-202 make test ENV_FILE=.env.worktree` | Exit 2 at REQUIRE_ENV: `Missing env file: .env.worktree`; **zero tests executed** | Safe full-backend entrypoint attempt blocked before Docker/SQL/test execution; neither product-test failure nor PASS |
| Task 6b `pnpm --filter @multica/views exec vitest run agents/components/tabs/activity-tab.render.test.tsx agents/components/tabs/activity-tab.test.ts` | Exit 0; 17 tests in 2 files | Work component/helper characterization including issue-less rows; **not browser proof** |

Controller frontend logs were recorded at `/tmp/multica-203-205-{typecheck,lint,test}.log`; no log content or secrets are copied here. The local continuation verification and amended Task 6/6b reports are the provenance of the results above. In particular, the amended Task 6b report supersedes its original all-16 exposure and extra DB opt-in claims: only 13 available carriers are exposed, and that redundant opt-in was removed.

The brief commands below were **NOT RUN** because their DB-backed packages are unauthorized; compilation above does not satisfy them:

```bash
( cd server && go test ./internal/service -run 'Aurora' -count=1 )
( cd server && go test ./internal/handler -run 'Aurora' -count=1 )
```

Billing is untouched, not calculated, not validated and **not an acceptance gate**. The existing completion contract requires at least one committed asset or refunds; no runtime settlement/refund assertion is claimed here. No necessary source defect was demonstrated, so no completion, billing, API or manifest code was changed.

## Final source-prerequisite follow-up

The combined final fix wave repairs the stale text-image network rationale and addresses three pre-existing prerequisites authorized by the continuation scope ruling, not introduced #203/#204 defects:

- The final Debian stage in [Dockerfile](<../../../docker/runtime/Dockerfile>) installs curl; [the image checker](<../../../scripts/check-runtime-image.sh>) requires it on PATH.
- [Managed Fleet setup](<../../../scripts/fleet-env.sh>) accepts the documented ARK HTTPS path prefix and preserves it in the config, while rejecting credentials, queries, fragments, explicit ports, whitespace and ambiguous paths. The separate server-origin validator is unchanged.
- The image checker's credential rejection emits a generic reason, never the image's environment values. Its [hermetic test](<../../../scripts/check-runtime-image.test.sh>) uses synthetic sentinels only.
- [Text-image step 3](<../../../server/internal/aurora/workflows/text-image.md#L45-L48>) explains task-scoped staging and manifest metadata instead of a nonexistent egress allowlist. Import remains mandatory.

Source-fix snapshot: `e42f555fc6743b2c8faa790903d2c4b37641b467` (after original record `dc83302eac48cd0d28f3d25afda701ac5cb90e73`). At that source snapshot, `bash scripts/check-runtime-image.test.sh` passed 3 hermetic cases, `bash scripts/fleet-env.test.sh` passed 38 behavioral cases (35 existing + 3 new) plus its shell preflight, and the guarded Go model tests `TestValidAnthropicBaseURL` and `TestLoadConfigAuroraAnthropicBaseURL` passed. No broader suite was rerun. These are source fixes, not evidence of a built/tested image. The historical test evidence at `29cfe0ad8` above is unchanged. Image build/run/inspect, M0, M4, all 13 real-provider routes, all three browser gates and the full DB-backed suite remain **INCOMPLETE / NOT RUN**. No live authorization or credential lookup occurred.

## Live round trips — both NOT RUN

The following are prescribed future commands, **not commands executed in this record**. They require exact owner authorization and verified setup first.

| Gate | Command / scenario | Status and concrete reason |
| --- | --- | --- |
| M0 | `make up C=api,fleet`; submit prompt-only text-image using an existing image, without building a new image | **NOT RUN**: no authorized managed DB/API/Fleet, no selected existing digest, and no approved real CLI/provider credentials and budget |
| M4 | `MULTICA_RUN_DOCKER_INTEGRATION=1 make env-exec ARGS="-- pnpm exec playwright test --project=fleet-docker"`; repeat on unified image selected by `AURORA_RUNTIME_IMAGE` | **NOT RUN**: no authorized environment or Docker lifecycle, no built/selected unified digest, no approved real CLI/provider call, and no browser trace |

For both M0 and M4, each assertion below is **NOT OBSERVED**: submission/generation ID, task ID and node claim; ordinary Bash and credential availability without disclosure; `begin` response; ARK response/image; successful import and `staging_id`; model-written manifest; daemon collection; committed asset identity/content; moderation result; generation `completed`; ledger reservation/settlement/refund or balance delta. No asset, ledger value, screenshot or trace has been fabricated.

A later run must distinguish failure locations: denied create or ARK 401/429 is a credential/quota issue; connection failure requires deployment DNS/routing/endpoint diagnosis, not a sidecar allowlist. Import 4xx requires fixing workflow payload documentation, not the route. Rejected manifest or unknown staging artifact requires correcting the document against the daemon contract, not weakening validation. Missing Bash or required credential channel is **not a pass**.

If only moderation blocks after a real provider image and successful import, record execution-chain success but **do not claim end-to-end completed**. Record credits problems and continue execution-chain verification where possible without changing billing; billing is not the gate. None of those exceptions can turn an entirely unrun chain into a pass.

## Thirteen real-provider rows — all SKIP

Inputs below are catalog input types, **not submitted test inputs**. For every row: actual input = NOT SUBMITTED; actual executed command = NONE; artifact = NOT OBSERVED; ledger change = NOT OBSERVED. No provider-specific command is invented for the 12 missing rewrites.

| Skill | Catalog input types | Real-provider result | Concrete reason |
| --- | --- | --- | --- |
| poster | text, image | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| xhs-image | text, image | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| product-image | text, image | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| text-image | text | **SKIP** | No authorized managed DB/Fleet or real CLI/provider call; usable ARK key/quota not inspected or approved for this run |
| image-edit | text, image | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| id-photo | image, text | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| image-video | image, text | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| text-video | text | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| video-captions | video, text | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| xhs-copy | text, document | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| resume | text, document | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| document-summary | document, text | **SKIP** | No authorized live environment/CLI/provider budget; executable rewrite missing under #199 waiver |
| transcription | audio, video | **SKIP** | No authorized live environment/CLI/provider budget or ASR setup; executable rewrite missing under #199 waiver |

## Three separate browser gates — all NOT RUN

These are the M4 product gates in the web app, not substitutes for the round trip. No app was started and no authenticated browser environment was authorized.

| Gate | Required observation | Result / evidence |
| --- | --- | --- |
| Agents | `/{ws}/agents` lists all 13 available Aurora-created agents, named exactly as `aurora.Catalog().Name` | **NOT RUN**; no browser screenshot/record; seed/migration tests were compiled only |
| Skills | An agent's Skills tab shows its corresponding document, matching the embedded workflow | **NOT RUN**; no browser screenshot/record; association code is not browser evidence |
| Work | An agent's Work tab shows all Aurora tasks, including issue-less quick-create tasks, without blank rows or “unknown issue” | **NOT RUN**; component evidence above is not a browser screenshot/record |

All three must pass before “fully connected” can be claimed. If an authorized browser run later shows blank issue-less Work rows, record FAIL and fix rendering rather than weaken the criterion.

## Accepted behavior changes and costs

- Ordinary agent tools with `bypassPermissions` replace the broker-only restricted surface; no second queue, agent type, claim channel or daemon is introduced. Existing runtime API fields and unconfigured generation 503 semantics remain the boundary.
- Docker defaults replace AppArmor/custom seccomp policy; read-only root and `no-new-privileges` remain. The removed isolation matrix has **no replacement** and is not a pending requirement. No claim of verified container isolation or Docker Desktop success is made.
- Provider credentials use container environment, reusing `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL`; speech ASR has its separate channel. Every container process and `docker inspect` can read env values. Secret-bearing Fleet configuration must be 0600, outside Git; never put keys in images, SQL, logs or this record.
- Provider-run create-once/`ambiguous` state is bookkeeping only. Direct model calls can duplicate billable creates; the state no longer enforces prevention.
- The model authors the manifest; the daemon retains route/path/type/link/limit checks and independently verifies size, SHA-256 and MIME. `producer.id` is a model-supplied shape check, **not provenance**.
- Only Volcengine routes remain; product-image/image-edit use Seedream, not OpenAI Images. Twelve executable-document rewrites are still absent. `omp` is intentionally not installed in this phase.
- External AWS deployment-repository changes are out of scope and unverified. Historical broker/sidecar acceptance cannot validate this changed execution surface.

## Owner authorization and setup checklist

These are prerequisites for a separately approved run, not authorization granted by this document:

- [ ] Authorize the exact environment, shared DB startup/ownership checks, isolated worktree database, migration scope, API/Web/Fleet/Docker lifecycle and cleanup plan. Use managed setup; do not copy the main checkout environment or default to localhost. Verify identity before any DB use.
- [ ] Select and record the existing M0 image digest without building a replacement. Separately authorize an M4 unified build/selection and record its digest through `AURORA_RUNTIME_IMAGE`. Do not reuse an old published digest as evidence for new code.
- [ ] Authorize real agent CLI and provider calls separately, including provider/model, inputs, budget, quota and retry limits. Docker tests retain `dockerintegration` + `MULTICA_RUN_DOCKER_INTEGRATION=1`; real CLI tests retain `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`. Inspect TestMain and opt-in ordering before any lookup. Flags alone are not consent.
- [ ] Owner supplies valid ARK endpoint/key through the approved private channel; independently arrange ASR credentials if needed. Do not print secrets. Confirm Bash and credential availability without copying values into evidence.
- [ ] Verify `LOCAL_UPLOAD_BASE_URL` is reachable by moderation, plus direct node DNS/routing/provider connectivity. Do not reinstall a removed proxy or weaken the importer/manifest checks.
- [ ] Complete and review the 12 waived workflow rewrites before claiming 13-skill execution. The stale egress-allowlist rationale in [text-image step 3](<../../../server/internal/aurora/workflows/text-image.md#L45-L48>) is repaired by the source-only follow-up above; task-scoped import and the staged-object manifest remain mandatory.
- [ ] Run M0 and M4, record real sanitized commands/inputs, IDs, import/manifest/asset observations, outcomes and trace references. Record ledger observations only if available; do not repair or evaluate billing as a gate.
- [ ] Independently capture all three browser gates, including issue-less Work rows. Replace each NOT RUN/SKIP only with actual evidence; partial moderation/credits exceptions must retain their limits.

No push or PR is part of this delivery. Integration remains for the owner's finishing choice. **Issue #205 real acceptance remains open.**
