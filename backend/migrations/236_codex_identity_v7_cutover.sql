-- Keep the cutover immutable across restarts and upgrades. Historical identity
-- mappings are deterministic, not stored; only newer time-bearing client IDs
-- opt into v7. Existing daily-session bindings are never rewritten.
INSERT INTO settings (key, value, updated_at)
VALUES ('codex_identity_v7_cutover_unix_ms', floor(extract(epoch FROM clock_timestamp()) * 1000)::bigint::text, NOW())
ON CONFLICT (key) DO NOTHING;
