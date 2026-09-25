-- Backing index for aurora_sandbox_node's primary key, attached in 520 via
-- PRIMARY KEY USING INDEX. Own single-statement migration so CONCURRENTLY runs
-- outside an implicit transaction (repo convention). Registered in
-- cmd/migrate/main.go's concurrentIndexCleanups so an interrupted build is not
-- mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_sandbox_node_pkey ON aurora_sandbox_node (id);
