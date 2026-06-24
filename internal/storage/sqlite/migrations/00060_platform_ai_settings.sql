-- +goose Up
-- +goose StatementBegin

-- Platform-AI opt-in state on the per-community tenant config row
-- (community_settings, migration 00055). All nullable / default-off so an
-- existing community keeps today's BYO behaviour until its owner opts in. See
-- eidos/spec - saas-platform-ai …
--
--   use_platform_ai             owner's master switch (request intent): route
--                               RAG embed + translate + agents to the operator's
--                               hosted compute instead of BYO.
--   platform_ai_status          lifecycle: '' | requested | approved_unpaid |
--                               active | canceled.
--   platform_ai_granted_free    super-admin sponsorship — authorizes platform AI
--                               without a Stripe subscription.
--   stripe_*                    subscription linkage; stripe_subscription_status
--                               is the mirror of Stripe's own state and the
--                               authority on "is the paid sub active".
--   platform_ai_requested_at    when the owner requested (for the approval queue).
--
-- Authorized = granted_free OR stripe_subscription_status = 'active'. Only an
-- authorized + use_platform_ai community reaches the platform-compute branch of
-- the resolver and gets metered.
ALTER TABLE community_settings ADD COLUMN use_platform_ai INTEGER;
ALTER TABLE community_settings ADD COLUMN platform_ai_status TEXT;
ALTER TABLE community_settings ADD COLUMN platform_ai_granted_free INTEGER;
ALTER TABLE community_settings ADD COLUMN stripe_customer_id TEXT;
ALTER TABLE community_settings ADD COLUMN stripe_subscription_id TEXT;
ALTER TABLE community_settings ADD COLUMN stripe_subscription_status TEXT;
ALTER TABLE community_settings ADD COLUMN platform_ai_requested_at INTEGER;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE community_settings DROP COLUMN platform_ai_requested_at;
ALTER TABLE community_settings DROP COLUMN stripe_subscription_status;
ALTER TABLE community_settings DROP COLUMN stripe_subscription_id;
ALTER TABLE community_settings DROP COLUMN stripe_customer_id;
ALTER TABLE community_settings DROP COLUMN platform_ai_granted_free;
ALTER TABLE community_settings DROP COLUMN platform_ai_status;
ALTER TABLE community_settings DROP COLUMN use_platform_ai;

-- +goose StatementEnd
