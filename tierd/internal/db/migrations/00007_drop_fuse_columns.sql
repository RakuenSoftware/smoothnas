-- +goose Up
-- Drop legacy FUSE-daemon columns: smoothfs kernel module replaced the
-- user-space FUSE daemon, so per-namespace socket_path / daemon_pid /
-- daemon_state / fuse_mode columns are dead weight on every CRUD. SQLite
-- 3.35+ supports ALTER TABLE DROP COLUMN.

ALTER TABLE mdadm_managed_namespaces DROP COLUMN socket_path;
ALTER TABLE mdadm_managed_namespaces DROP COLUMN daemon_pid;
ALTER TABLE mdadm_managed_namespaces DROP COLUMN daemon_state;

ALTER TABLE zfs_managed_targets      DROP COLUMN fuse_mode;

ALTER TABLE zfs_managed_namespaces   DROP COLUMN socket_path;
ALTER TABLE zfs_managed_namespaces   DROP COLUMN daemon_pid;
ALTER TABLE zfs_managed_namespaces   DROP COLUMN daemon_state;
ALTER TABLE zfs_managed_namespaces   DROP COLUMN fuse_mode;

-- +goose Down
-- No-op: the FUSE-daemon columns are gone for good. Rolling back would
-- require re-seeding all four with sentinel values, and nothing reads
-- them any more.
