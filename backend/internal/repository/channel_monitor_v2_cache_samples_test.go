package repository

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2MetricCacheSamplePresence(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		input, read, write, output int64
		wantSamples                bool
		wantRate                   float64
	}{
		{name: "empty"},
		{name: "output alone is not an input sample", output: 10},
		{name: "real zero hit", input: 100, wantSamples: true},
		{name: "fully cached", read: 100, wantSamples: true, wantRate: 1},
		{name: "partial cache", input: 20, read: 80, wantSamples: true, wantRate: .8},
		{name: "cache write counts as input", write: 100, wantSamples: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMetricAccumulator()
			a.input, a.cacheRead, a.cacheCreation, a.output = tc.input, tc.read, tc.write, tc.output
			for _, admin := range []bool{false, true} {
				metric := a.metric(1, admin)
				encoded, err := json.Marshal(metric)
				require.NoError(t, err)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(encoded, &payload))
				require.Contains(t, payload, "cache_rate_has_samples", "empty samples must be explicit, not omitted")
				require.Equal(t, tc.wantSamples, payload["cache_rate_has_samples"])
				require.InDelta(t, tc.wantRate, metric.CacheRate, .000001)
			}
		})
	}
}
