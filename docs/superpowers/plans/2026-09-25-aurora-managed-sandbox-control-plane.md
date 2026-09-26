# Aurora Managed Sandbox Control Plane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the global managed-runtime registration shortcut with a single-use, workspace-scoped enrollment protocol and make the real daemon claim and execute exactly one Aurora managed runtime through the reviewed Claude surface.

**Architecture:** The server persists one managed sandbox node per workspace and issues a five-minute `mse_` enrollment secret whose hash is stored with that node. A managed daemon reads the secret from a file, atomically exchanges it for an `mdt_` token and a `claude` execution projection, installs the runtime in its existing in-memory workspace/runtime indexes, and then reuses the normal heartbeat, batch-claim, task report, and deregistration paths with concurrency fixed at one.

**Tech Stack:** Go 1.26.6, Chi, pgx/v5, sqlc, PostgreSQL, existing Multica daemon client and task queue

**Spec:** `docs/superpowers/specs/2026-09-11-aurora-content-creation-app-design.md`

## Global Constraints

- This plan is child plan A of `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md`; use that master plan for shared security and acceptance rules.
- The persisted runtime remains `runtime_mode=cloud` and `provider=aurora_managed`. Only the enrollment response and daemon in-memory runtime use `execution_provider=claude`.
- Enrollment secrets use prefix `mse_`, 20 cryptographically random bytes encoded as 40 lowercase hex characters, SHA-256-at-rest, five-minute expiry, and exactly-once consumption.
- Managed daemon credentials use the existing `mdt_` format, are scoped to the enrolled workspace and daemon ID, and expire at the node’s eight-hour hard lifetime.
- The enrollment endpoint accepts no caller-selected workspace, runtime, daemon, provider, concurrency, or expiry. All identity comes from the token hash lookup.
- A managed process contains exactly one workspace and one runtime, claims with `max_tasks=1`, and rejects an enrollment response that names any execution provider other than `claude` or any concurrency other than `1`.
- Remove the `AURORA_SANDBOX_TOKEN` registration path; do not keep a compatibility endpoint, dual authentication, or fallback to the old global secret.
- Default tests use a fake agent executable and local HTTP servers. They do not invoke a real Claude CLI or external provider.
- Database migrations have no foreign keys or cascades. Explicit indexes are built concurrently in their own single-statement migration files.

## File Structure

### New files

- `server/migrations/518_aurora_sandbox_node.up.sql` / `.down.sql` — node/enrollment lifecycle table without indexes.
- `server/migrations/519_aurora_sandbox_node_id_idx.up.sql` / `.down.sql` — concurrently built unique ID index.
- `server/migrations/520_aurora_sandbox_node_primary_key.up.sql` / `.down.sql` — attach/drop the primary-key constraint using migration 519’s index.
- `server/migrations/521_aurora_sandbox_node_workspace_idx.up.sql` / `.down.sql` — one node row per workspace.
- `server/migrations/522_aurora_sandbox_node_runtime_idx.up.sql` / `.down.sql` — one node row per runtime.
- `server/migrations/523_aurora_sandbox_node_daemon_idx.up.sql` / `.down.sql` — one node row per daemon identity.
- `server/migrations/524_aurora_sandbox_node_enrollment_idx.up.sql` / `.down.sql` — unique non-null enrollment-token hash.
- `server/migrations/525_aurora_sandbox_node_reap_idx.up.sql` / `.down.sql` — lifecycle scan index.
- `server/migrations/526_aurora_managed_runtime_workspace_idx.up.sql` / `.down.sql` — one `aurora_managed` runtime per workspace.
- `server/pkg/db/queries/aurora_sandbox_node.sql` — sqlc lifecycle operations and atomic token consumption.
- `server/internal/aurora/sandbox_enrollment.go` — enrollment token generation, issuance, consumption, daemon-token creation, and runtime binding.
- `server/internal/aurora/sandbox_enrollment_test.go` — service transaction and replay/expiry tests.
- `server/internal/daemon/managed.go` — managed bootstrap validation and in-memory workspace/runtime installation.
- `server/internal/daemon/managed_test.go` — bootstrap and claim-set unit tests.
- `server/internal/handler/aurora_runtime_test.go` — enrollment HTTP contract tests.
- `server/internal/handler/aurora_managed_lifecycle_test.go` — actual managed-daemon code path with a fake agent.

### Modified files

- `server/internal/auth/jwt.go` — add `GenerateManagedEnrollmentToken`.
- `server/internal/auth/jwt_test.go` — assert token format and entropy-safe uniqueness.
- `server/pkg/db/queries/aurora_agents.sql` — load a managed runtime whether unbound or bound.
- `server/internal/aurora/agents.go` — rely on the unique managed-runtime invariant.
- `server/cmd/migrate/main.go` — execute migrations 519, 521–526 outside explicit transactions where they build indexes.
- `server/internal/handler/aurora_runtime.go` — replace global registration with single-use enrollment.
- `server/internal/handler/handler.go` — receive `*aurora.SandboxEnrollmentService`.
- `server/cmd/server/router.go` — route `POST /api/daemon/managed/enroll`; remove `/register`.
- `server/cmd/server/main.go` — construct the enrollment service; remove global sandbox token config.
- `server/internal/config/config.go` — remove `AuroraSandboxToken` if that field lives in central config.
- `server/internal/daemon/client.go` — add enrollment request and typed response.
- `server/internal/daemon/config.go` — add managed-mode settings and fail-closed validation.
- `server/internal/daemon/daemon.go` — bootstrap before loops, skip workstation sync in managed mode, keep one claim slot, and report managed readiness.
- `server/cmd/multica/cmd_daemon.go` — accept managed foreground flags without human profile/config discovery.
- `server/internal/daemon/aurora_tool_surface.go` — consume the execution provider, never `aurora_managed`, for launch review.
- `.env.example` — remove `AURORA_SANDBOX_TOKEN`.
- `docs/superpowers/plans/2026-09-11-aurora-execution.md` — record only the control-plane portion as completed after this plan passes.

## Public Interfaces

```go
// server/internal/aurora/sandbox_enrollment.go
const (
    ManagedExecutionProvider = "claude"
    ManagedMaxConcurrency    = 1
)

type SandboxNodeIdentity struct {
    NodeID      pgtype.UUID
    WorkspaceID pgtype.UUID
    RuntimeID   pgtype.UUID
    DaemonID    string
}

type IssuedEnrollment struct {
    Identity  SandboxNodeIdentity
    Token     string
    ExpiresAt time.Time
}

type ConsumedEnrollment struct {
    Identity             SandboxNodeIdentity
    DaemonToken          string
    DaemonTokenExpiresAt time.Time
}

type SandboxEnrollmentService struct { /* private dependencies */ }

func NewSandboxEnrollmentService(pool *pgxpool.Pool, q *db.Queries, now func() time.Time) *SandboxEnrollmentService
func (s *SandboxEnrollmentService) Issue(ctx context.Context, workspaceID, runtimeID pgtype.UUID, imageDigest string) (IssuedEnrollment, error)
func (s *SandboxEnrollmentService) Consume(ctx context.Context, rawToken string) (ConsumedEnrollment, db.AgentRuntime, error)
```

```go
// server/internal/daemon/client.go
type ManagedEnrollmentResponse struct {
    WorkspaceID         string    `json:"workspace_id"`
    DaemonID            string    `json:"daemon_id"`
    Runtime             Runtime   `json:"runtime"`
    ExecutionProvider   string    `json:"execution_provider"`
    MaxConcurrency      int       `json:"max_concurrency"`
    DaemonToken         string    `json:"daemon_token"`
    DaemonTokenExpiresAt time.Time `json:"daemon_token_expires_at"`
}

func (c *Client) EnrollManaged(ctx context.Context, enrollmentToken string) (ManagedEnrollmentResponse, error)
```

```go
// server/internal/daemon/config.go
type ManagedConfig struct {
    Enabled             bool
    EnrollmentTokenFile string
}

func (d *Daemon) bootstrapManaged(ctx context.Context) error
func (d *Daemon) installManagedEnrollment(resp ManagedEnrollmentResponse) error
```

---

### Task 1: Persist One Scoped Sandbox Node per Workspace

**Files:**
- Create: `server/migrations/518_aurora_sandbox_node.up.sql`
- Create: `server/migrations/518_aurora_sandbox_node.down.sql`
- Create: `server/migrations/519_aurora_sandbox_node_id_idx.up.sql`
- Create: `server/migrations/519_aurora_sandbox_node_id_idx.down.sql`
- Create: `server/migrations/520_aurora_sandbox_node_primary_key.up.sql`
- Create: `server/migrations/520_aurora_sandbox_node_primary_key.down.sql`
- Create: `server/migrations/521_aurora_sandbox_node_workspace_idx.up.sql`
- Create: `server/migrations/521_aurora_sandbox_node_workspace_idx.down.sql`
- Create: `server/migrations/522_aurora_sandbox_node_runtime_idx.up.sql`
- Create: `server/migrations/522_aurora_sandbox_node_runtime_idx.down.sql`
- Create: `server/migrations/523_aurora_sandbox_node_daemon_idx.up.sql`
- Create: `server/migrations/523_aurora_sandbox_node_daemon_idx.down.sql`
- Create: `server/migrations/524_aurora_sandbox_node_enrollment_idx.up.sql`
- Create: `server/migrations/524_aurora_sandbox_node_enrollment_idx.down.sql`
- Create: `server/migrations/525_aurora_sandbox_node_reap_idx.up.sql`
- Create: `server/migrations/525_aurora_sandbox_node_reap_idx.down.sql`
- Create: `server/migrations/526_aurora_managed_runtime_workspace_idx.up.sql`
- Create: `server/migrations/526_aurora_managed_runtime_workspace_idx.down.sql`
- Create: `server/pkg/db/queries/aurora_sandbox_node.sql`
- Create: `server/internal/aurora/sandbox_enrollment_test.go`
- Modify: `server/pkg/db/queries/aurora_agents.sql`
- Modify: `server/cmd/migrate/main.go`

**Interfaces:**
- Consumes: Existing `agent_runtime` rows with `provider='aurora_managed'` and `runtime_mode='cloud'`.
- Produces: Generated `db.AuroraSandboxNode` plus create, lock, consume, bind, touch, drain, stop, and reap-candidate queries.

- [ ] **Step 1: Write the failing database invariant test**

Add a DB-backed test that creates one workspace/runtime/node, then proves duplicate `workspace_id`, `runtime_id`, and `daemon_id` writes fail and that an expired or consumed enrollment hash is not returned by the consumable-token query.

```go
func TestAuroraSandboxNodeUniquenessAndConsumption(t *testing.T) {
    ctx := context.Background()
    pool := testutil.OpenDB(t)
    q := db.New(pool)
    ws, owner := dbfx.Workspace(t, pool), dbfx.User(t, pool)
    require.NoError(t, EnsureSystemAgents(ctx, q, ws.ID, owner.ID))
    runtimeID, err := ManagedRuntimeID(ctx, q, ws.ID)
    require.NoError(t, err)

    node := db.CreateAuroraSandboxNodeParams{
        ID:                  testutil.UUID(),
        WorkspaceID:         ws.ID,
        RuntimeID:           runtimeID,
        DaemonID:            "aurora-" + uuid.NewString(),
        ImageDigest:         "ghcr.io/eanfs/multica-aurora@sha256:" + strings.Repeat("a", 64),
        State:               "starting",
        EnrollmentTokenHash: pgtype.Text{String: auth.HashToken("mse_one"), Valid: true},
        EnrollmentExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
    }
    _, err = q.CreateAuroraSandboxNode(ctx, node)
    require.NoError(t, err)
    _, err = q.CreateAuroraSandboxNode(ctx, duplicateWithNewID(node))
    require.Error(t, err)
}
```

- [ ] **Step 2: Run the focused test and observe the missing generated API**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora -run TestAuroraSandboxNodeUniquenessAndConsumption -count=1
```

Expected: compilation fails because `db.CreateAuroraSandboxNodeParams` and its queries do not exist.

- [ ] **Step 3: Add the table migration without inline indexes**

Use this exact state model in migration 518:

```sql
CREATE TABLE aurora_sandbox_node (
    id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    runtime_id uuid NOT NULL,
    daemon_id text NOT NULL,
    backend_node_id text,
    image_digest text NOT NULL,
    state text NOT NULL CHECK (state IN ('starting', 'online', 'draining', 'stopped', 'failed')),
    enrollment_token_hash text,
    enrollment_expires_at timestamptz,
    enrollment_consumed_at timestamptz,
    last_active_at timestamptz NOT NULL DEFAULT now(),
    drain_started_at timestamptz,
    started_at timestamptz,
    stopped_at timestamptz,
    failure_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((enrollment_token_hash IS NULL) = (enrollment_expires_at IS NULL))
);
```

Migration 518 down drops the table. Do not add foreign keys.

- [ ] **Step 4: Add each index in its own migration**

Use these exact statements in migrations 519 and 521–526:

```sql
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_pkey ON aurora_sandbox_node (id);
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_workspace_uidx ON aurora_sandbox_node (workspace_id);
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_runtime_uidx ON aurora_sandbox_node (runtime_id);
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_daemon_uidx ON aurora_sandbox_node (daemon_id);
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_enrollment_uidx ON aurora_sandbox_node (enrollment_token_hash) WHERE enrollment_token_hash IS NOT NULL;
CREATE INDEX CONCURRENTLY aurora_sandbox_node_reap_idx ON aurora_sandbox_node (state, last_active_at) WHERE state IN ('starting', 'online', 'draining');
CREATE UNIQUE INDEX CONCURRENTLY agent_runtime_aurora_managed_workspace_uidx ON agent_runtime (workspace_id) WHERE runtime_mode = 'cloud' AND provider = 'aurora_managed';
```

Migration 520 attaches `aurora_sandbox_node_pkey` as the table primary key with `ALTER TABLE ... ADD CONSTRAINT ... PRIMARY KEY USING INDEX ...`. Its down migration drops the constraint; migration 519’s down is `SELECT 1;` because dropping the primary-key constraint already drops the attached index. The other index down migrations use one `DROP INDEX CONCURRENTLY IF EXISTS` statement each.

- [ ] **Step 5: Register concurrent migrations with the runner**

Add 519, 521, 522, 523, 524, 525, and 526 to the migration runner’s non-transactional/concurrent-index set. Keep 518 and 520 transactional.

- [ ] **Step 6: Add exact lifecycle queries**

Define sqlc queries with these names and semantics:

```sql
-- name: CreateAuroraSandboxNode :one
-- insert every identity/lifecycle field and return the row

-- name: GetAuroraSandboxNodeByWorkspace :one
-- WHERE workspace_id = $1

-- name: LockAuroraSandboxNodeByWorkspace :one
-- WHERE workspace_id = $1 FOR UPDATE

-- name: ConsumeAuroraSandboxEnrollment :one
UPDATE aurora_sandbox_node
SET enrollment_consumed_at = now(), enrollment_token_hash = NULL,
    enrollment_expires_at = NULL, state = 'online', started_at = COALESCE(started_at, now()),
    last_active_at = now(), updated_at = now()
WHERE enrollment_token_hash = $1
  AND enrollment_expires_at > now()
  AND enrollment_consumed_at IS NULL
  AND state = 'starting'
RETURNING *;

-- name: RotateAuroraSandboxEnrollment :one
-- set a fresh hash/expiry, clear consumed/failure, and set state='starting'

-- name: BindAuroraManagedRuntime :one
-- update the exact runtime_id/workspace_id/provider/mode row with daemon_id, online status, and last_seen_at

-- name: TouchAuroraSandboxNode :exec
-- update last_active_at only for matching workspace_id + daemon_id in online/draining state

-- name: MarkAuroraSandboxNodeDraining :one
-- online -> draining, first drain_started_at wins

-- name: MarkAuroraSandboxNodeStopped :one
-- set stopped, timestamps, clear enrollment fields and backend_node_id

-- name: ListAuroraSandboxNodesForReap :many
-- bounded ordered rows from starting/online/draining using last_active_at and created_at cutoffs
```

Change `GetAuroraManagedRuntime` so it no longer requires `daemon_id IS NULL`; uniqueness comes from migration 526.

- [ ] **Step 7: Generate sqlc and run migration/query tests**

Run:

```bash
make sqlc
source .env.worktree
cd server && go test ./internal/aurora ./cmd/migrate -run 'TestAuroraSandboxNode|Test.*Migration' -count=1
```

Expected: all selected tests pass, and generated code contains every query named above.

- [ ] **Step 8: Commit the schema atomically**

```bash
git add server/migrations/518_aurora_sandbox_node.* \
  server/migrations/519_aurora_sandbox_node_id_idx.* \
  server/migrations/520_aurora_sandbox_node_primary_key.* \
  server/migrations/521_aurora_sandbox_node_workspace_idx.* \
  server/migrations/522_aurora_sandbox_node_runtime_idx.* \
  server/migrations/523_aurora_sandbox_node_daemon_idx.* \
  server/migrations/524_aurora_sandbox_node_enrollment_idx.* \
  server/migrations/525_aurora_sandbox_node_reap_idx.* \
  server/migrations/526_aurora_managed_runtime_workspace_idx.* \
  server/pkg/db/queries/aurora_sandbox_node.sql \
  server/pkg/db/queries/aurora_agents.sql server/pkg/db/generated \
  server/internal/aurora/sandbox_enrollment_test.go server/cmd/migrate/main.go
git commit -m "feat(aurora): persist scoped sandbox nodes"
```

### Task 2: Issue and Consume Single-Use Enrollment Credentials

**Files:**
- Create: `server/internal/aurora/sandbox_enrollment.go`
- Modify: `server/internal/aurora/sandbox_enrollment_test.go`
- Modify: `server/internal/auth/jwt.go`
- Modify: `server/internal/auth/jwt_test.go`

**Interfaces:**
- Consumes: Task 1’s node queries, `auth.GenerateDaemonToken`, `auth.HashToken`, and `db.CreateDaemonToken`.
- Produces: `SandboxEnrollmentService.Issue` and `.Consume` using the types in this plan’s Public Interfaces section.

- [ ] **Step 1: Add failing token-format and service tests**

Cover all of these cases in named tests:

```go
func TestGenerateManagedEnrollmentTokenFormat(t *testing.T)
func TestSandboxEnrollmentIssueCreatesStartingNode(t *testing.T)
func TestSandboxEnrollmentIssueRotatesStoppedNode(t *testing.T)
func TestSandboxEnrollmentConsumeMintsScopedDaemonTokenAndBindsRuntime(t *testing.T)
func TestSandboxEnrollmentConsumeRejectsExpiredToken(t *testing.T)
func TestSandboxEnrollmentConsumeRejectsReplay(t *testing.T)
func TestSandboxEnrollmentConsumeRollsBackWhenRuntimeBindingFails(t *testing.T)
```

The success test must query the stored daemon token by hash and assert its workspace and daemon ID match the consumed node. It must also assert the managed runtime’s persisted provider is still `aurora_managed`.

- [ ] **Step 2: Run the focused tests and observe missing functions**

Run:

```bash
source .env.worktree
cd server && go test ./internal/auth ./internal/aurora -run 'TestGenerateManagedEnrollment|TestSandboxEnrollment' -count=1
```

Expected: compilation fails on `GenerateManagedEnrollmentToken` and `NewSandboxEnrollmentService`.

- [ ] **Step 3: Add the enrollment token generator**

Implement a shared random-hex helper or mirror the existing daemon-token implementation exactly:

```go
func GenerateManagedEnrollmentToken() (string, error) {
    b := make([]byte, 20)
    if _, err := rand.Read(b); err != nil {
        return "", fmt.Errorf("generate managed enrollment token: %w", err)
    }
    return "mse_" + hex.EncodeToString(b), nil
}
```

Do not accept any other prefix in `Consume`.

- [ ] **Step 4: Implement issuance under a workspace advisory lock**

`Issue` must:

1. Reject an image reference that does not contain `@sha256:` followed by exactly 64 lowercase hex characters.
2. Acquire `pg_advisory_xact_lock(hashtextextended(workspaceID::text, 0))` in a transaction.
3. Verify `runtimeID` is the workspace’s `aurora_managed` cloud runtime.
4. Generate node ID and daemon ID `aurora-<node UUID without braces>` on first issue.
5. Generate one `mse_` token, store only `auth.HashToken(raw)`, and set expiry to `now()+5m`.
6. Rotate the enrollment fields on a `stopped`, `failed`, or stale `starting` row while preserving node/runtime/daemon identity and clearing prior `started_at`, `stopped_at`, drain, backend, and failure fields.
7. Return `ErrSandboxNodeAlreadyActive` for `online` or `draining`; it must not mint a second token.
8. Commit before returning the raw token.

Use `time.Now` only through the injected `now` function so expiry tests are deterministic.

- [ ] **Step 5: Implement one-transaction consumption**

`Consume` must:

1. Reject malformed tokens before a database call.
2. Hash the raw token and call `ConsumeAuroraSandboxEnrollment` in a transaction.
3. Generate one `mdt_` token and insert its hash with workspace/node daemon identity and `expires_at=now()+8h`; the same transaction sets this launch’s `started_at=now()`.
4. Bind only the node’s managed runtime to its daemon ID and mark it online.
5. Roll back token consumption if daemon-token creation or runtime binding fails.
6. Return the raw daemon token once; never persist or log it.

Map no-row outcomes to one `ErrInvalidManagedEnrollment` error so callers do not distinguish unknown, expired, and replayed secrets.

- [ ] **Step 6: Run focused and package tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/auth ./internal/aurora -run 'TestGenerateManagedEnrollment|TestSandboxEnrollment' -count=1
go test ./internal/aurora -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit the enrollment service**

```bash
git add server/internal/auth/jwt.go server/internal/auth/jwt_test.go \
  server/internal/aurora/sandbox_enrollment.go server/internal/aurora/sandbox_enrollment_test.go
git commit -m "feat(aurora): issue scoped sandbox enrollments"
```

### Task 3: Replace Global Registration with Enrollment Exchange

**Files:**
- Modify: `server/internal/handler/aurora_runtime.go`
- Create: `server/internal/handler/aurora_runtime_test.go`
- Modify: `server/internal/handler/handler.go`
- Modify: `server/cmd/server/router.go`
- Modify: `server/cmd/server/main.go`
- Modify: `server/internal/config/config.go`

**Interfaces:**
- Consumes: `SandboxEnrollmentService.Consume`.
- Produces: `POST /api/daemon/managed/enroll` with no request body and a `ManagedEnrollmentResponse` JSON body.

- [ ] **Step 1: Write the failing HTTP contract matrix**

Use `testutil.Call` and table cases for missing bearer, malformed prefix, expired token, replay, service disabled, and success. The success assertion must be exact:

```go
var got struct {
    WorkspaceID          string          `json:"workspace_id"`
    DaemonID             string          `json:"daemon_id"`
    Runtime              RuntimeResponse `json:"runtime"`
    ExecutionProvider    string          `json:"execution_provider"`
    MaxConcurrency       int             `json:"max_concurrency"`
    DaemonToken          string          `json:"daemon_token"`
    DaemonTokenExpiresAt time.Time       `json:"daemon_token_expires_at"`
}

testutil.Call(h, req).
    Want(http.StatusOK).
    JSON(&got)
require.Equal(t, "claude", got.ExecutionProvider)
require.Equal(t, 1, got.MaxConcurrency)
require.Equal(t, workspaceID, got.WorkspaceID)
require.Equal(t, runtimeID, got.Runtime.ID)
require.True(t, strings.HasPrefix(got.DaemonToken, "mdt_"))
```

Also assert that a JSON body attempting to supply `workspace_id` is ignored because the endpoint decodes no caller identity.

- [ ] **Step 2: Run the handler test and observe the old route/contract**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run TestManagedRuntimeEnroll -count=1
```

Expected: FAIL because the handler still expects the global token and workspace body, and no `/enroll` route exists.

- [ ] **Step 3: Replace the handler implementation**

Implement:

```go
func (h *Handler) ManagedRuntimeEnroll(w http.ResponseWriter, r *http.Request) {
    if h.SandboxEnrollment == nil {
        writeFeatureDisabled(w, "aurora_sandbox_not_configured", "managed sandbox enrollment is not configured")
        return
    }
    consumed, runtime, err := h.SandboxEnrollment.Consume(r.Context(), middleware.BearerToken(r))
    if errors.Is(err, aurora.ErrInvalidManagedEnrollment) {
        writeError(w, http.StatusUnauthorized, "invalid managed enrollment")
        return
    }
    if err != nil {
        writeError(w, http.StatusInternalServerError, "failed to enroll managed sandbox")
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "workspace_id": uuidToString(consumed.Identity.WorkspaceID),
        "daemon_id": consumed.Identity.DaemonID,
        "runtime": runtimeToResponse(runtime),
        "execution_provider": aurora.ManagedExecutionProvider,
        "max_concurrency": aurora.ManagedMaxConcurrency,
        "daemon_token": consumed.DaemonToken,
        "daemon_token_expires_at": consumed.DaemonTokenExpiresAt,
    })
}
```

Log node/workspace/runtime/daemon IDs only. Never log either raw token.

- [ ] **Step 4: Replace the route and server wiring**

Register `POST /api/daemon/managed/enroll` outside `DaemonAuth`, delete `/api/daemon/managed/register`, inject the service through `Handler`, and remove `AuroraSandboxToken` loading from server config. A missing fleet integration may leave the service available for tests but must not restore a shared token.

- [ ] **Step 5: Prove old authentication no longer works**

Add an assertion that setting `AURORA_SANDBOX_TOKEN` and presenting its value still receives 401. Verify the removed route returns 404.

- [ ] **Step 6: Run handler tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run 'TestManagedRuntimeEnroll|TestManagedRuntimeRegisterRemoved' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit the endpoint replacement**

```bash
git add server/internal/handler/aurora_runtime.go server/internal/handler/aurora_runtime_test.go \
  server/internal/handler/handler.go server/cmd/server/router.go server/cmd/server/main.go \
  server/internal/config/config.go
git commit -m "feat(aurora): exchange managed enrollment credentials"
```

### Task 4: Bootstrap a Real Daemon in Managed Mode

**Files:**
- Create: `server/internal/daemon/managed.go`
- Create: `server/internal/daemon/managed_test.go`
- Modify: `server/internal/daemon/client.go`
- Modify: `server/internal/daemon/client_test.go`
- Modify: `server/internal/daemon/config.go`
- Modify: `server/internal/daemon/config_test.go`
- Modify: `server/internal/daemon/daemon.go`
- Modify: `server/cmd/multica/cmd_daemon.go`

**Interfaces:**
- Consumes: Task 3’s HTTP response; existing `Client.SetToken`, `workspaceState.runtimeIDs`, `Daemon.runtimeIndex`, heartbeat, batch claim, and report methods.
- Produces: `Client.EnrollManaged`, `Config.Managed`, `Daemon.bootstrapManaged`, and `Daemon.installManagedEnrollment`.

- [ ] **Step 1: Write client and bootstrap failures first**

Add tests that assert:

```go
func TestClientEnrollManagedSendsBearerWithoutJSONIdentity(t *testing.T)
func TestInstallManagedEnrollmentAddsOneWorkspaceAndRuntime(t *testing.T)
func TestInstallManagedEnrollmentRejectsWrongProvider(t *testing.T)
func TestInstallManagedEnrollmentRejectsConcurrencyOtherThanOne(t *testing.T)
func TestInstallManagedEnrollmentRejectsIdentityMismatch(t *testing.T)
func TestManagedConfigRejectsMissingOrOversizedTokenFile(t *testing.T)
func TestManagedModeDoesNotDiscoverWorkstationWorkspaces(t *testing.T)
```

The successful install must assert `d.allRuntimeIDs()` equals only the enrolled runtime ID and `d.findRuntime(id).Provider == "claude"` even though the response’s persisted runtime projection contains `provider=aurora_managed`.

- [ ] **Step 2: Run focused tests and observe missing managed mode**

Run:

```bash
cd server && go test ./internal/daemon -run 'TestClientEnrollManaged|TestInstallManaged|TestManagedConfig|TestManagedMode' -count=1
```

Expected: compilation fails on the new config and methods.

- [ ] **Step 3: Implement the enrollment client**

`EnrollManaged` must create a request-local client or request header carrying `Bearer <mse_...>` without replacing the client’s long-lived token until the response has passed validation. It sends `POST /api/daemon/managed/enroll` with an empty body and decodes the typed response. After successful validation, `bootstrapManaged` calls `SetToken(resp.DaemonToken)` exactly once.

- [ ] **Step 4: Add managed config validation**

Extend daemon config with `Managed ManagedConfig`. In managed mode:

- require an absolute enrollment token file path;
- require it to be a regular file, no symlink, owner-readable, no group/other permission bits, and at most 256 bytes;
- require the trimmed content to match `^mse_[0-9a-f]{40}$`;
- force `MaxConcurrentTasks=1`, `KeepEnvAfterTask=false`, and workspace GC on;
- require a discovered/provided Claude executable but skip every non-Claude agent probe;
- reject local profile, legacy daemon IDs, desktop launch mode, and background start.

Do not read a user CLI config or workstation home directory in managed mode.

- [ ] **Step 5: Install the enrolled identity into existing daemon state**

`installManagedEnrollment` validates all response fields, then creates one `workspaceState` and one in-memory `Runtime`:

```go
func (d *Daemon) installManagedEnrollment(resp ManagedEnrollmentResponse) error {
    if resp.ExecutionProvider != "claude" || resp.MaxConcurrency != 1 {
        return ErrInvalidManagedEnrollmentResponse
    }
    runtime := runtimeFromResponse(resp.Runtime)
    runtime.Provider = resp.ExecutionProvider
    runtime.DaemonID = resp.DaemonID
    // Under d.mu: install one workspace with []string{runtime.ID}, then
    // d.runtimeIndex[runtime.ID] = &runtime. Reject non-empty prior state.
    return nil
}
```

The implementation must compare runtime workspace ID with top-level workspace ID and reject a persisted runtime provider other than `aurora_managed` or mode other than `cloud`.

- [ ] **Step 6: Branch daemon startup before workstation registration/sync**

At daemon startup, managed mode must:

1. read and validate the token file;
2. enroll once;
3. validate/install the response;
4. set the returned `mdt_` token and daemon identity;
5. start existing heartbeat, wakeup/batch poller, execution, report, and deregistration loops;
6. skip user login/token refresh, normal daemon registration, local workspace sync, remote workspace discovery, agent rediscovery, auto-update, repo cache initialization, local skill scanning, and desktop notifications.

Do not add a second claim loop; reuse the existing batch path with the installed runtime ID and one slot.

- [ ] **Step 7: Expose a foreground-only CLI entry**

Add these flags to `multica daemon start`:

```text
--managed
--managed-enrollment-token-file=/run/secrets/aurora-enrollment
--foreground
```

`--managed` requires `--foreground`; background profile paths and PID/log management remain for workstation daemons only. The sandbox image will use this command directly.

- [ ] **Step 8: Run daemon package tests**

Run:

```bash
cd server && go test ./internal/daemon ./cmd/multica -run 'TestClientEnrollManaged|TestInstallManaged|TestManagedConfig|TestManagedMode' -count=1
```

Expected: all selected tests pass and no test resolves a user-installed Claude executable.

- [ ] **Step 9: Commit managed mode**

```bash
git add server/internal/daemon/managed.go server/internal/daemon/managed_test.go \
  server/internal/daemon/client.go server/internal/daemon/client_test.go \
  server/internal/daemon/config.go server/internal/daemon/config_test.go \
  server/internal/daemon/daemon.go server/cmd/multica/cmd_daemon.go
git commit -m "feat(daemon): add managed sandbox bootstrap"
```

### Task 5: Enforce Carrier-to-Execution Provider Separation

**Files:**
- Modify: `server/internal/daemon/aurora_tool_surface.go`
- Modify: `server/internal/daemon/aurora_tool_surface_test.go`
- Modify: `server/internal/daemon/managed.go`
- Modify: `server/internal/daemon/daemon.go`

**Interfaces:**
- Consumes: Task 4’s in-memory runtime with `Provider="claude"` and the task’s persisted Aurora context.
- Produces: Fail-closed launch review that never treats `aurora_managed` as an executable provider.

- [ ] **Step 1: Add a regression test for the exact identity split**

```go
func TestManagedAuroraTaskUsesClaudeToolSurface(t *testing.T) {
    persisted := Runtime{ID: runtimeID, Provider: "aurora_managed", RuntimeMode: "cloud"}
    installed := installForTest(t, persisted, "claude")
    surface, err := auroraToolSurface(installed.Provider)
    require.NoError(t, err)
    require.Equal(t, "claude", installed.Provider)
    require.NotContains(t, surface.AllowedTools, "Bash")
}

func TestAuroraManagedIsNotAnExecutableProvider(t *testing.T) {
    _, err := auroraToolSurface("aurora_managed")
    require.Error(t, err)
}
```

- [ ] **Step 2: Run the regression test**

Run:

```bash
cd server && go test ./internal/daemon -run 'TestManagedAuroraTaskUsesClaudeToolSurface|TestAuroraManagedIsNotAnExecutableProvider' -count=1
```

Expected before the fix: the managed runtime either reaches launch as `aurora_managed` and is rejected, or the install helper is missing.

- [ ] **Step 3: Make execution identity an enrollment-only value**

Ensure all provider-dependent launch, executable lookup, version reporting, argument construction, permission mode, deny list, and tool-surface review use the in-memory `claude` value. Ensure every server authorization, claim query, node query, and database row continues to use persisted runtime ID/workspace/daemon identity and never rewrites the database provider to `claude`.

- [ ] **Step 4: Preserve the deny list**

The effective Claude launch must still deny general `Bash`, `WebFetch`, `WebSearch`, arbitrary MCP servers, and user-installed skills. Later provider tools are added as explicit MCP names in child plan C; this task must not broaden them.

- [ ] **Step 5: Run the complete Aurora surface tests**

Run:

```bash
cd server && go test ./internal/daemon -run 'Aurora|Managed' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 6: Commit the identity boundary**

```bash
git add server/internal/daemon/aurora_tool_surface.go \
  server/internal/daemon/aurora_tool_surface_test.go \
  server/internal/daemon/managed.go server/internal/daemon/daemon.go
git commit -m "fix(aurora): separate carrier and execution providers"
```

### Task 6: Make Managed Readiness and Shutdown Observable

**Files:**
- Modify: `server/internal/daemon/daemon.go`
- Modify: `server/internal/daemon/health.go`
- Modify: `server/internal/daemon/health_test.go`
- Modify: `server/internal/daemon/client.go`
- Modify: `server/internal/handler/aurora_runtime.go`
- Modify: `server/internal/handler/aurora_runtime_test.go`
- Modify: `server/pkg/db/queries/aurora_sandbox_node.sql`

**Interfaces:**
- Consumes: Existing local daemon health server and runtime deregistration.
- Produces: Managed `/health` readiness fields, node activity touches, and token/runtime revocation on graceful shutdown.

- [ ] **Step 1: Write lifecycle tests**

Add tests for these exact outcomes:

```go
func TestManagedHealthStartsUnreadyUntilEnrollmentAndHeartbeat(t *testing.T)
func TestManagedHealthReportsOneRuntimeAndNoCredential(t *testing.T)
func TestManagedHeartbeatTouchesNodeActivity(t *testing.T)
func TestManagedShutdownRevokesDaemonTokensAndMarksRuntimeOffline(t *testing.T)
```

Health JSON may include node/workspace/runtime/daemon IDs and last successful heartbeat time. It must not include enrollment or daemon tokens, secret-file paths, prompts, provider URLs, or task-token values.

- [ ] **Step 2: Run tests and observe missing lifecycle state**

Run:

```bash
source .env.worktree
cd server && go test ./internal/daemon ./internal/handler -run 'TestManagedHealth|TestManagedHeartbeat|TestManagedShutdown' -count=1
```

Expected: tests fail because health and deregistration do not update the managed node.

- [ ] **Step 3: Extend managed heartbeat handling**

After a successful authenticated runtime heartbeat, call `TouchAuroraSandboxNode(workspaceID, daemonID)`. A failed touch logs identifiers and the error but must not turn a successful runtime heartbeat into a credential leak or duplicate claim.

- [ ] **Step 4: Extend graceful deregistration**

A managed shutdown request authenticated by its `mdt_` token must in one server transaction:

1. mark its bound runtime offline and clear `daemon_id`;
2. mark its node `stopped`, clear enrollment/backend fields, and retain audit timestamps;
3. delete every daemon token for that workspace/daemon;
4. return the deleted token hashes so `DaemonTokenCache.Invalidate` executes immediately.

An abrupt container death is handled by child plan B’s sweeper, not by inventing a client-side success.

- [ ] **Step 5: Expose health based on acknowledged control-plane state**

Managed health becomes ready only after enrollment validation and one acknowledged heartbeat. It becomes unready on token expiry, repeated heartbeat authorization failure, draining signal, or context cancellation. Keep the listener loopback-only.

- [ ] **Step 6: Run lifecycle tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/daemon ./internal/handler -run 'TestManagedHealth|TestManagedHeartbeat|TestManagedShutdown' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit lifecycle observability**

```bash
git add server/internal/daemon/daemon.go server/internal/daemon/health.go \
  server/internal/daemon/health_test.go server/internal/daemon/client.go \
  server/internal/handler/aurora_runtime.go server/internal/handler/aurora_runtime_test.go \
  server/pkg/db/queries/aurora_sandbox_node.sql server/pkg/db/generated
git commit -m "feat(aurora): track managed node lifecycle"
```

### Task 7: Prove the Real Managed Daemon Lifecycle with a Fake Agent

**Files:**
- Create: `server/internal/handler/aurora_managed_lifecycle_test.go`
- Modify: `server/internal/daemon/daemon_test.go`
- Modify: `scripts/agent-cli-command-names.txt`

**Interfaces:**
- Consumes: Tasks 1–6, existing quick-create enqueue, daemon claim, fake executable injection, heartbeat, progress, completion, and settlement.
- Produces: One canonical non-provider end-to-end regression for enroll → claim → fake execute → report → complete.

- [ ] **Step 1: Write a process-level test using a test-created executable**

The test must:

1. create user/workspace/system agents/runtime and an issued enrollment;
2. enqueue an Aurora quick-create task;
3. write a temporary fake Claude executable that emits the minimum supported successful stream/result;
4. start the real daemon object in managed mode against `httptest.Server` using the token file and fake executable path;
5. wait on observable task status, not a fixed sleep;
6. assert enrollment consumed once, claim request contains exactly the runtime ID and `max_tasks=1`, execution starts once, completion reaches the server, and runtime/node identity remains workspace-scoped;
7. cancel the daemon and assert graceful shutdown revokes the token and marks runtime/node offline/stopped.

Name the test:

```go
func TestAuroraManagedDaemonEnrollsClaimsAndCompletesWithFakeClaude(t *testing.T)
```

- [ ] **Step 2: Run the new test and capture the first failing seam**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run TestAuroraManagedDaemonEnrollsClaimsAndCompletesWithFakeClaude -count=1 -v
```

Expected before final wiring: FAIL at the earliest missing lifecycle seam; it must not skip because no real Claude CLI exists.

- [ ] **Step 3: Add only the test seam needed to inject the fake executable**

Pass the test-created executable through daemon configuration. Do not search `PATH`, `$HOME`, or user agent config in this test. If the fake command name is a new default command token, add it to `scripts/agent-cli-command-names.txt`; otherwise use an absolute temporary path and leave the list unchanged.

- [ ] **Step 4: Make startup and shutdown condition-driven**

Use channels/HTTP observations/task-row polling with a bounded context. Do not add arbitrary sleeps. Timeout diagnostics must include current task, runtime, node, and daemon health state but no tokens.

- [ ] **Step 5: Run the lifecycle test three times**

Run:

```bash
source .env.worktree
cd server && go test ./internal/handler -run TestAuroraManagedDaemonEnrollsClaimsAndCompletesWithFakeClaude -count=3 -v
```

Expected: three passes, one claim and one fake execution per run.

- [ ] **Step 6: Commit the canonical lifecycle regression**

```bash
git add server/internal/handler/aurora_managed_lifecycle_test.go \
  server/internal/daemon/daemon_test.go scripts/agent-cli-command-names.txt
git commit -m "test(aurora): cover managed daemon lifecycle"
```

### Task 8: Remove the Shared-Token Contract and Verify Plan A

**Files:**
- Modify: `.env.example`
- Modify: `docs/superpowers/plans/2026-09-11-aurora-execution.md`
- Modify: `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md` only to tick Plan A after verification

**Interfaces:**
- Consumes: Every deliverable in this child plan.
- Produces: No documented or compiled `AURORA_SANDBOX_TOKEN` contract and verified control-plane evidence for child plan B.

- [x] **Step 1: Remove obsolete configuration documentation**

Delete `AURORA_SANDBOX_TOKEN` from templates and examples. Document only that managed enrollment is issued internally by the server and delivered by the authenticated fleet API from child plan B; operators never configure an enrollment token manually.

- [x] **Step 2: Scan for the removed contract and forbidden compatibility paths**

Run:

```bash
rg -n 'AURORA_SANDBOX_TOKEN|/api/daemon/managed/register|ManagedRuntimeRegister' \
  --glob '!docs/superpowers/plans/2026-09-25-*.md' \
  --glob '!docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md' .
```

Expected: no matches in executable code, tests, active configuration docs, or environment templates. Historical plan prose may describe the removed gap.

- [x] **Step 3: Run code generation and focused suites**

Run:

```bash
make sqlc
source .env.worktree
cd server && go test ./internal/auth ./internal/aurora ./internal/daemon ./internal/handler ./cmd/multica ./cmd/migrate -count=1
```

Expected: all packages pass; no test invokes a real agent.

- [x] **Step 4: Run backend verification**

Run from the repository root:

```bash
make test
git diff --check
```

Expected: both commands exit 0. If known unrelated full-suite flakes occur, rerun the exact failing test, report both outputs, and do not claim the suite passed.

- [x] **Step 5: Update plan status narrowly**

Record that scoped enrollment and managed daemon bootstrap are complete. Keep fleet isolation, skill runtime, image supply chain, and issue #29 marked pending.

- [x] **Step 6: Commit documentation cleanup**

```bash
git add .env.example docs/superpowers/plans/2026-09-11-aurora-execution.md \
  docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md
git commit -m "docs(aurora): record managed control plane"
```

## Plan A Completion Evidence

Plan A is implemented on top of origin/main `20f3d5972`: scoped enrollment and managed daemon bootstrap (Tasks 1–7, merged through PRs #120–#126) plus this contract-removal and verification task (Task 8).

Preserved before handing off to child plan B:

- migration and sqlc output: `make sqlc` (sqlc v1.31.1) regenerated with no drift; the enrollment/schema migrations from Tasks 1–2 are merged.
- the three-run fake managed-daemon lifecycle result: the lifecycle regression from PR #126 (`server/internal/handler`) passed three consecutive runs against the worktree database.
- the focused package test result: `go test ./internal/auth ./internal/aurora ./internal/daemon ./internal/handler ./cmd/multica ./cmd/migrate -count=1` — all six packages `ok` on the macOS worktree (auth ~1s, aurora ~2s, daemon ~40s, handler ~22s, multica ~1s, migrate ~7s).
- the full `make test` result, including any explicitly identified unrelated known failure: completed with the documented known noise (repocache process-tree teardown message, two clock-skew handler tests, `cmd/migrate` resolving `localhost` to `[::1]` behind a Docker 127.0.0.1 publish) passing when rerun alone; local macOS passing is not Linux CI proof.
- the removal scan showing no active shared-token contract: the Task 8 scan over code, tests, active docs, and environment templates returns no matches — the enrollment constant, route literal, and doc references were removed, and the removal-proof regressions assert 401/404 with static non-contract values.
- confirmation that no `agentintegration` or external-provider test ran: the default suites above ran without the `agentintegration` build tag and without `MULTICA_RUN_REAL_AGENT_SMOKE`; no real agent CLI was invoked.

Scope notes: fleet lifecycle/isolation/egress (Plan B), the skill runtime surface (Plan C), and image supply chain (Plan D) remain pending. Tracker issue #29 stays open in `S2-InProgress`. Earlier tasks' step checkboxes were left unticked by their implementers; their completion is recorded by the merged PRs listed above.
