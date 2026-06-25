-- +goose Up

-- Channel agents: agents reply in-channel (restoring the dormant kind='bot'
-- bubble), may converse with each other (bot-to-bot), can pose as regular
-- members, and the whole behaviour is admin-toggleable per community via the
-- /bots and /autochat slash commands. Every column defaults to today's
-- behaviour so existing communities are unchanged until an admin opts in.

-- chat_as_human: when 1, this agent's in-channel messages render like a normal
-- member (avatar/initial + name, no "AI" badge) — "treat the bot as a citizen".
-- 0 = the usual badged bot bubble.
ALTER TABLE ai_agents ADD COLUMN chat_as_human INTEGER NOT NULL DEFAULT 0;

-- bot_as_human is denormalised onto the message at insert from the agent's
-- chat_as_human flag, so the read path renders the right bubble without a JOIN
-- and the choice survives an edit/refetch (the chat read path never joins
-- ai_agents — see internal/chat/CLAUDE.md §6.4).
ALTER TABLE chat_messages ADD COLUMN bot_as_human INTEGER NOT NULL DEFAULT 0;

-- agents_chat_enabled  — the /bots master switch: do bound agents answer
--                        channel messages at all. Default 1 = on (preserves the
--                        existing "agents respond when triggered" behaviour).
-- agents_autochat_enabled — the /autochat switch: may agents trigger EACH OTHER
--                        (bot-to-bot). Default 0 = off (opt-in; bounded by a
--                        per-agent 15s cooldown when on).
ALTER TABLE communities ADD COLUMN agents_chat_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE communities ADD COLUMN agents_autochat_enabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE communities DROP COLUMN agents_autochat_enabled;
ALTER TABLE communities DROP COLUMN agents_chat_enabled;
ALTER TABLE chat_messages DROP COLUMN bot_as_human;
ALTER TABLE ai_agents DROP COLUMN chat_as_human;
