-- Aurora create-once provider run (Plan C Task 4). One row per
-- (task_id, operation) records the single create a task is ever allowed to make
-- for that provider operation, plus the external id the provider returned and
-- the terminal outcome.
--
-- The table intentionally declares no primary key and no inline index. The repo
-- convention (see 518-526) is to build the id index CONCURRENTLY in its own
-- single-statement migration (528) and attach it as the primary key (529); the
-- uniqueness and lookup indexes follow in 530-531. There are no foreign keys or
-- cascading actions: the generation, task, and workspace relationships are
-- validated in application code.
CREATE TABLE aurora_provider_run (
    id uuid NOT NULL,
    generation_id uuid NOT NULL,
    task_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    provider text NOT NULL,
    operation text NOT NULL,
    model text NOT NULL,
    request_sha256 text NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    external_id text,
    state text NOT NULL CHECK (state IN ('creating', 'submitted', 'succeeded', 'failed', 'ambiguous')),
    error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);
