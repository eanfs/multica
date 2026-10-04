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
