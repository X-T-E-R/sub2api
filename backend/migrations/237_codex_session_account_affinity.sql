-- Session ownership is durable identity state. It is intentionally independent
-- of account deletion and daily-session allocation history.
CREATE TABLE IF NOT EXISTS codex_session_account_owners (
    binding_key TEXT PRIMARY KEY,
    account_scope TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The historical table is keyed by (account_scope, binding_key); affinity
-- restoration searches by binding_key alone and needs its own index.
CREATE INDEX IF NOT EXISTS codex_daily_session_bindings_binding_key_idx
    ON codex_daily_session_bindings (binding_key);
