-- Attach the CONCURRENTLY-built unique index as the table's primary key.
ALTER TABLE aurora_sandbox_node
    ADD CONSTRAINT aurora_sandbox_node_pkey PRIMARY KEY USING INDEX aurora_sandbox_node_pkey;
