-- name: CreateAuroraModerationLog :one
INSERT INTO aurora_moderation_log (generation_id, workspace_id, scope, verdict, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, generation_id, workspace_id, scope, verdict, reason, created_at;
