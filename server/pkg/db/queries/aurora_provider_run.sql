-- Aurora create-once provider runs (Plan C Task 4). One row per
-- (task_id, operation), inserted in the `creating` state by the begin endpoint
-- and advanced by the external-id and finish transitions. Uniqueness is
-- enforced by migrations 528-531, not by an ON CONFLICT arbiter: the begin
-- insert races the (task_id, operation) unique index and the loser reloads the
-- winning row rather than letting Postgres pick a side.

-- name: CreateAuroraProviderRun :one
-- Opens the create lease for one (task_id, operation). state is always
-- 'creating'; external_id, error_code and completed_at stay NULL until the
-- later transitions fill them in. A duplicate begin loses to migration 530's
-- unique index on (task_id, operation).
INSERT INTO aurora_provider_run (
    id, generation_id, task_id, workspace_id, provider, operation, model,
    request_sha256, state
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, 'creating'
)
RETURNING *;

-- name: GetAuroraProviderRun :one
-- The task's run for one operation, whatever state it is in. The begin retry,
-- the external-id replay, and the finish replay all converge here after a
-- write predicate matched nothing.
SELECT * FROM aurora_provider_run
WHERE task_id = $1 AND operation = $2;

-- name: ClaimAuroraProviderRunExternal :one
-- Records the provider's external id exactly once. The predicate is the
-- guard: only a run that is still creating and has no external id matches, so
-- a replay of the same id is a no-op for the caller (the row is reloaded) and a
-- different id can never overwrite the recorded one.
UPDATE aurora_provider_run
SET external_id = $3, state = 'submitted', updated_at = now()
WHERE task_id = $1
  AND operation = $2
  AND state = 'creating'
  AND external_id IS NULL
RETURNING *;

-- name: FinishAuroraProviderRun :one
-- Terminal transition for a run that actually reached the provider. Only a
-- creating or submitted run may finish, so an ambiguous run stays frozen and a
-- replay cannot overwrite a recorded outcome with its opposite.
UPDATE aurora_provider_run
SET state = $3, error_code = $4, completed_at = now(), updated_at = now()
WHERE task_id = $1
  AND operation = $2
  AND state IN ('creating', 'submitted')
RETURNING *;

-- name: MarkAuroraProviderRunAmbiguous :one
-- A retry of a live create lease with no recorded external id proves the first
-- create may already have reached the provider. Freeze the run as ambiguous so
-- it is failed/refunded instead of being submitted a second time.
UPDATE aurora_provider_run
SET state = 'ambiguous', updated_at = now()
WHERE task_id = $1
  AND operation = $2
  AND state = 'creating'
  AND external_id IS NULL
RETURNING *;
