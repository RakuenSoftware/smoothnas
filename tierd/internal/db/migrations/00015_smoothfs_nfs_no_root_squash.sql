-- +goose Up
-- SmoothFS-backed NFS exports are used by root-run backup rsync pulls. With
-- root_squash enabled, nfsd maps client root to nobody and rsync cannot read
-- non-world-readable source files, aborting the backup with code 23.
UPDATE nfs_exports
   SET root_squash = 0
 WHERE root_squash = 1
   AND EXISTS (
       SELECT 1
         FROM smoothfs_pools
        WHERE nfs_exports.path = smoothfs_pools.mountpoint
           OR nfs_exports.path LIKE smoothfs_pools.mountpoint || '/%'
   );

-- +goose Down
SELECT 1;
