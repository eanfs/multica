-- Immutable same-operation dispatch checkpoint; no restart grants another Apply.
ALTER TABLE fleet_node_operations
 ADD COLUMN IF NOT EXISTS action_claimed_at timestamptz,
 ADD COLUMN IF NOT EXISTS action_start_epoch text NOT NULL DEFAULT '';
