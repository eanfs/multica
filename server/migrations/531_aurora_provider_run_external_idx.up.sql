-- The provider plus its external id identifies one remote run; unique so two
-- local runs can never claim the same provider result. Partial because a run
-- has no external id until the create call returns. In its own file because
-- CREATE INDEX CONCURRENTLY cannot share a statement or run inside a
-- transaction, and registered in cmd/migrate/main.go's concurrentIndexCleanups
-- so an interrupted build is not mistaken for success on retry.
CREATE UNIQUE INDEX CONCURRENTLY aurora_provider_run_provider_external_uidx
    ON aurora_provider_run (provider, external_id) WHERE external_id IS NOT NULL;
