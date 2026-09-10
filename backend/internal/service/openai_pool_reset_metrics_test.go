package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPoolResetStatsStoreRecordAndSnapshot(t *testing.T) {
	store := NewOpenAIPoolResetStatsStore()
	first := time.Date(2026, time.January, 3, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	third := second.Add(time.Minute)

	store.Record(OpenAIPoolResetEvent{AccountID: 21, Protocol: "openai_h2", Triggered: true, At: first})
	store.Record(OpenAIPoolResetEvent{AccountID: 21, Protocol: "openai_h2", At: second})
	store.Record(OpenAIPoolResetEvent{AccountID: 22, Protocol: "openai_h1", Triggered: true, At: third})

	snapshot := store.Snapshot()
	require.Equal(t, uint64(2), snapshot.Triggered)
	require.Equal(t, uint64(1), snapshot.Suppressed)
	require.Equal(t, third, *snapshot.LastResetAt)
	require.Equal(t, "process_memory", snapshot.Persistence)
	require.True(t, snapshot.ResetOnRestart)

	require.Len(t, snapshot.ByAccount, 2)
	require.Equal(t, int64(21), snapshot.ByAccount[0].AccountID)
	require.Equal(t, uint64(1), snapshot.ByAccount[0].Triggered)
	require.Equal(t, uint64(1), snapshot.ByAccount[0].Suppressed)
	require.Equal(t, int64(22), snapshot.ByAccount[1].AccountID)

	require.Len(t, snapshot.ByProtocol, 2)
	require.Equal(t, "openai_h2", snapshot.ByProtocol[0].Protocol)
	require.Equal(t, "openai_h1", snapshot.ByProtocol[1].Protocol)

	*snapshot.LastResetAt = first
	snapshot.ByAccount[0].LastResetAt = nil
	again := store.Snapshot()
	require.Equal(t, third, *again.LastResetAt, "snapshot must not expose mutable internal timestamps")
	require.NotNil(t, again.ByAccount[0].LastResetAt)

	latestStore := NewOpenAIPoolResetStatsStore()
	latestStore.Record(OpenAIPoolResetEvent{AccountID: 23, Protocol: "openai_h1", Triggered: true, At: third})
	latestStore.Record(OpenAIPoolResetEvent{AccountID: 23, Protocol: "openai_h1", Triggered: true, At: first})
	latest := latestStore.Snapshot()
	require.Equal(t, third, *latest.LastResetAt)
	require.Equal(t, third, *latest.ByAccount[0].LastResetAt)
	require.Equal(t, third, *latest.ByProtocol[0].LastResetAt)
}

type poolResetMetricsUpstreamStub struct {
	resetResult bool
}

func (s *poolResetMetricsUpstreamStub) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (s *poolResetMetricsUpstreamStub) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (s *poolResetMetricsUpstreamStub) ResetIdleConnectionPool(HTTPUpstreamPoolEntryToken, time.Duration) bool {
	return s.resetResult
}

func TestResetOpenAIUpstreamPoolOnCapacityShedRecordsTriggeredAndSuppressed(t *testing.T) {
	previous := processOpenAIPoolResetStats
	processOpenAIPoolResetStats = NewOpenAIPoolResetStatsStore()
	t.Cleanup(func() { processOpenAIPoolResetStats = previous })

	request, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	require.NoError(t, err)
	request = request.WithContext(WithHTTPUpstreamPoolEntryToken(
		WithHTTPUpstreamProfile(request.Context(), HTTPUpstreamProfileOpenAI),
		HTTPUpstreamPoolEntryToken{CacheKey: "account:42|proto:openai_h2", Generation: 1},
	))
	response := &http.Response{Request: request}
	account := &Account{ID: 42, Platform: PlatformOpenAI}
	upstream := &poolResetMetricsUpstreamStub{resetResult: true}
	gateway := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ResetOpenAIPoolOnCapacityShed: true}},
		httpUpstream: upstream,
	}
	payload := []byte(`{"error":{"code":"server_is_overloaded"}}`)

	gateway.resetOpenAIUpstreamPoolOnCapacityShed(nil, account, response, payload)
	upstream.resetResult = false
	gateway.resetOpenAIUpstreamPoolOnCapacityShed(nil, account, response, payload)

	snapshot := SnapshotOpenAIPoolResetStats()
	require.Equal(t, uint64(1), snapshot.Triggered)
	require.Equal(t, uint64(1), snapshot.Suppressed)
	require.Len(t, snapshot.ByAccount, 1)
	require.Equal(t, int64(42), snapshot.ByAccount[0].AccountID)
	require.Len(t, snapshot.ByProtocol, 1)
	require.Equal(t, "openai_h2", snapshot.ByProtocol[0].Protocol)
	require.NotNil(t, snapshot.LastResetAt)
}

func TestResetOpenAIUpstreamPoolOnCapacityShedMissingTokenIsSuppressed(t *testing.T) {
	previous := processOpenAIPoolResetStats
	processOpenAIPoolResetStats = NewOpenAIPoolResetStatsStore()
	t.Cleanup(func() { processOpenAIPoolResetStats = previous })

	request, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	require.NoError(t, err)
	request = request.WithContext(WithHTTPUpstreamProfile(request.Context(), HTTPUpstreamProfileOpenAI))
	response := &http.Response{Request: request}
	gateway := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ResetOpenAIPoolOnCapacityShed: true}},
		httpUpstream: &poolResetMetricsUpstreamStub{resetResult: true},
	}

	gateway.resetOpenAIUpstreamPoolOnCapacityShed(nil, &Account{ID: 43, Platform: PlatformOpenAI}, response, []byte(`{"error":{"code":"slow_down"}}`))

	snapshot := SnapshotOpenAIPoolResetStats()
	require.Equal(t, uint64(0), snapshot.Triggered)
	require.Equal(t, uint64(1), snapshot.Suppressed)
	require.Equal(t, "openai", snapshot.ByProtocol[0].Protocol)
}
