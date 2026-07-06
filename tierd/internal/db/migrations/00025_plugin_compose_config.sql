-- +goose Up
-- Non-secret operator config for a compose plugin (compose-migration S2). Values
-- for keys declared in a top-level x-smoothnas.config: list (secret:false) are
-- stored here and rendered into the compose .env at Materialise (with the
-- schema's defaults for keys the operator left unset), so docker compose resolves
-- ${KEY} natively. Secret keys go to plugin_compose_secrets instead — a key is
-- ever in exactly one of the two tables, by its secret flag.
CREATE TABLE plugin_compose_config (
    plugin_name TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    PRIMARY KEY (plugin_name, key),
    FOREIGN KEY (plugin_name) REFERENCES plugins(name) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE plugin_compose_config;
