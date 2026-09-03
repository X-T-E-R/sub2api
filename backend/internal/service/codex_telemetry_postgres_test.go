package service

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestCodexTelemetryPostgresOpsRetention(t *testing.T) {
	raw := os.Getenv("CODEX_TELEMETRY_TEST_DSN")
	if raw == "" {
		t.Skip("CODEX_TELEMETRY_TEST_DSN is not set")
	}
	target, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", target.Hostname())
	require.Equal(t, "/codex_telemetry_test", target.Path)
	db, err := sql.Open("postgres", raw)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	// One physical connection keeps all mutations in its private temporary table.
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE ops_error_logs (id BIGSERIAL PRIMARY KEY, created_at TIMESTAMPTZ, upstream_errors JSONB)`)
	require.NoError(t, err)
	now := time.Now().UTC()
	metadata := `[{"upstream_status_code":429,"codex_telemetry":{"v":1,"transport":"websocket","observations":[{"source":"error_headers","association":"upstream_attempt","faster_model":"opaque-hint"}]}}]`
	_, err = db.ExecContext(ctx, `INSERT INTO ops_error_logs (created_at,upstream_errors) VALUES ($1,$3),($2,$3)`, now.AddDate(0, 0, -31), now, metadata)
	require.NoError(t, err)
	cutoff, truncate, ok := opsCleanupPlan(now, 30)
	require.True(t, ok)
	require.False(t, truncate)
	deleted, err := opsCleanupRunOne(ctx, db, truncate, cutoff, "ops_error_logs", "created_at", false, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	var remaining int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_error_logs WHERE upstream_errors IS NOT NULL`).Scan(&remaining))
	require.Equal(t, 1, remaining)
	cutoff, truncate, ok = opsCleanupPlan(now, 0)
	require.True(t, ok)
	deleted, err = opsCleanupRunOne(ctx, db, truncate, cutoff, "ops_error_logs", "created_at", false, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), deleted)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_error_logs`).Scan(&remaining))
	require.Zero(t, remaining)
}
