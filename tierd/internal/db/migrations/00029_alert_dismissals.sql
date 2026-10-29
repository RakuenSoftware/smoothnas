-- +goose Up
CREATE TABLE alert_dismissals (
    source       TEXT NOT NULL,
    device       TEXT NOT NULL,
    message      TEXT NOT NULL,
    dismissed_at TEXT NOT NULL,
    PRIMARY KEY (source, device, message)
);
-- +goose Down
DROP TABLE alert_dismissals;
