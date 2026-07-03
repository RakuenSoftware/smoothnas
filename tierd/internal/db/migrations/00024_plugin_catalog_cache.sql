-- +goose Up
-- Cached latest-release responses for first-party (bundled) plugin catalog
-- repos. The catalog serves the appliance's embedded snapshot as the offline
-- floor and, when online, refreshes from GitHub in the background; the newest
-- successful fetch per repo is cached here so a newer plugin version survives a
-- restart and is served during a later GitHub outage (plugins-12). Not a
-- security boundary — it only holds public release manifests.
CREATE TABLE plugin_catalog_cache (
    repo       TEXT PRIMARY KEY, -- lowercased "owner/name"
    tag_name   TEXT NOT NULL,
    response   TEXT NOT NULL,    -- JSON pluginCatalogLatestResponse
    fetched_at INTEGER NOT NULL  -- unix seconds
);

-- +goose Down
DROP TABLE plugin_catalog_cache;
