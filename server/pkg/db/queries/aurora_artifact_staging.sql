-- Aurora artifact staging (Plan C Task 6). Rows are created by the task-token
-- upload and provider-import endpoints before Task 7 commits them as workspace
-- assets. Uniqueness is enforced by migrations 533/535, not by an ON CONFLICT
-- arbiter: the upload path races the (task_id, manifest_artifact_id) unique
-- index and reports the loss as a duplicate artifact.

-- name: CreateAuroraArtifactStaging :one
-- Inserts one staged object. status is always 'staged'; the commit/deletion
-- transitions arrive with the reporting task. manifest_artifact_id, role and
-- format are NULL for provider imports, whose manifest identity is assigned by
-- the broker after the import returns.
INSERT INTO aurora_artifact_staging (
    id, task_id, generation_id, workspace_id, manifest_artifact_id,
    storage_key, name, kind, role, format, mime_type, size_bytes, sha256,
    metadata, source_type, status
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'staged'
)
RETURNING *;

-- name: GetAuroraArtifactStaging :one
SELECT * FROM aurora_artifact_staging WHERE id = $1;
