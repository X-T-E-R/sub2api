-- Optional bounded upstream observations share the usage row's retention lifecycle.
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS codex_telemetry JSONB;
