# INCOMPLETE / NOT ACCEPTED — Aurora agent + skill runtime

Task 7 record for [eanfs/multica#205](https://github.com/eanfs/multica/issues/205).

**Overall status: an image generation now reaches `completed` with settlement on a private-network stack; the 13-skill sweep has not been run.**

Run 3 could not reach `completed` at all: asset moderation fetched the media URL server-side and refused a non-public host, so every image generation failed with a refund. Run 4 reaches `completed` only after the owner removed that guard (PR #217) — a deliberate weakening of a security control and a deviation from the isolation plan, recorded below alongside the evidence rather than presented as a clean win. The 13 skills have still not each been run.

| Run | Date | Environment | Code HEAD | What it covers |
| --- | --- | --- | --- | --- |
| Run 4 | 2026-10-10 | `172.16.12.183`, aarch64 | `b5cd14938` | **An image generation reaches `completed`** with a real PNG and a charged settlement, after the artifact fetches' public-address guard was removed. Cost recorded. |
| Run 3 | 2026-10-09 | `172.16.12.183`, aarch64, arm64 images | `adec8cd02` | Full Aurora-entry round trip on digest-pinned production-architecture images. Execution chain verified; moderation blocked the terminal state. |
| Run 2 | 2026-10-09 | Workstation, x86_64 | `c04173034` | Step 1 unit checks; Step 4 gates ①②③. Steps 2/3 NOT RUN. |
| Run 1 | 2026-10-08 | — | `29cfe0ad8…`, `e42f555f…` | Documentation only; no live environment. Steps 2–4 all NOT RUN. |

## Run 4 — 2026-10-10, `172.16.12.183` (aarch64): the first `completed`, and what it cost

Runs 1–3 could not produce a single end-to-end `completed`. This run does, and records why it could not before.

### What was blocking it

The moderator fetches the artifact's media URL **server-side** and screens the bytes locally. Two gates sat in front of that fetch:

1. `parseAssetURL` accepts only an absolute `http(s)` URL with a host. With `LOCAL_UPLOAD_BASE_URL` unset, the stored URL is site-relative (`/uploads/…`), so the report was rejected with `422` — `asset media_url is not a fetchable http(s) URL`.
2. With the base URL set, the fetch reached the dial guard: every resolved address had to be public. On this host the media origin is `172.16.12.183`, so the fetch refused — `asset host "172.16.12.183" resolves to non-public address 172.16.12.183` — `screenImage` returned an error rather than a decision, and the handler answered `500 content moderation unavailable`.

Both are hard-coded. Grepping `internal/aurora/moderation.go` for `os.Getenv` returned nothing: no deployment setting could admit a private origin. Text artifacts were unaffected only because the MVP screen for them validates the URL and never fetches.

The observed failure shape, on this stack, for **every** image skill:

| Generation | Skill | Terminal state | `creditsCharged` |
| --- | --- | --- | --- |
| `07536e8c-3889-459e-8ebb-c2b3596de466` | `poster` | `failed / moderation blocked` | **0** (refunded) |
| `35efe516-f161-466b-83d7-4b59c09bd847` | `text-image` | `failed / moderation blocked` | **0** (refunded) |
| `ee6084bf-abf8-4654-a763-1d71e3f97818` | `text-image` | `failed / moderation blocked` | **0** (refunded) |

In each case the model ran, the provider succeeded, and a real image was staged — `aurora_artifact_staging` recorded `staged`, `2,489,960` bytes, `image/png` for the `poster` run — and only the asset screen refused it.

### The removal

The owner judged the guard over-design and directed its removal. [PR #217](https://github.com/eanfs/multica/pull/217), merge `b5cd14938`, 7 files, +28/−394:

- `isPublicIP`, the `DialContext` address check and the redirect refusal in `newAssetHTTPClient`
- `internal/aurora/artifact_import.go` entirely — `ErrArtifactAddrBlocked`, `ErrArtifactRedirectRefused`, `ArtifactHostResolver`, `ArtifactAddrPolicy`, `IsPublicAddress`, and the guarded `NewArtifactImportClient`
- the `400` mapping for the two removed errors

Kept: `parseAssetURL`'s scheme+host check, the 16 MiB fetch cap, the 25 MP decode cap, the decode checks, the NSFW colour heuristic, and query-string redaction in fetch errors.

### Result: `completed`, with settlement

One image generation, submitted through `POST /api/aurora/generations` on the merged build:

| Field | Value |
| --- | --- |
| generation | `10152bf8-1092-4806-9d92-f9c007f8edc7` |
| skill | `text-image` |
| status | **`completed`** |
| error | `null` |
| `creditsReserved` / `creditsCharged` | `68000000` / **`68000000`** |
| asset | `kind=image`, `format=png`, `/uploads/workspaces/…/aurora-artifacts/…/text-image.png` |

The asset is a real object, fetched through the same URL shape the moderator fetched:

```
$ curl -o act.png -w '%{http_code} %{size_download} %{content_type}' <media_url>
200 5043021 image/png
$ file act.png
PNG image data, 2048 x 2048, 8-bit/color RGB, non-interlaced
```

No `aurora_moderation_log` row was written for this run: the screen passed, and only refusals are logged.

A second run on the merged build (`2960dc2d-a07f-4b99-a33f-c9981acf02c6`, `text-image`) reproduced the same result — `completed`, `creditsCharged=68000000`.

### The settlement round trip

This is what Task 7 asked for, and it is the first time this record can show it. The ledger separates the two outcomes exactly:

| Generation | Ledger | Outcome |
| --- | --- | --- |
| `10152bf8…` (image, completed) | `deduction -68000000`, balance `606000000` — **no refund** | charged |
| `ab6c93f5…` (text, completed) | `deduction -26000000`, balance `674000000` — **no refund** | charged |
| `07536e8c…` (poster, failed) | `deduction -76000000` then `refund +76000000`, balance back to `700000000` | refunded |
| `35efe516…` (image, failed) | `deduction -68000000` then `refund +68000000` | refunded |

Reserve → execute → moderated writeback → charge, and reserve → failure → refund. Both legs observed on the same stack.

### What Run 4 does not establish, and what it costs

- **The 13-skill sweep has still not been run.** One image skill and one text skill have been exercised; the other eleven have not.
- **The `completed` terminal state now depends on a removed security control.** A `media_url` supplied by a sandbox is fetched wherever it points, cloud instance metadata (`169.254.169.254`) included. This is recorded here because the acceptance result cannot be separated from it, not as an endorsement.
- **It is a deliberate deviation from a written plan.** `docs/superpowers/plans/2026-09-25-aurora-sandbox-fleet-isolation.md` §366 requires exactly this rejection, and the skill-runtime plan lists "Implement SSRF-safe provider import" as a completed step. The owner made the call with that consequence stated in the PR and commit.
- **Equivalent guards remain elsewhere** and were not touched: `internal/integrations/wecom/media_guard.go` (which does carry a `MULTICA_WECOM_MEDIA_ALLOW_CIDRS` escape hatch) and `pkg/remotemcp/client.go`.
- `LOCAL_UPLOAD_BASE_URL` is still required for any artifact to pass at all; without it the URL stays site-relative and is rejected outright.

---

## Run 3 — 2026-10-09, `172.16.12.183` (aarch64): the Aurora-entry round trip

Run 2 exercised the local workstation (x86_64). This run builds the images for the **deployment architecture** and drives the whole Aurora chain on it.

### Host and image identity

| Item | Value |
| --- | --- |
| Host | `172.16.12.183` — aarch64, 96 CPUs, Docker 24.0.7, Docker Compose v2.24.1 |
| Why this host | Its architecture matches the AWS target (`t4g.xlarge` / arm64); the workstation is x86_64 |
| Image names | `apexai-mca-fleet`, `apexai-mca-aurora-sandbox` — the ECR repository names from `aws-deploy/multica/build-push.sh` |
| Tag | `v0.5.1-adec8cd02` — the same script's `<release tag>-<short sha>` convention (`git describe` → `v0.5.1`, short sha `adec8cd02`); `latest` is rejected by that script and is not used |
| Code HEAD | `adec8cd02` (the Run 2 branch head) |
| fleet digest | `127.0.0.1:5000/apexai-mca-fleet@sha256:ecf27bf0f24d0533cbc5acee2878cefb3fecf47a14519a9c3cf9c2dca193c1aa` |
| node digest | `127.0.0.1:5000/apexai-mca-aurora-sandbox@sha256:b637775ac793d6cffa9a4fb1dcf72a61c5e9001ac33a2917adb1cc6c30c64eb5` |

The registry is this host's own loopback registry, not ECR: the Fleet requires an image reference whose `RepoDigests` contains the exact `name@sha256:…` string, which a locally built image does not have, and pushing an amd64 workstation build into ECR would consume an immutable production tag with an image the arm64 target cannot run. Nothing was pushed to ECR.

### Build

Built from the repository's own `docker/fleet/Dockerfile` and `docker/runtime/Dockerfile`. Three host-driven adjustments were necessary; the complete diff is:

1. `# syntax=docker/dockerfile:1` removed — this host cannot resolve `registry-1.docker.io`, so BuildKit hangs on the frontend. The directive selects a frontend feature set and does not change image content.
2. `apt` sources rewritten to `mirrors.tuna.tsinghua.edu.cn` — `deb.debian.org` throughput on this host stalled the ~356 MB install.
3. The claude stage's npm registry switched to `registry.npmmirror.com`.

Nothing else was changed: the same base images, the same `go build`/`npm install` steps, and the same final stage as the repository defines. Go modules were vendored on the workstation (`go mod vendor`, 59 MB) so the in-image `go build` needs no module proxy; the base images (`debian:bookworm-slim`, `node:22-bookworm-slim`, `golang:1.26.6-bookworm`, `docker/dockerfile:1`) were transferred from the workstation because this host has no docker.io access.

**The repository's own image contract check passes on the built image:**

```
$ bash check-runtime-image.sh apexai-mca-aurora-sandbox:v0.5.1-adec8cd02
--- PASS: user
--- PASS: entrypoint
--- PASS: healthcheck_uses_fleet_node
--- PASS: env_has_no_credentials
--- PASS: entrypoint_executable /usr/local/bin/multica
--- PASS: entrypoint_executable /usr/local/bin/fleet-node
--- PASS: entrypoint_executable /usr/local/bin/claude
--- PASS: tool_present bash
--- PASS: tool_present curl
--- PASS: tool_present jq
--- PASS: tool_present unzip
--- PASS: tool_present chromium
--- PASS: tool_present ffmpeg
--- PASS: tool_present convert
--- PASS: tool_present pdftoppm
--- PASS: tool_present pdfinfo
--- PASS: layout_owner
--- PASS: claude_runs (2.1.289 (Claude Code))
runtime image contract: ok
```

### Runtime stack

- **PostgreSQL 17** is mandatory: `FleetSchemaVersion` is `SELECT current_setting('server_version_num')::integer >= 170000`, so a 16.x server makes `CheckSchema` return `fleet unavailable`. The first attempt failed exactly this way.
- The Fleet control plane was started through the supported path — `scripts/fleet-env.sh prepare` then `up`, with a private v1 descriptor and the operator built from `server/cmd/fleet-env` — not by hand.
- The API ran with `MULTICA_LOCAL_FLEET_URL`, `MULTICA_LOCAL_FLEET_SECRET_FILE` and `AURORA_RUNTIME_IMAGE` pointing at the digests above.

### Result: the execution chain ran end to end

| Stage | Observation |
| --- | --- |
| Aurora entry | `POST /api/aurora/generations` → **HTTP 201**, `status: queued`, `creditsReserved: 68000000` |
| Fleet provisioning | node container started from the digest-pinned image and reported **healthy** |
| Claim | `picked task agent=文字生成图片 provider=claude issue=""` — an Aurora system agent on an issue-less task |
| Model execution | 8 tool calls; `claude finished status=completed duration=1m32.948s`; `anthropic_base_url_configured=true` |
| Provider call | `aurora_provider_run`: `seedream.generate` **succeeded**, provider `volcengine-agentplan`, model `doubao-seedream-5.0-lite` |
| Artifact import | `aurora_artifact_staging`: **staged**, **5,419,926 bytes**, **image/png** |
| Moderation | `aurora_moderation_log`: scope `asset`, verdict **blocked** — `asset media_url is not a fetchable http(s) URL` |
| Terminal state | generation `failed`, `error: "moderation blocked"`, **`creditsCharged: 0`** |
| Agents | 13 seeded in the workspace |

The model's own narration in the node log shows the ordinary-agent surface doing the work the skill document specifies: `tool #1: Skill` → *"I'll follow the text-image skill steps. Let me start by creating the provider lease."* → `tool #2: Bash` → *"Lease granted. Now call Seedream with the user's prompt."* → further `Bash` calls, an `Read`, and a final artifact import. An earlier run on the same host produced the same shape with 6 tools in 1m08s.

### Verdict against the ticket's own criteria

The issue's failure table says that when **only** moderation blocks — `LOCAL_UPLOAD_BASE_URL` is not reachable by the moderator — the execution chain is a **pass** and end-to-end `completed` must **not** be claimed. That is precisely what happened: the model reached the provider, the provider succeeded, a real PNG was imported into staging, and only the asset moderation step refused it. This record therefore records **execution-chain success** and does **not** claim `completed`.

### Host-driven adjustments (every one recorded, none silent)

| Adjustment | Reason |
| --- | --- |
| A dedicated non-root user (`multica-run`, in the `docker` group) runs the Fleet, the API and their state | `fleet-env.sh` rejects root outright: `need(input.uid === process.getuid() && input.uid > 0)` |
| Registry addressed as `127.0.0.1:5000` | Docker 24 treats `localhost:5000` as HTTPS; `127.0.0.0/8` is the insecure-by-default range |
| `lsof` shim on `PATH` | The host has no `lsof`; `fleet-env.sh` uses it only to test whether a port is free, which the shim answers with `ss` |
| `go` shim on `PATH` | `prepare` shells out to `go run ./cmd/migrate up`; the host has no Go toolchain, so the shim runs the `migrate` binary built from the same revision with the caller's `DATABASE_URL` |
| `docker` shim on `PATH` | Compose v2.24.1 has no `compose start --wait`. The shim strips the flag and polls container health to the same deadline |
| `postgres:17-alpine` instead of the compose file's `pgvector/pgvector:pg17` | pgvector is unused by the migrations, and this host already had a PostgreSQL 17 image; the Fleet only validates compose labels, not the image |
| `start-api.sh` sets `set -a` around the env file | Sourcing a `.env` assigns plain shell variables; without `set -a` the fleet configuration is silently absent from the process environment. This cost real debugging time and is the kind of failure that looks like "the model received no tools". |

### Two product-side findings observed while doing this (outside #205's scope)

1. `server/cmd/server/router.go:406` — when `MULTICA_LOCAL_FLEET_SECRET_FILE` points at a path that does not exist, the server **panics** with `panic: Fleet service key requires a regular file reference` instead of failing closed. Reproduced by clearing the Fleet state directory and restarting the API before `fleet-env.sh` had recreated the key file.
2. `server/cmd/migrate/main.go:836` — `DATABASE_URL` unset silently falls back to `postgres://multica:multica@localhost:5432/multica?sslmode=disable`. Observed on the workstation: `make up` printed `✓ … reachable through DATABASE_URL and migrated` while the intended database was still empty, because the migration had run against a different server.

---

## Run 2 — 2026-10-09: code checks and the three visibility gates

### Snapshot, environment and authorization

| Item | Value |
| --- | --- |
| Code HEAD | `c04173034` — merge of PR #215 (`fix/aurora-brief-213`) into `main` |
| Branch / worktree | `feat/aurora-acceptance-205`, `/Users/lirichen/Work/apexai/multica-205` |
| Managed environment | `multica_205-908`; database `multica_multica_205_908`, 650 migrations applied |
| API | `http://localhost:18988` — `/health` OK, reports commit `c04173034` |
| Web | `http://localhost:13908` |
| Fleet | **not configured**. The workspace's Aurora managed runtime is `offline`; a generation create answers `503 aurora_runtime_unavailable`. |
| Provider authorization | **NOT GRANTED.** No ARK key was used and no provider request was made. |

Steps 2 (M0/M4 round trips) and 3 (13 skill runs) are **NOT RUN**. No real-provider row is PASS.

### Step 1 — unit level

| Command | Actual result | Verdict |
| --- | --- | --- |
| `( cd server && go test ./internal/service -run Aurora -count=1 )` | `ok  github.com/multica-ai/multica/server/internal/service  3.344s`, exit 0 | **PASS** |
| `( cd server && go test ./internal/handler -run Aurora -count=1 )` | `FAIL` — one case only: `--- FAIL: TestAuroraFleetProvisionToClaim (0.05s)` → `aurora_fleet_e2e_test.go:90: ensure workspace sandbox: ensure workspace sandbox: fleet unavailable: private response status 503` | **FAIL, pre-existing** |

Both ran through `make env-exec` so they used the environment's `DATABASE_URL`.

`TestAuroraFleetProvisionToClaim` is **not a regression from this work**: the Run 2 worktree carries **zero source changes** (clean tree at `c04173034`). Localization performed:

- The 205 database holds every `fleet_*` and `aurora_*` table with all 650 migrations applied, so this is not a missing-relation failure.
- `( cd server && go test ./internal/fleet/store -run TestFleetAuroraIntent -count=1 )` → `ok`. The store-level Aurora admission path passes against the same database, so the write path itself is sound.
- The failure is at the service/HTTP layer. The test's in-process Fleet service answers **503**, and `cloudruntime.privateOperationError` (`server/internal/cloudruntime/fleet_internal.go:33`) reports only the status code for a 503, so the Fleet's `error_code` never reaches the test output. `fleet/http.go:60-77` maps every unclassified error to `503 unavailable`.

The root cause therefore remains **not settled**, consistent with the pre-existing red set already recorded in `AGENTS.md` (Aurora leftovers, item 9). No source was changed to make the case pass.

### Step 4 — the three visibility gates

Mode: a real headless Chromium session against `http://localhost:13908`, signed in as `dev@localhost` through the UI, in that user's personal workspace.

The 13 Aurora system agents were materialized by the product path, not by a fixture: `POST /api/aurora/generations` calls `aurora.EnsureSystemAgents` at `server/internal/handler/aurora.go:172` *before* the Fleet check at `:183`, so the seed lands even when the sandbox cannot start. That request answered `503 aurora_runtime_unavailable` — expected without a Fleet — and the 13 agents were created.

#### Gate ① — agent list: **PASS**

`/{ws}/agents` renders **13** agents (`Mine 13` / `All 13`). The extracted page text contains all 13 `aurora.Catalog()` `Name` values and no extras; the three `available: false` catalog entries (`数字人口播`, `PPT 制作`, `Excel 数据分析`) are not agents.

![agents list](../../assets/aurora-acceptance-205/agents-list.png)

#### Gate ② — the agent's skill: **PASS at the data layer; the UI does not render content while the runtime is offline**

- **UI:** `Capabilities → Skills` shows the agent's single skill, `文字生成图片`, labelled *Inherited from runtime*. No skill body is rendered; the pane states *"The local runtime is offline. Reconnect it to refresh inherited skills."* Clicking the row opens nothing.
- **Data layer, all 13:** every agent has exactly one `agent_skill` row whose bound `skill.name` equals the agent's name (13/13). Each `skill.content` is byte-equal to `server/internal/aurora/workflows/<id>.md` apart from one trailing newline — the document ends with one, the stored value does not. The `id`↔`name` mapping came from `GET /api/aurora/skills`.

So "one skill per agent, matching its workflow document" is verified in the database and through the API, **not** in the browser. This build renders inherited skill *content* only while the runtime is connected, and connecting it is exactly the Fleet round trip that is not authorized.

![agent skills tab](../../assets/aurora-acceptance-205/agent-skills-tab.png)

#### Gate ③ — issue-less task rows: **PASS for rendering; the named tab is not where tasks render**

A task row with `issue_id = NULL` renders as:

> **Quick create** · Succeeded · just now · 20s

No blank row and no "unknown issue" text appears anywhere on the page. `GET /api/agents/{id}/tasks` returns the row carrying `"issue_id": ""` and `"kind": "quick_create"`.

Two qualifications:

1. **The row was hand-seeded, not produced by a round trip.** The product path refuses it while the managed runtime is offline: `POST /api/issues/quick-create` answers `422 agent_unavailable / runtime_offline`, and the Aurora generation path stops at the same Fleet check. This gate is about the renderer, so a row with the same shape (`status='completed'`, `issue_id NULL`, `runtime_id` = the workspace's `aurora_managed` runtime) was inserted directly into `agent_task_queue`.
2. **The literal "Work tab" criterion does not match this build.** At `c04173034` the agent detail tabs are Overview / Work / Capabilities / Settings. The **Work tab lists issues** (`Assigned` / `Created`, empty here); agent task rows render in **Overview → Recent work** (`ActivityTab` in `packages/views/agents/components/tabs/activity-tab.tsx`). The criterion's wording predates that split. This record reports what was observed rather than changing the criterion.

A first seed without `completed_at` rendered nothing: the row filter requires `!!t.completed_at` (`activity-tab.tsx:132`). That is the component's own filter, not a defect.

![issue-less recent work row](../../assets/aurora-acceptance-205/agent-recent-work-issue-less.png)

### Environment findings (not product defects)

| Observation | Detail |
| --- | --- |
| `make up C=api,web` failed | `pnpm install` failed in `apps/desktop`'s `electron` postinstall: the environment exports `ELECTRON_MIRROR=https://npm.taobao.org/mirrors/electron/`, whose certificate no longer matches. Installing with `ELECTRON_SKIP_BINARY_DOWNLOAD=1` succeeded. |
| Browser launch failed | The installed Playwright expects `chromium_headless_shell-1208`; the machine's cache holds 1234/1243. The cached 1243 binary was used through `executablePath`. |
| Transient `ENOSPC` | The worktree's `node_modules` is 2.2 GB and the volume ran out mid-run; space recovered without intervention. |
| Browser authentication | The API's cookies (`multica_auth`, `multica_csrf*`) alone are not sufficient. `multica_logged_in` plus a real UI sign-in was required, and the user needed `onboarded_at` set. |
| `bash -l` breaks Go tooling | A login shell resolves an older `/usr/local/go/bin/go` that cannot parse `go 1.26.6` or the `tool` directive. Run Go commands with `bash -c`. |
| `GIT_CONFIG_*` pollution | The ambient environment sets `GIT_CONFIG_COUNT=2` without the matching `GIT_CONFIG_KEY_*`, which breaks every `git` call inside the `internal/daemon/execenv` tests (44 spurious failures). Clearing those variables makes the package pass. |

### What Run 2 does not establish

- No real model call, no ARK request, no image, no import, no manifest, no asset and no ledger change.
- No Fleet node was provisioned; `503 aurora_runtime_unavailable` is the observed and expected answer.
- Gate ②'s browser content view and gate ③'s real producer path stay blocked on the same unauthorized round trip.
- Nothing here proves container isolation, Docker Desktop behaviour, or that the 13 workflows execute.

---

## Run 1 — 2026-10-08 (documentation-only)

Task 7 record for [eanfs/multica#205](https://github.com/eanfs/multica/issues/205), 2026-10-08. **Documentation delivered; operational acceptance not achieved.** Code evidence is not a live round trip. No real provider row below is PASS.

### Snapshot and authority

- Historical tested code HEAD: `29cfe0ad8450ab662fa66f12922c138943decbb0`, before the original documentation-only commit. The controller evidence below remains attached to that snapshot, not to later source fixes or a built image.
- Branch: `chore/aurora-remove-sandbox-202`; worktree: `/tmp/multica-issue-202`.
- M0 existing image digest: **NOT SELECTED / NOT OBSERVED**. M4 unified image digest: **NOT BUILT / NOT SELECTED / NOT OBSERVED** in this acceptance run. No historical digest is substituted.
- The [current plan](<2026-10-08-aurora-agent-skill-runtime.md>), Task 7 brief and continuation rulings supersede stale issue-snapshot names: use `AURORA_RUNTIME_IMAGE`, `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL`; direct node networking, not an egress sidecar or allowlist.
- Tasks 6/6b code was approved. This does not establish DB, migration, API or browser behavior. The catalog has 16 entries: 13 available carriers; `avatar-video`, `ppt`, `excel` remain unavailable and hidden. See [catalog](<../../../server/internal/aurora/catalog.go>). Visibility of 13 agents is **not execution of 13 workflows**.
- [#199](https://github.com/eanfs/multica/issues/199) remains waived: only [text-image](<../../../server/internal/aurora/workflows/text-image.md>) has the executable-document rewrite; **12 rewrites remain missing**. The waiver permits source cleanup, not a claim that those workflows work.

### Authorization and environment

Controller status: **NOT AUTHORIZED / NOT USABLE YET**. Its read-only investigation recorded a stopped shared PostgreSQL container, no reachable database on port 5432, and no registered worktree environment. The startup/isolated-testing authorization request timed out without consent. The final preflight found no `.env.worktree`; no credentials were inspected.

No DB-backed TestMain was executed against default localhost. No migration, DB provisioning, API/Web/Fleet startup, Docker lifecycle, real agent CLI lookup/execution, provider request, AWS operation or spend was performed for Task 7. Optional gated tests were not invoked: an unset opt-in is not proof that package TestMain is safe. No new authorization flags or live-looking fixtures were added.

### Existing code verification, not live acceptance

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

### Final source-prerequisite follow-up

The combined final fix wave repairs the stale text-image network rationale and addresses three pre-existing prerequisites authorized by the continuation scope ruling, not introduced #203/#204 defects:

- The final Debian stage in [Dockerfile](<../../../docker/runtime/Dockerfile>) installs curl; [the image checker](<../../../scripts/check-runtime-image.sh>) requires it on PATH.
- [Managed Fleet setup](<../../../scripts/fleet-env.sh>) accepts the documented ARK HTTPS path prefix and preserves it in the config, while rejecting credentials, queries, fragments, explicit ports, whitespace and ambiguous paths. The separate server-origin validator is unchanged.
- The image checker's credential rejection emits a generic reason, never the image's environment values. Its [hermetic test](<../../../scripts/check-runtime-image.test.sh>) uses synthetic sentinels only.
- [Text-image step 3](<../../../server/internal/aurora/workflows/text-image.md#L45-L48>) explains task-scoped staging and manifest metadata instead of a nonexistent egress allowlist. Import remains mandatory.

Source-fix snapshot: `e42f555fc6743b2c8faa790903d2c4b37641b467` (after original record `dc83302eac48cd0d28f3d25afda701ac5cb90e73`). At that source snapshot, `bash scripts/check-runtime-image.test.sh` passed 3 hermetic cases, `bash scripts/fleet-env.test.sh` passed 38 behavioral cases (35 existing + 3 new) plus its shell preflight, and the guarded Go model tests `TestValidAnthropicBaseURL` and `TestLoadConfigAuroraAnthropicBaseURL` passed. No broader suite was rerun. These are source fixes, not evidence of a built/tested image. The historical test evidence at `29cfe0ad8` above is unchanged. Image build/run/inspect, M0, M4, all 13 real-provider routes, all three browser gates and the full DB-backed suite remain **INCOMPLETE / NOT RUN**. No live authorization or credential lookup occurred.

### Live round trips — both NOT RUN

The following are prescribed future commands, **not commands executed in this record**. They require exact owner authorization and verified setup first.

| Gate | Command / scenario | Status and concrete reason |
| --- | --- | --- |
| M0 | `make up C=api,fleet`; submit prompt-only text-image using an existing image, without building a new image | **NOT RUN**: no authorized managed DB/API/Fleet, no selected existing digest, and no approved real CLI/provider credentials and budget |
| M4 | `MULTICA_RUN_DOCKER_INTEGRATION=1 make env-exec ARGS="-- pnpm exec playwright test --project=fleet-docker"`; repeat on unified image selected by `AURORA_RUNTIME_IMAGE` | **NOT RUN**: no authorized environment or Docker lifecycle, no built/selected unified digest, no approved real CLI/provider call, and no browser trace |

For both M0 and M4, each assertion below is **NOT OBSERVED**: submission/generation ID, task ID and node claim; ordinary Bash and credential availability without disclosure; `begin` response; ARK response/image; successful import and `staging_id`; model-written manifest; daemon collection; committed asset identity/content; moderation result; generation `completed`; ledger reservation/settlement/refund or balance delta. No asset, ledger value, screenshot or trace has been fabricated.

A later run must distinguish failure locations: denied create or ARK 401/429 is a credential/quota issue; connection failure requires deployment DNS/routing/endpoint diagnosis, not a sidecar allowlist. Import 4xx requires fixing workflow payload documentation, not the route. Rejected manifest or unknown staging artifact requires correcting the document against the daemon contract, not weakening validation. Missing Bash or required credential channel is **not a pass**.

If only moderation blocks after a real provider image and successful import, record execution-chain success but **do not claim end-to-end completed**. Record credits problems and continue execution-chain verification where possible without changing billing; billing is not the gate. None of those exceptions can turn an entirely unrun chain into a pass.

### Thirteen real-provider rows — all SKIP

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

### Three separate browser gates — all NOT RUN

These are the M4 product gates in the web app, not substitutes for the round trip. No app was started and no authenticated browser environment was authorized.

| Gate | Required observation | Result / evidence |
| --- | --- | --- |
| Agents | `/{ws}/agents` lists all 13 available Aurora-created agents, named exactly as `aurora.Catalog().Name` | **NOT RUN**; no browser screenshot/record; seed/migration tests were compiled only |
| Skills | An agent's Skills tab shows its corresponding document, matching the embedded workflow | **NOT RUN**; no browser screenshot/record; association code is not browser evidence |
| Work | An agent's Work tab shows all Aurora tasks, including issue-less quick-create tasks, without blank rows or “unknown issue” | **NOT RUN**; component evidence above is not a browser screenshot/record |

All three must pass before “fully connected” can be claimed. If an authorized browser run later shows blank issue-less Work rows, record FAIL and fix rendering rather than weaken the criterion.

### Accepted behavior changes and costs

- Ordinary agent tools with `bypassPermissions` replace the broker-only restricted surface; no second queue, agent type, claim channel or daemon is introduced. Existing runtime API fields and unconfigured generation 503 semantics remain the boundary.
- Docker defaults replace AppArmor/custom seccomp policy; read-only root and `no-new-privileges` remain. The removed isolation matrix has **no replacement** and is not a pending requirement. No claim of verified container isolation or Docker Desktop success is made.
- Provider credentials use container environment, reusing `ANTHROPIC_API_KEY` and `ANTHROPIC_BASE_URL`; speech ASR has its separate channel. Every container process and `docker inspect` can read env values. Secret-bearing Fleet configuration must be 0600, outside Git; never put keys in images, SQL, logs or this record.
- Provider-run create-once/`ambiguous` state is bookkeeping only. Direct model calls can duplicate billable creates; the state no longer enforces prevention.
- The model authors the manifest; the daemon retains route/path/type/link/limit checks and independently verifies size, SHA-256 and MIME. `producer.id` is a model-supplied shape check, **not provenance**.
- Only Volcengine routes remain; product-image/image-edit use Seedream, not OpenAI Images. Twelve executable-document rewrites are still absent. `omp` is intentionally not installed in this phase.
- External AWS deployment-repository changes are out of scope and unverified. Historical broker/sidecar acceptance cannot validate this changed execution surface.

### Owner authorization and setup checklist

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
