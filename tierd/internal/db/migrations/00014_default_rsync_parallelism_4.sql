-- +goose Up
-- Tierd 0.0.47 (PR #64) made backup_configs.parallelism actually take
-- effect for the rsync method; before that, the field was a no-op and
-- every rsync backup ran single-stream regardless of the column value.
-- Live measurement on a 2.5 GbE NAS pulling small-file directories
-- showed single-stream capping at ~95 MB/s while 4 parallel streams hit
-- the NIC ceiling at ~305 MB/s — a ~3× speedup with no other change.
--
-- Bump the default from 1 to 4 so existing rsync configs that took the
-- old (meaningless) default get the benefit of real parallelism after
-- update. Configs that explicitly used 2/3/5/etc. are left alone.
-- cp-method configs are also left alone — for cp, parallelism=1 was
-- never a no-op so the value may reflect a deliberate operator choice.
UPDATE backup_configs
   SET parallelism = 4
 WHERE method      = 'rsync'
   AND parallelism = 1;

-- +goose Down
SELECT 1;
