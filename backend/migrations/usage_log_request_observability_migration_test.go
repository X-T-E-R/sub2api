package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageLogRequestObservabilityMigrations(t *testing.T) {
	columns, err := FS.ReadFile("232_usage_log_request_observability.sql")
	require.NoError(t, err)
	normalized := strings.Join(strings.Fields(string(columns)), " ")
	for _, field := range []string{
		"handler_duration_ms INTEGER",
		"first_visible_output_ms INTEGER",
		"semantic_output_seen BOOLEAN",
		"terminal_kind VARCHAR(64)",
		"attempt_count INTEGER",
		"account_switch_count INTEGER",
		"failed_attempt_duration_ms INTEGER",
		"retry_wait_ms INTEGER",
		"account_switch_ms INTEGER",
		"gateway_request_id VARCHAR(64)",
		"client_request_id VARCHAR(64)",
		"attempt_ledger JSONB",
	} {
		require.Contains(t, normalized, "ADD COLUMN IF NOT EXISTS "+field)
	}
	require.NotContains(t, strings.ToUpper(normalized), " UPDATE ", "historical rows must remain NULL")
	require.NotContains(t, strings.ToUpper(normalized), " DEFAULT ", "nullable observation fields must have no backfill default")

	indexes, err := FS.ReadFile("233_usage_log_request_observability_indexes_notx.sql")
	require.NoError(t, err)
	indexSQL := strings.Join(strings.Fields(string(indexes)), " ")
	require.Contains(t, indexSQL, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_gateway_request_id")
	require.Contains(t, indexSQL, "WHERE gateway_request_id IS NOT NULL")
	require.Contains(t, indexSQL, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_client_request_id")
	require.Contains(t, indexSQL, "WHERE client_request_id IS NOT NULL")
}
