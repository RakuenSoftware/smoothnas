-- +goose Up
-- Operator image pin for a plugin service. When set, materialise resolves and runs
-- this image for the primary container ref instead of the manifest's, and -- unlike
-- image_ref (re-derived from the manifest on every materialise/reconcile) -- the pin
-- persists across plugin updates and daemon restarts. Empty/NULL = no pin (manifest
-- image is used). Lets operators run e.g. a vendor's :vulkan image durably without
-- forking the plugin manifest.
ALTER TABLE plugin_services ADD COLUMN pinned_image TEXT;

-- +goose Down
ALTER TABLE plugin_services DROP COLUMN pinned_image;
