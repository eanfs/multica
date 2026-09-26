-- Attach the CONCURRENTLY-built unique index as the table's primary key.
ALTER TABLE aurora_artifact_staging
    ADD CONSTRAINT aurora_artifact_staging_pkey PRIMARY KEY USING INDEX aurora_artifact_staging_pkey;
