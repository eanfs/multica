-- name: FleetSchemaVersion :one
SELECT current_setting('server_version_num')::integer >= 170000 AS supported;

-- name: FleetSchemaProbe :exec
-- Parse and permission-check required fields and query dependencies without reading rows.
SELECT n.id,
 n.namespace,
 n.owner_id,
 n.created_at,
 n.updated_at,
 n.container_id,
 n.daemon_id,
 n.name,
 n.spec,
 n.image,
 n.profile_ref,
 n.start_epoch,
 n.data_volume,
 n.secrets_volume,
 n.desired,
 n.status,
 n.generation,
 n.ready,
 n.health_at,
 n.active_runs,
 n.pending_reports,
 n.failed_reports,
 n.maintenance,
 n.revoked,
 n.error_code,
 n.error_message,
 n.spec_config,
 o.id,
 o.namespace,
 o.owner_id,
 o.created_at,
 o.updated_at,
 o.node_id,
 o.action,
 o.idempotency_key,
 o.request_hash,
 o.phase,
 o.prior_desired,
 o.generation,
 o.approved,
 o.attempts,
 o.error_code,
 o.error_message,
 c.id,
 c.namespace,
 c.owner_id,
 c.created_at,
 c.updated_at,
 c.node_id,
 c.token_hash,
 c.revoked_at,
 c.generation,
 p.id,
 p.namespace,
 p.owner_id,
 p.created_at,
 p.updated_at,
 p.profile_ref,
 p.config_version,
 u.id,
 r.id,
 r.owner_id,
 r.metadata,
 t.runtime_id,
 t.status
FROM fleet_nodes n CROSS JOIN fleet_node_operations o
 CROSS JOIN fleet_node_credentials c CROSS JOIN fleet_credential_profiles p
 CROSS JOIN "user" u CROSS JOIN agent_runtime r CROSS JOIN agent_task_queue t
WHERE false;

-- name: FleetNodeSharedLock :exec
SELECT pg_advisory_xact_lock_shared(hashtextextended(sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));

-- name: FleetNodeExclusiveLock :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));

-- name: FleetNodeCapacityLock :exec
SELECT pg_advisory_xact_lock(hashtextextended('capacity:' || sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));

-- name: GetFleetNode :one
SELECT * FROM fleet_nodes WHERE namespace = @namespace AND owner_id = @owner_id AND id = @node_id;

-- name: ListFleetNodesByOwner :many
SELECT * FROM fleet_nodes WHERE namespace = @namespace AND owner_id = @owner_id
ORDER BY created_at, id LIMIT @page_limit OFFSET @page_offset;

-- name: ResolveFleetNodeReference :one
SELECT id FROM fleet_nodes WHERE namespace = @namespace AND owner_id = @owner_id
 AND (id::text = @reference::text OR (container_id <> '' AND container_id = @reference::text))
ORDER BY (id::text = @reference::text) DESC LIMIT 1;

-- name: GetFleetOperation :one
SELECT * FROM fleet_node_operations WHERE namespace = @namespace AND owner_id = @owner_id AND id = @operation_id;

-- name: GetFleetNodeForRuntime :one
SELECT n.* FROM fleet_nodes n JOIN agent_runtime r ON r.metadata->>'fleet_node_id' = n.id::text
WHERE n.namespace = @namespace AND n.owner_id = @owner_id AND r.owner_id = n.owner_id
 AND r.id = @runtime_id AND r.metadata->>'managed_by' = 'local_fleet';

-- name: CountFleetActiveRuns :one
-- Safety counts deliberately span all associated workspaces, not a request header.
SELECT count(*) FROM agent_task_queue t
JOIN agent_runtime r ON r.id = t.runtime_id
JOIN fleet_nodes n ON r.metadata->>'fleet_node_id' = n.id::text
WHERE n.namespace = @namespace AND n.owner_id = @owner_id AND n.id = @node_id
 AND r.owner_id = n.owner_id AND r.metadata->>'managed_by' = 'local_fleet'
 AND t.status IN ('dispatched', 'running', 'waiting_local_directory');

-- name: CountFleetQueuedRuns :one
SELECT count(*) FROM agent_task_queue t
JOIN agent_runtime r ON r.id = t.runtime_id
JOIN fleet_nodes n ON r.metadata->>'fleet_node_id' = n.id::text
WHERE n.namespace = @namespace AND n.owner_id = @owner_id AND n.id = @node_id
 AND r.owner_id = n.owner_id AND r.metadata->>'managed_by' = 'local_fleet'
 AND t.status IN ('queued', 'deferred');

-- name: FleetOwnerExclusiveLock :exec
-- Creation and profile projection share this protocol; batch owners sort by UUID.
-- Acquire owner before any node/capacity/runtime locks if combining protocols.
SELECT pg_advisory_xact_lock(hashtextextended('fleet-owner:' || sqlc.arg(namespace)::text || ':' || sqlc.arg(owner_id)::uuid::text, 0));

-- name: FleetOwnerExists :one
SELECT EXISTS (SELECT 1 FROM "user" WHERE id = @owner_id);

-- name: GetFleetProfile :one
SELECT * FROM fleet_credential_profiles WHERE namespace = @namespace AND owner_id = @owner_id;

-- name: UpsertFleetProfile :exec
INSERT INTO fleet_credential_profiles (namespace, owner_id, profile_ref, config_version)
VALUES (@namespace, @owner_id, @profile_ref, @config_version)
ON CONFLICT (namespace, owner_id) DO UPDATE SET profile_ref = EXCLUDED.profile_ref,
 config_version = EXCLUDED.config_version, updated_at = now();

-- name: GetFleetIntentByKey :one
SELECT * FROM fleet_node_operations WHERE namespace = @namespace AND owner_id = @owner_id AND idempotency_key = @idempotency_key;

-- name: CountFleetProvisionedNodes :one
SELECT count(*) FROM fleet_nodes WHERE namespace = @namespace AND owner_id = @owner_id
 AND NOT (desired = 'terminated' AND status = 'terminated');

-- name: InsertFleetNode :one
INSERT INTO fleet_nodes (namespace, owner_id, name, spec, image, profile_ref, spec_config)
VALUES (@namespace, @owner_id, @name, @spec, @image, @profile_ref, @spec_config) RETURNING *;

-- name: InsertFleetCreateOperation :one
INSERT INTO fleet_node_operations (namespace, owner_id, node_id, action, idempotency_key, request_hash)
VALUES (@namespace, @owner_id, @node_id, 'create', @idempotency_key, @request_hash) RETURNING *;

-- name: GetFleetNodeIdentity :one
-- The globally unique node ID locates its trusted namespace, never caller metadata.
SELECT * FROM fleet_nodes WHERE id = @node_id AND owner_id = @owner_id;

-- name: GetFleetNodeByID :one
SELECT * FROM fleet_nodes WHERE namespace = @namespace AND id = @node_id;

-- name: MaxFleetCredentialGeneration :one
SELECT COALESCE(max(generation), 0)::bigint FROM fleet_node_credentials
WHERE namespace = @namespace AND node_id = @node_id AND owner_id = @owner_id;

-- name: RevokeFleetCredentials :exec
UPDATE fleet_node_credentials SET revoked_at = now(), updated_at = now()
WHERE namespace = @namespace AND node_id = @node_id AND owner_id = @owner_id AND revoked_at IS NULL;

-- name: InsertFleetCredential :exec
INSERT INTO fleet_node_credentials (namespace, node_id, owner_id, token_hash, generation)
VALUES (@namespace, @node_id, @owner_id, @token_hash, @generation);

-- name: VerifyFleetCredential :one
SELECT n.* FROM fleet_node_credentials c
JOIN fleet_nodes n ON n.id = c.node_id AND n.owner_id = c.owner_id AND n.namespace = c.namespace
JOIN "user" u ON u.id = n.owner_id
WHERE c.namespace = @namespace AND c.token_hash = @token_hash AND c.revoked_at IS NULL
 AND NOT n.revoked AND n.desired <> 'terminated' AND n.status <> 'terminated'
 AND c.generation = (SELECT max(h.generation) FROM fleet_node_credentials h
   WHERE h.namespace = n.namespace AND h.node_id = n.id AND h.owner_id = n.owner_id);
