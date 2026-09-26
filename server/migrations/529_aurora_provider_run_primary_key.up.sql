-- Attach the CONCURRENTLY-built unique index as the table's primary key.
ALTER TABLE aurora_provider_run
    ADD CONSTRAINT aurora_provider_run_pkey PRIMARY KEY USING INDEX aurora_provider_run_pkey;
