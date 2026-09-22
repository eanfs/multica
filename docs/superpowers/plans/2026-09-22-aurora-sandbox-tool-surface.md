# Aurora sandbox node — tool-surface narrowing (code side)

Plan 3 Task 6 (issue #29) splits into a repository-side portion and an
external-infrastructure portion. This note records what is implemented in-repo
and where the boundary to external capability lies, so the two halves do not
blur. It is the implementation companion to
[`2026-09-11-aurora-execution.md`](2026-09-11-aurora-execution.md) (Task 6).

## Implemented in-repo

### Additive per-agent tool-surface narrowing (`server/pkg/agent`)

Two new `ExecOptions` fields give callers an opt-in, per-execution override of
the autonomous tool surface. Empty values preserve the historical default, so
ordinary user agents are byte-identical to before:

- `PermissionMode` — overrides the backend's autonomous permission mode. Empty
  keeps `bypassPermissions` (Claude) and its equivalents elsewhere; a sandboxed
  system agent sets `"default"` to opt out of bypass so the deny list below
  actually binds.
- `DisallowedTools` — extra tool names merged into the backend's built-in deny
  list (Claude Code's `--disallowedTools`, comma-joined). A sandboxed system
  agent lists the host-touching tools its provider exposes.

Only the **claude** backend honours these fields today. The mechanism is
provider-agnostic (same opt-in pattern as `ExtraArgs`/`ThinkingLevel`); codex
and the other backends add their provider's equivalent deny flag as they are
onboarded onto the sandbox node.

### Aurora system-agent gating (`server/internal/daemon`)

- The claim endpoint now forwards `agent.system_key` to the daemon
  (`AgentData.SystemKey`). This is additive: user agents (no system key) emit
  no new field, so the wire shape is unchanged for them.
- `isAuroraTask` recognises `aurora:<skillID>` and, for those tasks only, sets
  `ExecOptions.MaxTurns = 30` (MVP turn bound) and applies
  `auroraToolSurface(provider)`. For claude that is permission mode `"default"`
  plus deny-list `Bash`, `WebFetch`, `WebSearch`.

### Tests

- `pkg/agent`: `TestBuildClaudeArgsNarrowsToolSurface`,
  `TestBuildClaudeArgsKeepsDefaultSurfaceWhenNotNarrowed`, and the fake-CLI
  end-to-end `TestClaudeExecuteNarrowsToolSurface` (a fake `claude` records its
  argv; no real agent binary is resolved or executed).
- `internal/daemon`: `TestIsAuroraTask`, `TestAuroraToolSurface`.

## Pending external capability (NOT in this repository)

The sandbox node image and everything that builds, configures, and isolates it
is **external infrastructure** and is not implemented here. It must not be
assumed to exist. Required, still pending:

- **Image build**: `deploy/aurora-sandbox/` — Dockerfile, entrypoint, and image
  build script assembling the Multica daemon binary, a coding agent CLI
  (claude), the media-generation MCP servers (image/transcribe), the
  HyperFrames CLI, Node 22, FFmpeg, and headless Chrome.
- **Isolation**: container/VM-level isolation, an egress allowlist,
  CPU/memory/duration caps, and `MULTICA_AGENT_TIMEOUT` as the daemon-level
  first timeout gate (the server sweeper remains the backstop).
- **Managed registration**: the sandbox daemon registers via the managed-runtime
  endpoint (Plan 3 Task 3) and adds the workspace's managed runtime id to its
  claim set.

The code side only *asserts* the narrowing it can express in launch arguments.
Container isolation, egress control, and resource limits are enforced by the
sandbox host and image, which this repository does not build or verify.

## What the external image must satisfy

Until the image exists, the code-side narrowing is the only in-repo control. The
external build must, at minimum:

- Install **only** providers whose narrowing is defined (`claude` for the MVP).
  A provider that `auroraToolSurface` does not narrow must not be installed on
  the sandbox node, because an un-narrowed surface still runs with
  `bypassPermissions`.
- Run the daemon as non-root or set `IS_SANDBOX=1`, matching the existing
  Claude root/sudo preflight.
- Set `MULTICA_AGENT_TIMEOUT` to the first timeout gate.

## Deferred

- Per-task call-count guard (keyed per task) — Plan 3 follow-up, spec §10.
- Per-skill allow/deny composition and per-provider deny lists beyond claude.
- The HyperFrames "text-to-video" smoke (requires the sandbox image) is gated
  behind `MULTICA_RUN_REAL_AGENT_SMOKE=1` / `-tags=agentintegration`.
