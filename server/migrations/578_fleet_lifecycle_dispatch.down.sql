ALTER TABLE fleet_node_operations
 DROP COLUMN IF EXISTS action_start_epoch,
 DROP COLUMN IF EXISTS action_claimed_at;
