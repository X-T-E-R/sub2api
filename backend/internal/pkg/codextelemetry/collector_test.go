package codextelemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func created(c *Collector, id string) {
	c.Observe([]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)), "response.created")
}

func timing(c *Collector, id string, engines ...string) {
	payload := map[string]any{"type": TimingEvent, "timing_metrics": map[string]any{"engine_ids": engines}}
	if id != "" {
		payload["response_id"] = id
	}
	raw, _ := json.Marshal(payload)
	c.Observe(raw, TimingEvent)
}

func TestCollectorAllowlistAssociationAndOwnedSnapshot(t *testing.T) {
	c := New(WebSocket, true)
	c.ObserveHeaders(http.Header{
		"X-Codex-Safety-Buffering-Faster-Model": {"hint-only"},
		"X-Codex-Safety-Buffering-Enabled":      {"false"},
		"X-Codex-Primary-Used-Percent":          {"100"},
		"Authorization":                         {"secret-sentinel"},
	}, WSUpgradeHeaders)
	timing(c, "", "idle-must-not-bind")
	timing(c, "resp-other", "wrong-response")
	created(c, "resp-1")
	timing(c, "", "opaque-one", "opaque-one", "opaque-two")
	c.Observe([]byte(`{"type":"response.metadata","response_id":"resp-1","headers":{"x-codex-active-limit":"primary","x-codex-secondary-used-percent":"23.5","x-codex-secondary-window-minutes":"10080","cookie":"secret-sentinel"},"prompt":"prompt-sentinel"}`), ResponseMetadata)
	timing(c, "resp-1", "explicit-engine")
	first := c.Snapshot()
	require.Len(t, first.Observations, 4)
	require.Equal(t, "resp-1", first.ResponseID)
	require.True(t, first.ConnectionReused)
	require.Equal(t, Connection, first.Observations[0].Association)
	require.Equal(t, "hint-only", first.Observations[0].FasterModel)
	require.Equal(t, 100.0, *first.Observations[0].PrimaryUsedPercent)
	require.Equal(t, ActiveResponse, first.Observations[1].Association)
	require.Equal(t, []string{"opaque-one", "opaque-two"}, first.Observations[1].EngineIDs)
	require.Equal(t, ExplicitResponse, first.Observations[2].Association)
	require.Equal(t, 2, first.UnassociatedEvents)
	raw := string(Marshal(first))
	for _, excluded := range []string{"Authorization", "cookie", "secret-sentinel", "prompt-sentinel", "timing_metrics", "enabled", "wrong-response", "idle-must-not-bind"} {
		require.NotContains(t, raw, excluded)
	}
	first.Observations[1].EngineIDs[0] = "mutated"
	*first.Observations[0].PrimaryUsedPercent = 1
	require.Equal(t, "opaque-one", c.Snapshot().Observations[1].EngineIDs[0])
	require.Equal(t, 100.0, *c.Snapshot().Observations[0].PrimaryUsedPercent)
	c.Observe([]byte(`{"type":"response.completed","response":{"id":"resp-1"}}`), "response.completed")
	timing(c, "resp-1", "after-terminal")
	require.NotContains(t, string(Marshal(c.Snapshot())), "after-terminal")
}

func TestCollectorContextUnknownAndNamedLanes(t *testing.T) {
	c := New(WebSocket, false)
	timing(c, "resp-1", "before-created")
	c.Observe([]byte(`{"type":"responsesapi.websocket_timing","stream_id":"lane-b","timing_metrics":{"engine_ids":["other-lane"]}}`), TimingEvent)
	require.Nil(t, c.Snapshot(), "stray metadata cannot establish a generation")
	c.ObserveHeaders(http.Header{"X-Codex-Active-Limit": {"primary"}}, WSUpgradeHeaders)
	c.Observe([]byte(`{"type":"response.created","stream_id":"lane-a","response":{"id":"resp-1"}}`), "response.created")
	timing(c, "resp-1", "explicit-engine")
	timing(c, "", "inferred-engine")
	c.Observe([]byte(`{"type":"response.created","stream_id":"lane-b","response":{"id":"resp-2"}}`), "response.created")
	timing(c, "", "overlap-engine")
	timing(c, "resp-1", "late-first-response-engine")
	snapshot := c.Snapshot()
	require.Len(t, snapshot.Observations, 1, "overlap invalidates all event-derived observations")
	require.Equal(t, WSUpgradeHeaders, snapshot.Observations[0].Source)
	require.Equal(t, Connection, snapshot.Observations[0].Association)
	require.Empty(t, snapshot.ResponseID)
	require.Empty(t, snapshot.StreamID)
	require.Empty(t, snapshot.Observations[0].EngineIDs)
	require.Equal(t, 4, snapshot.UnassociatedEvents)

	httpCollector := New(HTTP, false)
	timing(httpCollector, "", "http-context-engine")
	require.Equal(t, HTTPResponse, httpCollector.Snapshot().Observations[0].Association)
	require.Empty(t, httpCollector.Snapshot().ResponseID)
}

func TestCollectorMetadataConflictInvalidatesEstablishedContext(t *testing.T) {
	for _, transport := range []string{HTTP, WebSocket} {
		for name, conflict := range map[string]string{
			"response":      `{"type":"responsesapi.websocket_timing","response_id":"resp-other","timing_metrics":{"engine_ids":["other-engine"]}}`,
			"explicit_lane": `{"type":"responsesapi.websocket_timing","response_id":"resp-current","stream_id":"lane-other","timing_metrics":{"engine_ids":["other-engine"]}}`,
			"idless_lane":   `{"type":"responsesapi.websocket_timing","stream_id":"lane-other","timing_metrics":{"engine_ids":["other-engine"]}}`,
		} {
			t.Run(transport+"/"+name, func(t *testing.T) {
				c := New(transport, false)
				source, association := HTTPHeaders, HTTPResponse
				if transport == WebSocket {
					source, association = WSUpgradeHeaders, Connection
				}
				c.ObserveHeaders(http.Header{"X-Codex-Active-Limit": {"real-header"}}, source)
				c.Observe([]byte(`{"type":"response.created","response":{"id":"resp-current"},"stream_id":"lane-current"}`), "response.created")
				timing(c, "resp-current", "earlier-engine")
				timing(c, "", "earlier-inferred-engine")
				c.Observe([]byte(conflict), TimingEvent)
				timing(c, "", "later-inferred-engine")
				timing(c, "resp-current", "later-explicit-engine")
				snapshot := c.Snapshot()
				require.NotNil(t, snapshot)
				require.Empty(t, snapshot.ResponseID)
				require.Empty(t, snapshot.StreamID)
				require.Equal(t, []Observation{{Source: source, Association: association, ActiveLimit: "real-header"}}, snapshot.Observations)
				require.Equal(t, 3, snapshot.UnassociatedEvents)
			})
		}
	}
}

func TestCollectorPendingErrorHeadersAreAttemptScoped(t *testing.T) {
	c := New(WebSocket, false)
	c.Observe([]byte(`{"type":"error","status":429,"headers":{"x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300","x-codex-safety-buffering-faster-model":"retry-hint"},"error":{"message":"private-sentinel"},"timing_metrics":{"engine_ids":["not-an-error-field"]}}`), "error")
	snapshot := c.Snapshot()
	require.Len(t, snapshot.Observations, 1)
	require.Equal(t, ErrorHeaders, snapshot.Observations[0].Source)
	require.Equal(t, UpstreamAttempt, snapshot.Observations[0].Association)
	require.Equal(t, "retry-hint", snapshot.Observations[0].FasterModel)
	require.Empty(t, snapshot.ResponseID)
	require.NotContains(t, string(Marshal(snapshot)), "sentinel")
	require.Empty(t, snapshot.Observations[0].EngineIDs)
}

func TestCollectorRejectsMalformedAndAmbiguousFields(t *testing.T) {
	for _, raw := range []string{
		`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["engine"]}`,
		`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["one"],"engine_ids":["two"]}}`,
		`{"type":"responsesapi.websocket_timing","response_id":"resp-1","response_id":"resp-2","timing_metrics":{"engine_ids":["engine"]}}`,
		`{"type":"responsesapi.websocket_timing","response_id":"resp-1","response":{"id":"resp-2"},"timing_metrics":{"engine_ids":["engine"]}}`,
		`{"type":"responsesapi.websocket_timing","response_id":17,"timing_metrics":{"engine_ids":["engine"]}}`,
		`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":"engine"}}`,
		`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":[null,{},3,"line\nbreak"]}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			c := New(WebSocket, false)
			created(c, "resp-1")
			c.Observe([]byte(raw), TimingEvent)
			require.Nil(t, c.Snapshot())
		})
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-0.1", "100.1", "1e1000", "not-a-number"} {
		c := New(HTTP, false)
		c.ObserveHeaders(http.Header{"X-Codex-Primary-Used-Percent": {value}}, HTTPHeaders)
		require.Nil(t, c.Snapshot(), value)
	}
	for _, value := range []string{"0", "-1", "527041", "300.5", "NaN"} {
		c := New(HTTP, false)
		c.ObserveHeaders(http.Header{"X-Codex-Primary-Window-Minutes": {value}}, HTTPHeaders)
		require.Nil(t, c.Snapshot(), value)
	}
	c := New(HTTP, false)
	c.ObserveHeaders(http.Header{"X-Codex-Active-Limit": {"one", "two"}, "x-codex-primary-used-percent": {"1"}, "X-Codex-Primary-Used-Percent": {"2"}}, HTTPHeaders)
	require.Nil(t, c.Snapshot())
	c.Observe([]byte(`{"type":"response.metadata","headers":{"x-codex-active-limit":"one","X-Codex-Active-Limit":"two","x-codex-primary-used-percent":100}}`), ResponseMetadata)
	require.Nil(t, c.Snapshot())
	created(c, "resp-1")
	timing(c, "resp-1", strings.Repeat("x", MaxIDBytes+1))
	require.Nil(t, c.Snapshot())
}

func TestCollectorBoundsAndLaterEvidence(t *testing.T) {
	c := New(WebSocket, false)
	created(c, "resp-1")
	for i := range 30 {
		c.Observe([]byte(fmt.Sprintf(`{"type":"codex.response.metadata","headers":{"x-codex-active-limit":"limit-%d"},"timestamp":%d}`, i, i)), CodexMetadata)
	}
	// Noisy metadata cannot exhaust the separately reserved timing budget.
	timing(c, "", "later-engine")
	require.True(t, c.Snapshot().Truncated)
	require.Contains(t, string(Marshal(c.Snapshot())), "later-engine")

	worst := New(WebSocket, true)
	created(worst, strings.Repeat("&", MaxIDBytes))
	for i := range 16 {
		engines := make([]string, 12)
		for j := range engines {
			engines[j] = fmt.Sprintf("%03d%03d%s", i, j, strings.Repeat("<", MaxIDBytes-6))
		}
		timing(worst, "", engines...)
	}
	raw := Marshal(worst.Snapshot())
	require.NotEmpty(t, raw)
	require.LessOrEqual(t, len(raw), MaxSnapshotBytes)
	require.True(t, worst.Snapshot().Truncated)
	require.Contains(t, string(raw), "015", "newer engine observations win under the byte cap")
	t.Logf("escaped worst-case serialized snapshot: %d bytes (cap %d)", len(raw), MaxSnapshotBytes)

	typical := New(WebSocket, false)
	typical.ObserveHeaders(http.Header{"X-Codex-Safety-Buffering-Faster-Model": {"gpt-5.6-luna"}, "X-Codex-Active-Limit": {"primary"}, "X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Window-Minutes": {"300"}}, WSUpgradeHeaders)
	created(typical, "resp-local-fixture")
	timing(typical, "", "gpt56sol-opaque", "gpt56lun-opaque")
	t.Logf("typical serialized snapshot: %d bytes", len(Marshal(typical.Snapshot())))

	oversized := New(HTTP, false)
	timing(oversized, "", "known")
	oversized.Observe([]byte(`{"type":"responsesapi.websocket_timing","padding":"`+strings.Repeat("x", MaxEventBytes)+`"}`), TimingEvent)
	require.True(t, oversized.Snapshot().Truncated)
}

func TestMarshalRechecksBoundaries(t *testing.T) {
	nan := math.NaN()
	huge := MaxWindowMinutes + 1
	snapshot := &Snapshot{Version: 1, Transport: HTTP, Observations: []Observation{{Source: HTTPHeaders, Association: HTTPResponse, PrimaryUsedPercent: &nan, PrimaryWindowMinutes: &huge}}}
	require.Empty(t, Marshal(snapshot))
	snapshot.Observations[0].FasterModel = "retained-hint"
	raw := Marshal(snapshot)
	require.NotEmpty(t, raw)
	require.NotContains(t, string(raw), "primary_used_percent")
	require.NotContains(t, string(raw), "primary_window_minutes")
	require.Empty(t, MarshalLimit(snapshot, 10))
	var nilCollector *Collector
	nilCollector.Observe(nil, TimingEvent)
	nilCollector.ObserveHeaders(nil, HTTPHeaders)
	require.Nil(t, nilCollector.Snapshot())
}
