-- +goose Up
-- Sessions whose owner must change their password before the session may be
-- used for anything other than the password-change endpoint. Required by
-- smoothgui/auth v0.2.4+: its RequireAuth middleware queries this table on
-- every authenticated request (SessionMustChange), so without it every such
-- request fails with 500 auth.error. A token's presence means "must change";
-- the row is removed once the password is changed or the session ends.
-- Mirrors the CREATE in smoothgui/auth's own Migrations list.
CREATE TABLE session_must_change (
    token TEXT PRIMARY KEY
);

-- +goose Down
DROP TABLE session_must_change;
