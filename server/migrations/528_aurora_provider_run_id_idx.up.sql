-- Backing index for aurora_provider_run's primary key, attached in 529 via
-- PRIMARY KEY USING INDEX. Own single-statement migration so CONCURRENTLY runs
-- outside an implicit transaction (repo convention). Registered in
-- cmd/migrate/main.go's concurrentIndexCleanups so an interrupted build is not
-- mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_provider_run_pkey ON aurora_provider_run (id);
