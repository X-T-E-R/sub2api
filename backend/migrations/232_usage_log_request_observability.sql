-- Request-level timing and bounded retry evidence for OpenAI-compatible gateway calls.
-- All columns are nullable so historical rows retain an explicit unknown state.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS handler_duration_ms INTEGER,
    ADD COLUMN IF NOT EXISTS first_visible_output_ms INTEGER,
    ADD COLUMN IF NOT EXISTS semantic_output_seen BOOLEAN,
    ADD COLUMN IF NOT EXISTS terminal_kind VARCHAR(64),
    ADD COLUMN IF NOT EXISTS attempt_count INTEGER,
    ADD COLUMN IF NOT EXISTS account_switch_count INTEGER,
    ADD COLUMN IF NOT EXISTS failed_attempt_duration_ms INTEGER,
    ADD COLUMN IF NOT EXISTS retry_wait_ms INTEGER,
    ADD COLUMN IF NOT EXISTS account_switch_ms INTEGER,
    ADD COLUMN IF NOT EXISTS gateway_request_id VARCHAR(64),
    ADD COLUMN IF NOT EXISTS client_request_id VARCHAR(64),
    ADD COLUMN IF NOT EXISTS attempt_ledger JSONB;
