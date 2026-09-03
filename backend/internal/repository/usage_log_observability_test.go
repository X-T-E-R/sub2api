package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPrepareUsageLogInsertRequestObservation(t *testing.T) {
	terminal := "response.completed"
	gatewayID := "gateway-1"
	clientID := "client-1"
	log := &service.UsageLog{
		UserID: 1, APIKeyID: 2, AccountID: 3, RequestID: "request-1", Model: "gpt-5",
		HandlerDurationMs:       intTestPtr(700),
		FirstVisibleOutputMs:    intTestPtr(400),
		SemanticOutputSeen:      boolTestPtr(true),
		TerminalKind:            &terminal,
		AttemptCount:            intTestPtr(3),
		AccountSwitchCount:      intTestPtr(1),
		FailedAttemptDurationMs: intTestPtr(250),
		RetryWaitMs:             intTestPtr(50),
		AccountSwitchMs:         intTestPtr(75),
		GatewayRequestID:        &gatewayID,
		ClientRequestID:         &clientID,
		AttemptLedger: &service.RequestAttemptLedger{
			Version: 1, TotalAttempts: 3, Attempts: []service.RequestAttemptEvidence{{Sequence: 1, AccountID: 9, Outcome: "failover", Reason: "http_503"}},
		},
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, 73)
	handlerDuration, ok := prepared.args[59].(sql.NullInt64)
	require.True(t, ok)
	firstVisible, ok := prepared.args[60].(sql.NullInt64)
	require.True(t, ok)
	semanticSeen, ok := prepared.args[61].(sql.NullBool)
	require.True(t, ok)
	terminalKind, ok := prepared.args[62].(sql.NullString)
	require.True(t, ok)
	gatewayRequestID, ok := prepared.args[68].(sql.NullString)
	require.True(t, ok)
	clientRequestID, ok := prepared.args[69].(sql.NullString)
	require.True(t, ok)
	require.Equal(t, int64(700), handlerDuration.Int64)
	require.Equal(t, int64(400), firstVisible.Int64)
	require.True(t, semanticSeen.Bool)
	require.Equal(t, terminal, terminalKind.String)
	require.Equal(t, gatewayID, gatewayRequestID.String)
	require.Equal(t, clientID, clientRequestID.String)
	ledgerJSON, ok := prepared.args[70].(string)
	require.True(t, ok)
	require.NotContains(t, ledgerJSON, "Authorization")
	var ledger service.RequestAttemptLedger
	require.NoError(t, json.Unmarshal([]byte(ledgerJSON), &ledger))
	require.Equal(t, 3, ledger.TotalAttempts)
}

func TestUsageLogRepositoryCorrelationFilterMatchesAllRequestIDs(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	condition := "request_id = \\$1 OR gateway_request_id = \\$1 OR client_request_id = \\$1 OR request_id = 'client:' \\|\\| \\$1 OR request_id = 'local:' \\|\\| \\$1"
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs WHERE \\(.*" + condition + ".*\\)").
		WithArgs("correlation-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("SELECT .*attempt_ledger IS NOT NULL.* FROM usage_logs WHERE \\(.*"+condition+".*\\) ORDER BY").
		WithArgs("correlation-1", 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"unused"}))

	logs, page, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20}, usagestats.UsageLogFilters{CorrelationID: "correlation-1"})
	require.NoError(t, err)
	require.Empty(t, logs)
	require.Equal(t, int64(0), page.Total)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScanUsageLogDetailRoundTripsAttemptLedgerAndHistoricalNulls(t *testing.T) {
	now := time.Now().UTC()
	base := legacyUsageLogObservabilityScanValues(now)
	require.Len(t, base, 61)
	ledger := `{"version":1,"total_attempts":2,"truncated":false,"attempts":[{"sequence":1,"account_id":7,"started_offset_ms":0,"selection_ms":1,"slot_wait_ms":2,"forward_ms":30,"outcome":"failover","reason":"http_503","semantic_output_seen":false}]}`
	values := append([]any{}, base[:len(base)-1]...)
	values = append(values,
		sql.NullInt64{Valid: true, Int64: 80},
		sql.NullInt64{},
		sql.NullBool{Valid: true, Bool: false},
		sql.NullString{Valid: true, String: "response.completed"},
		sql.NullInt64{Valid: true, Int64: 2},
		sql.NullInt64{Valid: true, Int64: 1},
		sql.NullInt64{Valid: true, Int64: 30},
		sql.NullInt64{Valid: true, Int64: 10},
		sql.NullInt64{Valid: true, Int64: 5},
		sql.NullString{Valid: true, String: "gateway-1"},
		sql.NullString{Valid: true, String: "client-1"},
		true,
		base[len(base)-1],
		false,
		sql.NullString{Valid: true, String: ledger},
		sql.NullString{},
	)
	log, err := scanUsageLogWithLedger(usageLogScannerStub{values: values})
	require.NoError(t, err)
	require.Equal(t, 80, *log.HandlerDurationMs)
	require.False(t, *log.SemanticOutputSeen)
	require.Nil(t, log.FirstVisibleOutputMs)
	require.True(t, log.AttemptLedgerAvailable)
	require.Equal(t, 2, log.AttemptLedger.TotalAttempts)
	require.Equal(t, "http_503", log.AttemptLedger.Attempts[0].Reason)

	historical, err := scanUsageLog(usageLogScannerStub{values: base})
	require.NoError(t, err)
	require.Nil(t, historical.HandlerDurationMs)
	require.Nil(t, historical.SemanticOutputSeen)
	require.False(t, historical.AttemptLedgerAvailable)
}

func legacyUsageLogObservabilityScanValues(now time.Time) []any {
	return []any{
		int64(1), int64(1), int64(2), int64(3), sql.NullString{Valid: true, String: "request-1"}, "gpt-5",
		sql.NullString{Valid: true, String: "gpt-5"}, sql.NullString{}, sql.NullString{}, sql.NullBool{}, sql.NullInt64{}, sql.NullInt64{},
		1, 2, 0, 0, 0, 0, 0, 0.0, 0, 0.0,
		0.1, 0.2, 0.0, 0.0, 0.3, 0.3, 1.0, sql.NullFloat64{},
		int16(service.BillingTypeBalance), int16(service.RequestTypeSync), false, false,
		sql.NullInt64{}, sql.NullInt64{}, sql.NullString{}, sql.NullString{},
		0, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
		0, sql.NullString{}, sql.NullInt64{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
		false, false, sql.NullInt64{}, sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullFloat64{}, sql.NullString{}, now,
	}
}

func intTestPtr(value int) *int    { return &value }
func boolTestPtr(value bool) *bool { return &value }
