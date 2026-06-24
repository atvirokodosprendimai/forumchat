-- +goose Up
-- +goose StatementBegin

-- stripe_events deduplicates Stripe webhook deliveries. Stripe sends each event
-- at-least-once and may redeliver on any non-2xx response, so a replayed
-- checkout.session.completed could wrongly re-activate a since-canceled
-- subscription. billing.Service records each event id here before handling and
-- skips ids already seen — making webhook processing idempotent. See
-- eidos/spec - saas-platform-ai …
CREATE TABLE stripe_events (
    id         TEXT PRIMARY KEY,   -- Stripe event id (evt_…)
    created_at INTEGER NOT NULL
);

-- A Stripe customer maps to exactly one community. Enforce it so the
-- customer→community lookup in a subscription webhook is deterministic (a
-- duplicate would let one tenant's event touch another's row). Partial: most
-- rows have no customer id (NULL), which the unique index ignores.
CREATE UNIQUE INDEX idx_community_settings_stripe_customer
    ON community_settings(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_community_settings_stripe_customer;
DROP TABLE IF EXISTS stripe_events;

-- +goose StatementEnd
