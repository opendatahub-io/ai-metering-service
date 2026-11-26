-- AI Metering Service schema.
--
-- The usage ledger is normalized: `usage` is 1:1 with the CloudEvent and
-- holds the event-level facts (identity, resource, usage) exactly once;
-- `usage_charges` holds the per-charge facts — one row per budget charged,
-- keyed by (usage_id, budget_ref), which is also the per-charge idempotency
-- marker for redelivered events.

CREATE TABLE rates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    items       JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE budgets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    "limit"         BIGINT NOT NULL,
    period_start    TIMESTAMPTZ NOT NULL,
    period_duration TEXT NOT NULL,
    renewal_type    TEXT NOT NULL CHECK (renewal_type IN ('drain', 'rollover')),
    max_carry       BIGINT,
    -- Next period boundary at which the Metering Service refills the
    -- balance. Derived from period_start + period_duration on create and
    -- update, advanced by the renewal loop when a boundary is processed.
    -- The unindexed scan would be fine at this scale (one sweep per
    -- interval over a small table), but the index keeps the sweep O(due).
    next_renewal_at TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX idx_budgets_next_renewal ON budgets (next_renewal_at);

CREATE TABLE budget_policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    budget_ref      TEXT NOT NULL,
    rate_ref        TEXT NOT NULL,
    subjects        JSONB NOT NULL DEFAULT '[]',
    overage         JSONB,
    deduct_overage  BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_budget_policies_tenant ON budget_policies (tenant_id);
-- Backs FindPoliciesForIdentity's per-event `subjects @>` lookups: a GIN
-- index makes each containment condition an index scan (combined via
-- BitmapOr) instead of filtering every policy row in the tenant. The
-- jsonb_path_ops opclass is smaller and faster than the default jsonb_ops
-- and supports the containment operator these queries use exclusively.
CREATE INDEX idx_budget_policies_subjects ON budget_policies USING GIN (subjects jsonb_path_ops);

-- Event-level facts, one row per CloudEvent.
CREATE TABLE usage (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    request_id  TEXT NOT NULL,
    "timestamp" TIMESTAMPTZ NOT NULL,
    identity    JSONB NOT NULL,
    resource    JSONB NOT NULL,
    usage       JSONB NOT NULL,
    UNIQUE (tenant_id, event_id)
);

-- Per-charge facts, one row per budget charged per event.
CREATE TABLE usage_charges (
    id         BIGSERIAL PRIMARY KEY,
    usage_id   BIGINT NOT NULL REFERENCES usage(id) ON DELETE CASCADE,
    budget_ref TEXT NOT NULL,
    bu_cost    BIGINT NOT NULL,
    UNIQUE (usage_id, budget_ref)
);

CREATE INDEX idx_usage_charges_budget ON usage_charges (budget_ref);
CREATE INDEX idx_usage_tenant_timestamp ON usage (tenant_id, "timestamp");
CREATE INDEX idx_usage_identity ON usage USING GIN (identity);

-- Aggregate function: merges JSONB objects by summing numeric values per key.
-- Used by the usage query to aggregate dynamic usage maps across rows.
CREATE OR REPLACE FUNCTION jsonb_sum_sfunc(state jsonb, val jsonb) RETURNS jsonb AS $$
  SELECT CASE
    WHEN state IS NULL THEN val
    WHEN val IS NULL THEN state
    ELSE (
      SELECT jsonb_object_agg(key, COALESCE((state->>key)::bigint, 0) + COALESCE(value::bigint, 0))
      FROM jsonb_each_text(state || val)
    )
  END;
$$ LANGUAGE sql IMMUTABLE;

CREATE AGGREGATE jsonb_sum_agg(jsonb) (
  SFUNC = jsonb_sum_sfunc,
  STYPE = jsonb,
  INITCOND = '{}'
);
