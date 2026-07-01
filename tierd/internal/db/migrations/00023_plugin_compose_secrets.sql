-- +goose Up
-- Secret env values for a compose plugin (plugins-11 gh-runner). Values named by
-- a top-level x-smoothnas.secrets: [KEY] list are stored here, NEVER in the
-- compose file / a compose-loaded .env: at `compose up` tierd injects them into
-- the subprocess environment so compose resolves ${KEY} from the service's
-- environment: block. (A sealed/age-wrapped file store is a hardening follow-up;
-- this matches the existing plugin_secrets DB posture.)
CREATE TABLE plugin_compose_secrets (
    plugin_name TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    PRIMARY KEY (plugin_name, key),
    FOREIGN KEY (plugin_name) REFERENCES plugins(name) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE plugin_compose_secrets;
