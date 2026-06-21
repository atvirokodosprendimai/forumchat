-- +goose Up

-- Chat-agents: make per-community ai_agents first-class participants in the
-- live chat channel (roster bot icon, @mentionable, triggered in-line).
-- See: eidos/spec - chat-agents - in-channel-ai-participants-triggered-by-mention-or-prefix.md

-- Per-agent chat participation + trigger config. in_chat_enabled gates chat
-- participation independently of `enabled` (pane availability). trigger_mode:
--   mention — @<name> word-boundaried
--   prefix  — a line starts with trigger_prefix (multi-prefix agents need <prefix><name>)
--   both    — either of the above
--   all     — every non-bot message in a bound channel
-- avatar_url is the bot's display avatar (the pane never needed one).
ALTER TABLE ai_agents ADD COLUMN in_chat_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ai_agents ADD COLUMN trigger_mode    TEXT NOT NULL DEFAULT 'mention'
    CHECK (trigger_mode IN ('mention','prefix','both','all'));
ALTER TABLE ai_agents ADD COLUMN trigger_prefix  TEXT NOT NULL DEFAULT '.';
ALTER TABLE ai_agents ADD COLUMN avatar_url      TEXT NOT NULL DEFAULT '';

-- Admin-assigned channel scope. An agent appears in the roster / mention
-- autocomplete and is dispatched ONLY for its bound channels. Both FKs CASCADE.
CREATE TABLE ai_agent_channels (
    agent_id   TEXT NOT NULL REFERENCES ai_agents(id)     ON DELETE CASCADE,
    channel_id TEXT NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (agent_id, channel_id)
);
CREATE INDEX idx_ai_agent_channels_channel ON ai_agent_channels(channel_id);

-- A kind='bot' chat message is posted by an agent. bot_agent_id records which
-- agent (provenance: render link, loop attribution, regenerate). gen_status
-- tracks the streaming lifecycle of a bot bubble: '' for every non-bot message,
-- 'generating' while the model streams, 'done' / 'interrupted' terminal.
-- bot_name / bot_avatar_url (the denormalised display identity) already exist
-- from the webhooks migration (00042) and are reused.
ALTER TABLE chat_messages ADD COLUMN bot_agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL;
ALTER TABLE chat_messages ADD COLUMN gen_status   TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE chat_messages DROP COLUMN gen_status;
ALTER TABLE chat_messages DROP COLUMN bot_agent_id;
DROP INDEX IF EXISTS idx_ai_agent_channels_channel;
DROP TABLE IF EXISTS ai_agent_channels;
ALTER TABLE ai_agents DROP COLUMN avatar_url;
ALTER TABLE ai_agents DROP COLUMN trigger_prefix;
ALTER TABLE ai_agents DROP COLUMN trigger_mode;
ALTER TABLE ai_agents DROP COLUMN in_chat_enabled;
