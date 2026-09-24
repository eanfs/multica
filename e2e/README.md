# End-to-end suite

Playwright drives the real web app against the real backend and database. These
specs are the only end-to-end gate in the repository, and nothing runs them
automatically — there is no CI job for them. A contributor runs them by hand,
so this file records what a clean local run produces and what each remaining
failure means.

## Running it

```bash
make up                                            # api + web for this checkout
make env-exec ARGS="-- pnpm exec playwright test"  # run with the environment's variables
```

`make env-exec` is the reliable form: the suite reads `.env.worktree` (or
`.env`) through `e2e/env.ts` and takes `baseURL` from `PLAYWRIGHT_BASE_URL`,
then `FRONTEND_ORIGIN`, then `http://localhost:3000`. `playwright.config.ts`
never starts a server — the backend and frontend must already be up.

The suite runs **one worker, no retries**, against the **Next.js dev server**
(`make up` starts `web` in dev mode). Two consequences are worth knowing before
reading a failure:

- Routes compile on first visit, so the first navigation to a surface can take
  seconds. A machine under load (several checkouts, other agents, Docker VMs)
  makes every timeout assertion tighter than it looks.
- `next dev` renders the App Router under React StrictMode, which double-invokes
  effects. Specs that count side effects have to allow the development duplicate.

`e2e/perf/` is a separate scenario with its own config and is ignored here.

## Baseline

On a clean `make up` environment at the commit this file was added, a full run
reports:

| | Count |
| --- | --- |
| Tests | 49 in 19 files |
| Passed | 44 |
| Failed | 5 |
| Duration | ~230 s |

Two of the five failures are `ensureWorkspace` (#57) and clear when it is fixed.
The other three are documented defects in the product or in the spec's
expectations, listed below with the evidence.

Before this change the same run reported **36 passed / 13 failed**. The eight
failures that are now fixed were all stale specs or environment assumptions;
none of them was a product regression.

## Failure dispositions

### Fixed here

| Spec | Was failing because |
| --- | --- |
| `agent-mcp.spec.ts` (2 tests) | The agent detail rail renders its sections as `role="tab"` inside a tablist; the spec still asked for `role="button"`. The creator-only entry is a *Capabilities* sub-tab, so the section has to be opened first, and the `composio_mcp_apps` flag that gates it ships off. |
| `auth-callback-locale.spec.ts` (2 tests) | The spec treated every non-frontend origin as third-party and aborted it. Both `.env` and `.env.worktree` set an absolute `NEXT_PUBLIC_API_URL`, so the browser calls the API on its own origin and the spec aborted the app's own traffic. |
| `chat-attachments.spec.ts` | The seeded `chat_session` predates migration 420: without `explicitly_created_at` the session is not a public Chat, so `/api/upload-file` answered 404. |
| `issue-table.spec.ts` — "groups 1,001 issues exactly" | Asserted the toolbar's `Loaded N of 1001` counter and `Grouping and hierarchy are paused` notice. MUL-5164 (`dd45f3055`) deliberately removed both; the assertions were dropped. |
| `navigation.spec.ts` — "settings page loads via sidebar" | Settings sections are links in the settings rail, not tabs. |
| `settings.spec.ts` — "connecting a Composio toolkit" | The Composio section is gated on `composio_mcp_apps`, which ships off. The spec now patches `/api/config` for itself, like it already mocks the Composio endpoints. |

Specs that need a feature flag patch the real `/api/config` response instead of
asking the environment to turn a product flag on. The flag is off in production
on purpose, and the suite should not be the reason it moves.

### Blocked on #57 (`ensureWorkspace`)

`TestApiClient.ensureWorkspace` returns `workspaces[0]` whenever no workspace
matches the requested slug. Every account gets a personal workspace at signup,
so the named `E2E Workspace <n>` is never created and the tests run against the
personal space instead. These two fail only for that reason:

- `auth.spec.ts` — "logout redirects to /login": opens the workspace menu by the
  name `/E2E Workspace/`, which no workspace has.
- `settings.spec.ts` — "updating workspace name reflects in sidebar immediately":
  reads the current name off the same sidebar button.

Both go green once #57 makes `ensureWorkspace` create or find the requested
workspace. Do not patch them here.

### Known debt

These fail on the current product behaviour, not on a stale selector. They are
recorded here rather than fixed, because each needs a product decision before a
spec can pin it down.

- `navigation.spec.ts` — "sidebar navigation works": after any client-side
  navigation the document head holds **two** `<title>` elements — the stale root
  default first, the correct one second — and `document.title` reads the first.
  The browser tab shows the site title instead of the page name until a full
  reload. Initial loads and reloads are correct, so this is a regression in the
  MUL-6222 tab-naming path.
- `issue-table.spec.ts` — "keeps same-group children nested and cross-group
  children at the group root": asserts the `status:todo` branch reports
  `total: 3`, but the rendered group header reads `Todo 2` and the captured
  response reports `0`. The spec's expected total and the product's branch
  semantics disagree; the API response and the rendered header disagree too.
- `issue-table.spec.ts` — "drops stale branch cursors after a realtime
  sort-boundary update": after moving an issue to the sort boundary, the fresh
  head page returns the same `next_cursor` as the pre-move page, so the stale
  cursor is never invalidated.

## Adding a spec

Follow the testing rules in `AGENTS.md`: setup and teardown go through
`TestApiClient` in `e2e/fixtures.ts`, specs live beside the behaviour they cover
and are not moved into app suites, and the suite creates its own workspace and
issue fixtures. Prefer selectors that describe the control (`getByRole` with the
real role) — the eight fixes above were all a spec drifting from the UI it
described.
