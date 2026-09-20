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
