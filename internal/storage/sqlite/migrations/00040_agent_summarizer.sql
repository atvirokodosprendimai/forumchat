-- +goose Up

-- Designate which agent handles the chat /resume (channel summary) slash
-- command. Exactly one agent per community may carry the flag (the service
-- clears the others on save); when none is marked, /resume falls back to the
-- first enabled agent. Marking a vision-capable agent lets /resume forward the
-- channel's recent image attachments to the model.
ALTER TABLE ai_agents ADD COLUMN is_summarizer INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE ai_agents DROP COLUMN is_summarizer;
