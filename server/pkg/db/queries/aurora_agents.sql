-- Aurora system-agent seeding (Plan 3 Task 1). These queries back
-- aurora.EnsureSystemAgents, which lazily materialises a workspace's 16 skill
-- system agents — one managed runtime row, and per catalog skill one
-- kind='system' agent, one skill row, and the agent_skill junction — on first
-- generation creation.

-- name: GetAuroraManagedRuntime :one
-- The workspace's server-hosted (managed) runtime, the idempotency anchor for
-- seeding. daemon_id is NULL because no daemon has claimed it yet; a sandbox
-- daemon claims it later (Plan 3 Task 3). There is no unique constraint that
-- covers a NULL daemon_id — migration 121's partial index keys
-- (workspace_id, daemon_id, provider) and NULLs are distinct — so the seed
-- looks it up rather than relying on an ON CONFLICT arbiter.
SELECT * FROM agent_runtime
WHERE workspace_id = $1
  AND daemon_id IS NULL
  AND runtime_mode = 'cloud'
  AND provider = $2
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: CreateAuroraManagedRuntime :one
INSERT INTO agent_runtime (
    workspace_id, daemon_id, name, runtime_mode, provider, status,
    device_info, metadata, owner_id, visibility
) VALUES (
    $1, NULL, $2, 'cloud', $3, 'offline', '', '{}'::jsonb, $4, 'private'
)
RETURNING *;

-- name: UpsertAuroraSystemAgent :one
-- Inserts or refreshes one Aurora system agent. kind='system' marks it an
-- invisible execution carrier (hidden from agent lists and hard-deleted with
-- its runtime), exactly like the Agent Builder's carriers. Idempotency rides
-- migration 172's partial unique index on
-- (workspace_id, owner_id, runtime_id, system_key) WHERE system_key IS NOT
-- NULL; the arbiter must name all four columns and repeat the predicate, or
-- Postgres will not match the partial index. DO UPDATE refreshes
-- name/instructions so a later seed enriches the prompt without creating a
-- second row.
INSERT INTO agent (
    workspace_id, owner_id, runtime_id, kind, system_key, name, instructions,
    runtime_mode, visibility, permission_mode, runtime_config
) VALUES (
    $1, $2, $3, 'system', $4, $5, $6, 'cloud', 'workspace', 'private', '{}'::jsonb
)
ON CONFLICT (workspace_id, owner_id, runtime_id, system_key) WHERE system_key IS NOT NULL
DO UPDATE SET name = EXCLUDED.name, instructions = EXCLUDED.instructions
RETURNING *;

-- name: UpsertAuroraSkill :one
-- Inserts or refreshes the skill row a system agent's skill resolves to. The
-- name equals the agent's display name, and skill's UNIQUE(workspace_id, name)
-- is the idempotency key. content is the minimal SKILL.md workflow text,
-- refreshed on every seed so Plan 3.5's enrichment reaches already-seeded
-- workspaces.
INSERT INTO skill (workspace_id, name, description, content, config, created_by)
VALUES ($1, $2, '', $3, '{}'::jsonb, $4)
ON CONFLICT (workspace_id, name)
DO UPDATE SET content = EXCLUDED.content, updated_at = now()
RETURNING *;
