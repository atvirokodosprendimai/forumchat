-- +goose Up

-- Per-community choice of WHERE a triggered agent answers — the third
-- channel-agent switch alongside agents_chat_enabled (/bots) and
-- agents_autochat_enabled (/autochat). Until now a human trigger ALWAYS did
-- both (§6.9.0): opened a forum thread AND streamed an in-channel bubble. Some
-- communities want only one surface.
--
--   'both'    — forum thread + in-channel bubble (default; historical behaviour)
--   'channel' — in-channel kind='bot' bubble only, no thread
--   'thread'  — forum thread + chat announce only, no live bubble
--
-- Admin-set via the /surface slash command. TEXT (not an enum SQLite lacks);
-- an unrecognised value is treated as 'both' by the dispatcher, so a bad write
-- degrades to the safe historical behaviour rather than silencing agents.
ALTER TABLE communities ADD COLUMN agents_reply_surface TEXT NOT NULL DEFAULT 'both';

-- +goose Down
ALTER TABLE communities DROP COLUMN agents_reply_surface;
