-- +goose Up

-- Agent forum threads: a triggered chat-agent now opens a FORUM THREAD instead
-- of streaming an in-channel bubble. The thread is owned by an agent; every
-- member reply is a prompt and the agent answers as the next post.
-- See: eidos/spec - chat-agents - …  /  memory/plan - 2606211139 - …

-- threads.agent_id marks an agent-owned thread (the thread itself is authored by
-- the triggering human; agent_id says which agent answers in it).
ALTER TABLE threads ADD COLUMN agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL;

-- An agent reply is a post with agent_id set. Its display identity is
-- denormalised (bot_name / bot_avatar_url) and gen_status tracks the streaming
-- lifecycle ('' | generating | done | interrupted). author_id stays NOT NULL
-- (posts carry FTS + RAG triggers — no table rebuild), so agent posts are
-- authored by the sentinel bot user below; the bot identity overrides on render.
ALTER TABLE posts ADD COLUMN agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL;
ALTER TABLE posts ADD COLUMN bot_name TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN bot_avatar_url TEXT NOT NULL DEFAULT '';
ALTER TABLE posts ADD COLUMN gen_status TEXT NOT NULL DEFAULT '';

-- Sentinel bot user: a single, disabled, non-login account that owns every
-- agent post's author_id FK. Never authenticates; real identity lives in the
-- post's agent_id / bot_name columns.
INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
VALUES ('agent-bot', 'agent-bot@system.local', '', 'disabled', 0, 0)
ON CONFLICT(id) DO NOTHING;

-- +goose Down
DELETE FROM users WHERE id = 'agent-bot';
ALTER TABLE posts DROP COLUMN gen_status;
ALTER TABLE posts DROP COLUMN bot_avatar_url;
ALTER TABLE posts DROP COLUMN bot_name;
ALTER TABLE posts DROP COLUMN agent_id;
ALTER TABLE threads DROP COLUMN agent_id;
