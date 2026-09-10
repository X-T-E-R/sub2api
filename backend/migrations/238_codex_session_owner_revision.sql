-- Owner revisions make durable rebinding a compare-and-swap operation.
ALTER TABLE codex_session_account_owners
    ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1;
