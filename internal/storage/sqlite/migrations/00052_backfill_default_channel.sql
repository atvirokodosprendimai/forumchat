-- +goose Up
-- Backfill the undeletable #general channel for every community that lacks
-- one. Migration 00032 seeded #general only for communities that existed when
-- it ran, and the boot-time chat.Repo.EnsureDefaultChannel ensures ONLY the
-- single bootstrap community (cmd/app/main.go) -- so a community created at
-- runtime via the super-admin or admin "create community" flow before those
-- handlers were fixed has zero channels, and its first chat visit crashes with
-- "load channel: sql: no rows in result set". This heals those communities on
-- deploy.
--
-- Guarded by NOT EXISTS on the (community_id, slug='general') row so it is
-- idempotent (a no-op for communities that already have #general) and can
-- never hit the UNIQUE(community_id, slug) constraint. is_default = 1 so the
-- seeded row satisfies chat.Repo.DefaultChannel. Mirrors the seed in 00032.
INSERT INTO chat_channels (id, community_id, slug, name, topic, position, is_default, created_by, created_at)
SELECT lower(hex(randomblob(16))), c.id, 'general', 'general', '', 0, 1, NULL, strftime('%s','now')
FROM communities c
WHERE NOT EXISTS (
    SELECT 1 FROM chat_channels ch
    WHERE ch.community_id = c.id AND ch.slug = 'general'
);

-- +goose Down
-- No-op: a backfilled #general is indistinguishable from an original one, and
-- removing a community's only channel would re-introduce the crash. Leave the
-- seeded rows in place.
SELECT 1;
