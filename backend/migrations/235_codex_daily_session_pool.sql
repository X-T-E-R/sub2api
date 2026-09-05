-- Durable conversation bindings are operational identity, not usage/log data.
CREATE TABLE IF NOT EXISTS codex_daily_session_days (
    account_scope TEXT NOT NULL,
    allocation_day DATE NOT NULL,
    budget INTEGER NOT NULL CHECK (budget BETWEEN 1 AND 1000),
    sessions TEXT[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (account_scope, allocation_day),
    CHECK (cardinality(sessions) <= budget)
);
CREATE TABLE IF NOT EXISTS codex_daily_session_bindings (
    account_scope TEXT NOT NULL,
    binding_key TEXT NOT NULL,
    session_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_scope, binding_key)
);
CREATE INDEX IF NOT EXISTS codex_daily_session_days_cleanup_idx ON codex_daily_session_days (allocation_day);
