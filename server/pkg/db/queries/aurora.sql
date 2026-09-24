-- name: CreateAuroraGeneration :one
INSERT INTO aurora_generation (workspace_id, user_id, skill_id, prompt, status)
VALUES ($1, $2, $3, $4, 'queued')
RETURNING id, workspace_id, user_id, skill_id, prompt, status, task_id,
          credits_reserved, credits_charged, error, created_at, updated_at;

-- name: GetAuroraGeneration :one
SELECT id, workspace_id, user_id, skill_id, prompt, status, task_id,
       credits_reserved, credits_charged, error, created_at, updated_at
FROM aurora_generation
WHERE id = $1 AND workspace_id = $2;

-- name: UpdateAuroraGenerationTask :one
UPDATE aurora_generation
SET task_id = $2, credits_reserved = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $4
RETURNING id, workspace_id, user_id, skill_id, prompt, status, task_id,
          credits_reserved, credits_charged, error, created_at, updated_at;

-- name: UpdateAuroraGenerationTerminal :one
UPDATE aurora_generation
SET status = $2, error = $3, credits_charged = $4, updated_at = now()
WHERE id = $1 AND workspace_id = $5
RETURNING id, workspace_id, user_id, skill_id, prompt, status, task_id,
          credits_reserved, credits_charged, error, created_at, updated_at;

-- name: GetAuroraGenerationByTaskID :one
SELECT id, workspace_id, user_id, skill_id, prompt, status, task_id,
       credits_reserved, credits_charged, error, created_at, updated_at
FROM aurora_generation
WHERE task_id = $1;

-- name: ListAuroraGenerations :many
SELECT id, workspace_id, user_id, skill_id, prompt, status, task_id,
       credits_reserved, credits_charged, error, created_at, updated_at
FROM aurora_generation
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateAuroraAsset :one
INSERT INTO aurora_asset (generation_id, workspace_id, kind, media_url, format)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, generation_id, workspace_id, kind, media_url, format, created_at;

-- name: ListAuroraAssets :many
SELECT id, generation_id, workspace_id, kind, media_url, format, created_at
FROM aurora_asset
WHERE (sqlc.narg('generation_id')::uuid IS NULL OR generation_id = sqlc.narg('generation_id')::uuid)
  AND (sqlc.narg('workspace_id')::uuid IS NULL OR workspace_id = sqlc.narg('workspace_id')::uuid)
ORDER BY created_at DESC
LIMIT sqlc.arg('limit')::int OFFSET sqlc.arg('offset')::int;

-- name: GetAuroraAsset :one
SELECT id, generation_id, workspace_id, kind, media_url, format, created_at
FROM aurora_asset
WHERE id = $1 AND workspace_id = $2;

-- name: DeleteAuroraAsset :execrows
DELETE FROM aurora_asset
WHERE id = $1 AND workspace_id = $2;

-- name: CountWorkspacesForUser :one
SELECT count(*) FROM member WHERE user_id = $1;

-- name: CountGenerationsThisMonth :one
-- Monthly generation quota (Task 6). The window is the calendar month, matching
-- the credit grant window so a user cannot spend one tier's quota against
-- another tier's month.
SELECT count(*) FROM aurora_generation
WHERE user_id = $1 AND created_at >= date_trunc('month', now());

-- name: CountActiveGenerations :one
-- Concurrency gate (Task 6): generations whose task has not reached a terminal
-- state. deferred counts as active — a retry armed with a backoff is still work
-- the user has in flight.
SELECT count(*) FROM aurora_generation g
JOIN agent_task_queue t ON t.id = g.task_id
WHERE g.user_id = $1
  AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred');

-- name: GetPersonalWorkspaceForUser :one
-- Aurora credits are granted to a personal workspace, but the ledger is keyed by
-- user. The owner membership is the anchor: a user can belong to many
-- workspaces, and the earliest owned one is the personal one created at signup.
SELECT w.id FROM workspace w
JOIN member m ON m.workspace_id = w.id
WHERE m.user_id = $1 AND m.role = 'owner'
ORDER BY w.created_at ASC
LIMIT 1;
