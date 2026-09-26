-- Dropping the primary-key constraint in 529's down migration drops the
-- attached index too, so there is nothing left to roll back here.
SELECT 1;
