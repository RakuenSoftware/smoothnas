-- +goose Up
-- Phase 7.7 — smoothfs_pools persists the operator-declared
-- ManagedPool shape (name, uuid, tier list, mountpoint, unit path)
-- that tierd wrote to /etc/systemd/system/<unit>.mount.
--
-- tiers is stored as a ':'-joined path list, matching smoothfs's
-- on-the-wire tiers= mount option — this is the same format the
-- systemd unit carries and it avoids a second table for what's
-- always small (≤ SMOOTHFS_MAX_TIERS ≈ 16 paths).
CREATE TABLE smoothfs_pools (
    uuid         TEXT    PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    tiers        TEXT    NOT NULL,
    mountpoint   TEXT    NOT NULL,
    unit_path    TEXT    NOT NULL,
    created_at   TEXT    NOT NULL
);
-- +goose Down
DROP TABLE smoothfs_pools;
