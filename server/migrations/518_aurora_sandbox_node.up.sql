-- Aurora managed sandbox node: exactly one node row per workspace.
--
-- The table intentionally declares no primary key and no inline index. The repo
-- convention (see 207-209 / 327-329) is to build the id index CONCURRENTLY in
-- its own single-statement migration (519) and attach it as the primary key
-- (520); the remaining uniqueness and scan indexes follow in 521-526. There are
-- no foreign keys or cascading actions: the workspace, runtime, and daemon
-- relationships are validated and cleaned up in application code.
CREATE TABLE aurora_sandbox_node (
    id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    runtime_id uuid NOT NULL,
    daemon_id text NOT NULL,
    backend_node_id text,
    image_digest text NOT NULL,
    state text NOT NULL CHECK (state IN ('starting', 'online', 'draining', 'stopped', 'failed')),
    enrollment_token_hash text,
    enrollment_expires_at timestamptz,
    enrollment_consumed_at timestamptz,
    last_active_at timestamptz NOT NULL DEFAULT now(),
    drain_started_at timestamptz,
    started_at timestamptz,
    stopped_at timestamptz,
    failure_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((enrollment_token_hash IS NULL) = (enrollment_expires_at IS NULL))
);
