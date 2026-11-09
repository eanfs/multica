-- name: CreateFleetNode :one
INSERT INTO fleet_node (owner_id, namespace, name, container_id, container_name, daemon_id, image, spec, target_state, observed_state, idempotency_key)
VALUES (@owner_id, @namespace, @name, @container_id, @container_name, @daemon_id, @image, @spec, @target_state, @observed_state, @idempotency_key)
RETURNING *;

-- name: GetFleetNode :one
SELECT * FROM fleet_node WHERE id = @id AND namespace = @namespace;

-- name: GetFleetNodeByDaemonID :one
SELECT * FROM fleet_node WHERE daemon_id = @daemon_id AND namespace = @namespace;

-- name: GetFleetNodeByContainerID :one
SELECT * FROM fleet_node WHERE container_id = @container_id AND namespace = @namespace;

-- name: GetFleetNodeByIDAndOwner :one
SELECT * FROM fleet_node WHERE id = @id AND namespace = @namespace AND owner_id = @owner_id;

-- name: ListFleetNodesByOwner :many
SELECT * FROM fleet_node
WHERE owner_id = @owner_id AND namespace = @namespace
ORDER BY created_at DESC
LIMIT @limit_count OFFSET @offset_count;

-- name: ListFleetNodesByNamespace :many
SELECT * FROM fleet_node
WHERE namespace = @namespace
ORDER BY created_at ASC, id ASC
LIMIT @limit_count OFFSET @offset_count;

-- name: CountFleetNodesByOwner :one
SELECT count(*) FROM fleet_node
WHERE owner_id = @owner_id AND namespace = @namespace
  AND target_state NOT IN ('terminated', 'failed');

-- name: UpdateFleetNodeContainer :exec
UPDATE fleet_node
SET container_id = @container_id, container_name = @container_name, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: UpdateFleetNodeTargetState :exec
UPDATE fleet_node
SET target_state = @target_state, generation = generation + 1, error = @error, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: UpdateFleetNodeObservedState :exec
UPDATE fleet_node
SET observed_state = @observed_state, daemon_ready = @daemon_ready,
    health_checked_at = now(), status_detail = @status_detail, error = @error, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: CreateFleetNodeCredential :one
INSERT INTO fleet_node_credential (node_id, token_hash)
VALUES (@node_id, @token_hash)
RETURNING *;

-- name: GetFleetNodeCredentialByHash :one
SELECT c.id, c.node_id, c.token_hash, c.revoked_at, c.created_at,
       n.owner_id, n.namespace, n.container_id, n.target_state, n.observed_state
FROM fleet_node_credential c
JOIN fleet_node n ON n.id = c.node_id
WHERE c.token_hash = @token_hash AND c.revoked_at IS NULL;

-- name: RevokeFleetNodeCredentialsByNode :exec
UPDATE fleet_node_credential SET revoked_at = now()
WHERE node_id = @node_id AND revoked_at IS NULL;

-- name: CreateFleetNodeOperation :one
INSERT INTO fleet_node_operation (node_id, operation, phase, idempotency_key, request_fingerprint)
VALUES (@node_id, @operation, @phase, @idempotency_key, @request_fingerprint)
RETURNING *;

-- name: GetFleetNodeOperation :one
SELECT * FROM fleet_node_operation WHERE id = @id;

-- name: GetFleetNodeOperationByIdempotencyKey :one
SELECT * FROM fleet_node_operation
WHERE idempotency_key = @idempotency_key AND idempotency_key <> '';

-- name: MarkFleetNodeOperationPhase :execrows
UPDATE fleet_node_operation
SET phase = @phase, updated_at = now()
WHERE id = @id AND phase = @expected_phase;

-- name: RecordFleetNodeOperationFailure :exec
UPDATE fleet_node_operation
SET phase = @phase, retry_count = @retry_count, error = @error, updated_at = now()
WHERE id = @id;

-- name: ListPendingFleetNodeOperations :many
SELECT * FROM fleet_node_operation
WHERE phase IN ('prepared', 'approved', 'executing')
ORDER BY created_at ASC;

-- name: ListRunnableFleetNodeOperationsByNamespace :many
SELECT o.* FROM fleet_node_operation o
JOIN fleet_node n ON n.id = o.node_id
WHERE n.namespace = @namespace
  AND (o.phase = 'approved'
       OR (o.phase = 'executing' AND o.updated_at < now() - interval '1 minute'))
ORDER BY o.created_at ASC
LIMIT 100;

-- name: CountActiveTasksForDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.workspace_id = @workspace_id
  AND r.daemon_id = @daemon_id
  AND q.status IN ('dispatched', 'running', 'waiting_local_directory');

-- name: CountQueuedTasksForDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.workspace_id = @workspace_id
  AND r.daemon_id = @daemon_id
  AND q.status IN ('queued', 'deferred');

-- name: GetFleetCredentialProfileByOwner :one
SELECT * FROM fleet_credential_profile WHERE owner_id = @owner_id;

-- name: UpsertFleetCredentialProfile :one
INSERT INTO fleet_credential_profile (owner_id, profile_name, config_version)
VALUES (@owner_id, @profile_name, @config_version)
ON CONFLICT (owner_id) DO UPDATE
SET profile_name = EXCLUDED.profile_name, config_version = EXCLUDED.config_version, updated_at = now()
RETURNING *;

-- name: ListStaleFleetNodes :many
SELECT * FROM fleet_node
WHERE namespace = @namespace AND target_state IN ('running', 'starting', 'stopping', 'rebooting')
  AND (health_checked_at IS NULL OR health_checked_at < now() - interval '30 seconds');
