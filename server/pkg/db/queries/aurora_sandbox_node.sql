-- Aurora managed sandbox node lifecycle (Plan A Task 1). One node row per
-- workspace, created unbound and later claimed by a single managed daemon.
-- Uniqueness is enforced by migrations 519-526, not by an ON CONFLICT arbiter:
-- every insert here carries a fresh node id, workspace id, runtime id, and
-- daemon id, and the unique indexes are the write-time arbiters.

-- name: CreateAuroraSandboxNode :one
-- Inserts one sandbox node with every identity and lifecycle field written
-- explicitly. last_active_at/created_at/updated_at fall back to their column
-- defaults. The workspace/runtime/daemon unique indexes (521-523) reject a
-- second live node for any of those identities.
INSERT INTO aurora_sandbox_node (
    id, workspace_id, runtime_id, daemon_id, backend_node_id, image_digest, state,
    enrollment_token_hash, enrollment_expires_at, enrollment_consumed_at,
    drain_started_at, started_at, stopped_at, failure_reason
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10,
    $11, $12, $13, $14
)
RETURNING *;

-- name: GetAuroraSandboxNodeByWorkspace :one
-- The workspace's node row, whatever its lifecycle state.
SELECT * FROM aurora_sandbox_node
WHERE workspace_id = $1;

-- name: LockAuroraSandboxNodeByWorkspace :one
-- Serialises enrollment rotation and node lifecycle transitions for one
-- workspace until the surrounding transaction commits.
SELECT * FROM aurora_sandbox_node
WHERE workspace_id = $1
FOR UPDATE;

-- name: LockAuroraSandboxEnrollmentWorkspace :exec
-- Transaction-scoped advisory lock on a workspace, taken before issuance reads
-- or writes the node row. It is what makes first-issue safe when no row exists
-- yet to FOR UPDATE, so two concurrent issues cannot both mint a node.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('workspace_id')::uuid::text, 0));

-- name: ConsumeAuroraSandboxEnrollment :one
-- Atomically spends a single-use enrollment secret. The predicate is the
-- exactly-once guard: only an unconsumed, unexpired secret on a starting node
-- matches, and the row is flipped to online with the secret cleared before it
-- is returned. A replay or expired secret matches nothing.
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
-- Re-arms a stopped, failed, or stale-starting node with a fresh single-use
-- secret. Consumed and failure markers are cleared so the new secret is live
-- and the row returns to 'starting'; node/runtime/daemon identity is preserved.
UPDATE aurora_sandbox_node
SET enrollment_token_hash = $2,
    enrollment_expires_at = $3,
    enrollment_consumed_at = NULL,
    state = 'starting',
    started_at = NULL,
    stopped_at = NULL,
    drain_started_at = NULL,
    backend_node_id = NULL,
    failure_reason = NULL,
    updated_at = now()
WHERE workspace_id = $1
RETURNING *;

-- name: BindAuroraManagedRuntime :one
-- Binds the workspace's managed runtime to the daemon identity that consumed
-- its enrollment. The predicate repeats every carrier invariant so a stale
-- runtime id, a foreign workspace id, or a non-managed carrier cannot be
-- bound; one such runtime per workspace is migration 526.
UPDATE agent_runtime
SET daemon_id = $1,
    status = 'online',
    last_seen_at = now(),
    updated_at = now()
WHERE id = $2
  AND workspace_id = $3
  AND runtime_mode = 'cloud'
  AND provider = 'aurora_managed'
RETURNING *;

-- name: TouchAuroraSandboxNode :exec
-- Heartbeat activity for the bound daemon only: a mismatched daemon, or a node
-- that is not online or draining, is left untouched.
UPDATE aurora_sandbox_node
SET last_active_at = now()
WHERE workspace_id = $1
  AND daemon_id = $2
  AND state IN ('online', 'draining');

-- name: MarkAuroraSandboxNodeDraining :one
-- Begins drain for an online node. The first drain_started_at wins so a
-- repeated signal does not extend the drain window.
UPDATE aurora_sandbox_node
SET state = 'draining',
    drain_started_at = COALESCE(drain_started_at, now()),
    updated_at = now()
WHERE workspace_id = $1
  AND daemon_id = $2
  AND state = 'online'
RETURNING *;

-- name: MarkAuroraSandboxNodeStopped :one
-- Terminal stop for the bound daemon: records the stop time and clears every
-- live enrollment/backend field so the row cannot be reused without a fresh
-- enrollment. started_at is retained as an audit timestamp.
UPDATE aurora_sandbox_node
SET state = 'stopped',
    stopped_at = now(),
    last_active_at = now(),
    enrollment_token_hash = NULL,
    enrollment_expires_at = NULL,
    enrollment_consumed_at = NULL,
    backend_node_id = NULL,
    updated_at = now()
WHERE workspace_id = $1
  AND daemon_id = $2
RETURNING *;

-- name: ListAuroraSandboxNodesForReap :many
-- Bounded, oldest-first candidate scan for the node reaper: an active node is
-- a candidate once it is idle past the idle cutoff or past the hard lifetime
-- measured from created_at.
SELECT * FROM aurora_sandbox_node
WHERE state IN ('starting', 'online', 'draining')
  AND (last_active_at < $1 OR created_at < $2)
ORDER BY created_at ASC, id ASC
LIMIT $3;
