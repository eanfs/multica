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

-- name: CountWorkspacesForUser :one
SELECT count(*) FROM member WHERE user_id = $1;
