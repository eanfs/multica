CREATE TABLE fleet_namespace_fences (
 namespace text NOT NULL,
 fleet_id text NOT NULL,
 closed boolean NOT NULL,
 generation bigint NOT NULL CHECK (generation > 0),
 operation_key text NOT NULL
);
