-- +goose Up
-- +goose StatementBegin

-- join_reason holds the "why do you want to join?" note a user writes when
-- requesting membership in an approval-gated community (the /explore request
-- flow). Empty for instant-join / invited / bootstrap memberships. Shown to
-- admins in the /admin pending queue.
ALTER TABLE memberships ADD COLUMN join_reason TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE memberships DROP COLUMN join_reason;

-- +goose StatementEnd
