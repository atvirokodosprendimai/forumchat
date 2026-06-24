-- +goose Up
-- +goose StatementBegin

-- admin_display_name is a per-community moderator override for how a member
-- is shown to everyone else (chat, forum, roster, @mentions, …). Empty means
-- "no override" — fall back to the member's own display_name. Lets an admin
-- replace a member's self-chosen (possibly offensive) name with a clean one
-- without touching what the member set for themselves.
ALTER TABLE memberships ADD COLUMN admin_display_name TEXT NOT NULL DEFAULT '';

-- effective_display_name resolves the override rule ONCE, in the schema:
-- the admin override when set, otherwise the member's own name. Every read
-- path that renders a member's name to OTHERS selects this column, so the
-- resolution rule never gets re-implemented per query.
ALTER TABLE memberships ADD COLUMN effective_display_name TEXT
    GENERATED ALWAYS AS (
        CASE WHEN admin_display_name <> '' THEN admin_display_name ELSE display_name END
    ) VIRTUAL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Drop the generated column before the column it depends on.
ALTER TABLE memberships DROP COLUMN effective_display_name;
ALTER TABLE memberships DROP COLUMN admin_display_name;

-- +goose StatementEnd
