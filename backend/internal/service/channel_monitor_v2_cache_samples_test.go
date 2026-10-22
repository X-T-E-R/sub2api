package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func cacheSampleMetric(t *testing.T, body string) ChannelMonitorV2Metric {
	t.Helper()
	var metric ChannelMonitorV2Metric
	require.NoError(t, json.Unmarshal([]byte(body), &metric))
	return metric
}

func requireCacheSamplesJSON(t *testing.T, metric ChannelMonitorV2Metric, want bool) {
	t.Helper()
	encoded, err := json.Marshal(metric)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Contains(t, payload, "cache_rate_has_samples")
	require.Equal(t, want, payload["cache_rate_has_samples"])
}

func TestRedactChannelMonitorV2KeepsCacheSamplePresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "real zero", body: `{"cache_rate_has_samples":true,"cache_rate":0,"cache_rate_denominator":100,"input_tokens":100,"request_count":2,"rpm":2,"tpm":100}`, want: true},
		{name: "partial", body: `{"cache_rate_has_samples":true,"cache_rate":0.8,"cache_rate_numerator":80,"cache_rate_denominator":100,"input_tokens":20,"cache_read_tokens":80,"request_count":2,"rpm":2,"tpm":100}`, want: true},
		{name: "empty", body: `{"cache_rate_has_samples":false,"cache_rate":0}`},
	} {
		for _, hideThroughput := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "visible rates", true: "hidden rates"}[hideThroughput], func(t *testing.T) {
				metric := cacheSampleMetric(t, tc.body)
				rate := metric.CacheRate
				redactChannelMonitorV2Metric(&metric, hideThroughput)
				requireCacheSamplesJSON(t, metric, tc.want)
				require.Equal(t, rate, metric.CacheRate)
				require.Zero(t, metric.CacheRateNumerator)
				require.Zero(t, metric.CacheRateDenominator)
				require.Zero(t, metric.RequestCount)
				require.Zero(t, metric.InputTokens)
				require.Zero(t, metric.CacheReadTokens)
				if hideThroughput {
					require.Zero(t, metric.RPM)
					require.Zero(t, metric.TPM)
				}
				redactChannelMonitorV2Metric(&metric, hideThroughput)
				requireCacheSamplesJSON(t, metric, tc.want)
			})
		}
	}
}

func TestChannelMonitorV2PublicSnapshotKeepsCacheSamplesInTrend(t *testing.T) {
	withSamples := cacheSampleMetric(t, `{"cache_rate_has_samples":true,"cache_rate":0,"cache_rate_denominator":100,"input_tokens":100,"request_count":2}`)
	empty := cacheSampleMetric(t, `{"cache_rate_has_samples":false,"cache_rate":0}`)
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true},
		snap: &ChannelMonitorV2Snapshot{
			Metrics: withSamples,
			Trend:   []ChannelMonitorV2TrendPoint{{Metrics: withSamples}, {Metrics: empty}},
		},
	}
	snapshot, err := NewChannelMonitorV2Service(repo).Snapshot(context.Background(), ChannelMonitorV2Filter{}, false)
	require.NoError(t, err)
	requireCacheSamplesJSON(t, snapshot.Metrics, true)
	require.Zero(t, snapshot.Metrics.CacheRateDenominator)
	requireCacheSamplesJSON(t, snapshot.Trend[0].Metrics, true)
	require.Zero(t, snapshot.Trend[0].Metrics.CacheRateDenominator)
	requireCacheSamplesJSON(t, snapshot.Trend[1].Metrics, false)
}
