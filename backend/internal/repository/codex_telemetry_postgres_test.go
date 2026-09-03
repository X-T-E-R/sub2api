package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// This opt-in fixture accepts only the explicitly named disposable local DB.
// All request records live in temporary tables on dedicated connections.
func codexTelemetryPostgres(t *testing.T) *sql.DB {
	t.Helper()
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
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	require.NoError(t, ApplyMigrations(ctx, db), "create the canonical schema, including columns intentionally absent from Ent")
	return db
}

func codexRepositorySnapshot() *codextelemetry.Snapshot {
	collector := codextelemetry.New(codextelemetry.WebSocket, false)
	collector.ObserveHeaders(http.Header{"X-Codex-Safety-Buffering-Faster-Model": {"buffering-hint"}, "X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Window-Minutes": {"300"}}, codextelemetry.WSUpgradeHeaders)
	collector.Observe([]byte(`{"type":"response.created","response":{"id":"resp-postgres"}}`), "response.created")
	collector.Observe([]byte(`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["opaque-engine"]}}`), codextelemetry.TimingEvent)
	return collector.Snapshot()
}

func TestCodexTelemetryPostgresPersistenceAndCleanup(t *testing.T) {
	db := codexTelemetryPostgres(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(ctx, `CREATE TEMP TABLE usage_logs (LIKE public.usage_logs INCLUDING DEFAULTS INCLUDING INDEXES)`)
	require.NoError(t, err)
	// The prior schema has no telemetry column; the additive migration preserves
	// a pre-existing row and works when replayed against the same parent.
	_, err = conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN codex_telemetry`)
	require.NoError(t, err)
	var legacyID int64
	require.NoError(t, conn.QueryRowContext(ctx, `INSERT INTO usage_logs (user_id,api_key_id,account_id,request_id,model) VALUES (1,2,3,'legacy-codex-fixture','gpt-5.1') RETURNING id`).Scan(&legacyID))
	migration, err := migrations.FS.ReadFile("234_usage_log_codex_telemetry.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = conn.ExecContext(ctx, string(migration))
		require.NoError(t, err)
	}
	var absent bool
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT codex_telemetry IS NULL FROM usage_logs WHERE id=$1`, legacyID).Scan(&absent))
	require.True(t, absent)

	repo := newUsageLogRepositoryWithSQL(nil, conn)
	snapshot := codexRepositorySnapshot()
	now := time.Now().UTC()
	newLog := func(id string, when time.Time) *service.UsageLog {
		return &service.UsageLog{UserID: 1, APIKeyID: 2, AccountID: 3, RequestID: id, Model: "gpt-5.1", RequestedModel: "gpt-5.1", InputTokens: 2, CodexTelemetry: snapshot, CreatedAt: when}
	}
	single := newLog("codex-single", now)
	inserted, err := repo.createSingle(ctx, conn, single)
	require.NoError(t, err)
	require.True(t, inserted)
	got, err := repo.GetByID(ctx, single.ID)
	require.NoError(t, err)
	require.True(t, got.CodexTelemetryAvailable)
	require.JSONEq(t, string(codextelemetry.Marshal(snapshot)), string(codextelemetry.Marshal(got.CodexTelemetry)))
	inserted, err = repo.createSingle(ctx, conn, single)
	require.NoError(t, err)
	require.False(t, inserted, "existing request deduplication is unchanged")
	items, _, err := repo.ListByAccount(ctx, 3, pagination.PaginationParams{Page: 1, PageSize: 20})
	require.NoError(t, err)
	for _, item := range items {
		require.Nil(t, item.CodexTelemetry, "list/filter reads carry presence only, never the JSON")
		if item.ID == single.ID {
			require.True(t, item.CodexTelemetryAvailable)
		}
	}

	batch := newLog("codex-batch", now.Add(-48*time.Hour))
	key := usageLogBatchKey(batch.RequestID, batch.APIKeyID)
	query, args := buildUsageLogBatchInsertQuery([]string{key}, map[string]usageLogInsertPrepared{key: prepareUsageLogInsert(batch)})
	var batchJSON []byte
	require.NoError(t, conn.QueryRowContext(ctx, query, args...).Scan(&batchJSON))
	var batchRows []usageLogBatchRow
	require.NoError(t, json.Unmarshal(batchJSON, &batchRows))
	require.Len(t, batchRows, 1)
	require.True(t, batchRows[0].Inserted)

	bestEffort := newLog("codex-best-effort", now.Add(-48*time.Hour))
	query, args = buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepareUsageLogInsert(bestEffort)})
	_, err = conn.ExecContext(ctx, query, args...)
	require.NoError(t, err)
	require.NoError(t, execUsageLogInsertNoResult(ctx, conn, prepareUsageLogInsert(newLog("codex-no-result", now))))
	for _, requestID := range []string{batch.RequestID, bestEffort.RequestID, "codex-no-result"} {
		var raw []byte
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT codex_telemetry FROM usage_logs WHERE request_id=$1`, requestID).Scan(&raw))
		require.JSONEq(t, string(codextelemetry.Marshal(snapshot)), string(raw), requestID)
	}
	var storedBytes, databaseTextBytes int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_column_size(codex_telemetry), octet_length(codex_telemetry::text) FROM usage_logs WHERE id=$1`, single.ID).Scan(&storedBytes, &databaseTextBytes))
	t.Logf("typical payload: compact JSON=%d bytes, PostgreSQL JSONB text=%d bytes, pg_column_size=%d bytes", len(codextelemetry.Marshal(snapshot)), databaseTextBytes, storedBytes)
	largeCollector := codextelemetry.New(codextelemetry.HTTP, false)
	random := rand.New(rand.NewPCG(42, 17))
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for range 16 {
		engines := make([]string, 8)
		for index := range engines {
			value := make([]byte, codextelemetry.MaxIDBytes)
			for i := range value {
				value[i] = alphabet[random.IntN(len(alphabet))]
			}
			engines[index] = string(value)
		}
		frame, marshalErr := json.Marshal(map[string]any{"type": codextelemetry.TimingEvent, "timing_metrics": map[string]any{"engine_ids": engines}})
		require.NoError(t, marshalErr)
		largeCollector.Observe(frame, codextelemetry.TimingEvent)
	}
	large := newLog("codex-large", now)
	large.CodexTelemetry = largeCollector.Snapshot()
	_, err = repo.createSingle(ctx, conn, large)
	require.NoError(t, err)
	require.LessOrEqual(t, len(codextelemetry.Marshal(large.CodexTelemetry)), codextelemetry.MaxSnapshotBytes)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_column_size(codex_telemetry), octet_length(codex_telemetry::text) FROM usage_logs WHERE id=$1`, large.ID).Scan(&storedBytes, &databaseTextBytes))
	t.Logf("high-entropy cap fixture: compact JSON=%d bytes, PostgreSQL JSONB text=%d bytes, pg_column_size=%d bytes", len(codextelemetry.Marshal(large.CodexTelemetry)), databaseTextBytes, storedBytes)
	_, err = conn.ExecContext(ctx, `CREATE TEMP TABLE ops_error_logs (LIKE public.ops_error_logs INCLUDING DEFAULTS INCLUDING INDEXES)`)
	require.NoError(t, err)
	opsInput := &service.OpsInsertErrorLogInput{RequestID: "codex-ops-fixture", Platform: "openai", Model: "gpt-5.1", StatusCode: 429, ErrorPhase: "upstream", ErrorType: "rate_limit_error", ErrorMessage: "synthetic", ErrorOwner: "provider", ErrorSource: "upstream_http", Severity: "warning", CreatedAt: now,
		UpstreamErrors: []*service.OpsUpstreamErrorEvent{{AccountID: 3, UpstreamStatusCode: 429, CodexTelemetry: snapshot}},
	}
	require.NoError(t, service.SanitizeOpsUpstreamErrorsForQueue(opsInput))
	var opsID int64
	require.NoError(t, conn.QueryRowContext(ctx, insertOpsErrorLogSQL+" RETURNING id", opsInsertErrorLogArgs(opsInput)...).Scan(&opsID))
	var opsJSON string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT upstream_errors FROM ops_error_logs WHERE id=$1`, opsID).Scan(&opsJSON))
	require.JSONEq(t, *opsInput.UpstreamErrorsJSON, opsJSON)

	// Both manual filters and automatic retention delete the whole native row.
	cleanup := newUsageCleanupRepositoryWithSQL(nil, conn)
	model := "gpt-5.1"
	deleted, err := cleanup.DeleteUsageLogsBatch(ctx, service.UsageCleanupFilters{StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour), AccountID: &single.AccountID, Model: &model}, 5000)
	require.NoError(t, err)
	require.GreaterOrEqual(t, deleted, int64(2))
	_, err = repo.GetByID(ctx, single.ID)
	require.ErrorIs(t, err, service.ErrUsageLogNotFound)
	aggregation := newDashboardAggregationRepositoryWithSQL(conn)
	require.NoError(t, aggregation.cleanupUsageLogsBatches(ctx, now.Add(-24*time.Hour)))
	var remaining int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs WHERE codex_telemetry IS NOT NULL`).Scan(&remaining))
	require.Zero(t, remaining)

	// The same additive column propagates to an existing monthly partition.
	partitionConn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = partitionConn.Close() }()
	_, err = partitionConn.ExecContext(ctx, `CREATE TEMP TABLE usage_logs (id BIGINT, created_at TIMESTAMPTZ) PARTITION BY RANGE (created_at);
CREATE TEMP TABLE codex_usage_partition PARTITION OF usage_logs FOR VALUES FROM ('2020-01-01') TO ('2030-01-01')`)
	require.NoError(t, err)
	_, err = partitionConn.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = partitionConn.ExecContext(ctx, `INSERT INTO usage_logs VALUES (1, '2026-01-01', $1)`, string(codextelemetry.Marshal(snapshot)))
	require.NoError(t, err)
	var inherited []byte
	require.NoError(t, partitionConn.QueryRowContext(ctx, `SELECT codex_telemetry FROM codex_usage_partition`).Scan(&inherited))
	require.JSONEq(t, string(codextelemetry.Marshal(snapshot)), string(inherited))
	_, err = partitionConn.ExecContext(ctx, `DROP TABLE codex_usage_partition`)
	require.NoError(t, err)
	require.NoError(t, partitionConn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs`).Scan(&remaining))
	require.Zero(t, remaining)
}

func TestCodexTelemetryInsertArgumentIsNullableOwnedAndBounded(t *testing.T) {
	log := &service.UsageLog{RequestID: "bounded", Model: "gpt-5.1", CodexTelemetry: codexRepositorySnapshot()}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, 73)
	require.Equal(t, "jsonb", usageLogInsertArgTypes[72])
	raw, ok := prepared.args[72].(string)
	require.True(t, ok)
	require.LessOrEqual(t, len(raw), codextelemetry.MaxSnapshotBytes)
	log.CodexTelemetry.Observations[0].FasterModel = "mutated-after-queue"
	require.NotContains(t, prepared.args[72], "mutated-after-queue")
	log.CodexTelemetry = nil
	require.Nil(t, prepareUsageLogInsert(log).args[72])
	for _, builder := range []func() (string, []any){
		func() (string, []any) { return buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared}) },
		func() (string, []any) {
			return buildUsageLogBatchInsertQuery([]string{"key"}, map[string]usageLogInsertPrepared{"key": prepared})
		},
	} {
		query, args := builder()
		require.Contains(t, query, "codex_telemetry")
		require.Equal(t, raw, args[len(args)-1], fmt.Sprintf("last JSONB argument of %d args", len(args)))
	}
}
