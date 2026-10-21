package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicUsageDeltaProducerContract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		snapshots   bool
		delta       string
		input, read int
		write       int
	}{
		{"native sparse output keeps input buckets", false, `{"output_tokens":10}`, 100, 50, 20},
		{"native sparse cache update keeps uncached input", false, `{"cache_read_input_tokens":80,"output_tokens":10}`, 100, 80, 20},
		{"snapshot fully cached clears uncached and write", true, `{"input_tokens":0,"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"output_tokens":10}`, 0, 100, 0},
		{"snapshot uncached clears stale cache buckets", true, `{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":10}`, 100, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewAnthropicEventToResponsesStateWithOptions(AnthropicToResponsesOptions{UsageDeltasAreSnapshots: tc.snapshots})
			var start AnthropicStreamEvent
			require.NoError(t, json.Unmarshal([]byte(`{"type":"message_start","message":{"id":"msg_test","model":"configured-model","usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":20,"output_tokens":1}}}`), &start))
			AnthropicEventToResponsesEvents(&start, state)
			var delta AnthropicStreamEvent
			require.NoError(t, json.Unmarshal([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+tc.delta+`}`), &delta))
			AnthropicEventToResponsesEvents(&delta, state)
			// An event without usage does not erase the latest snapshot either.
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_delta"}, state)
			events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
			require.NotEmpty(t, events)
			usage := events[len(events)-1].Response.Usage
			require.Equal(t, tc.input, state.InputTokens)
			require.Equal(t, tc.read, state.CacheReadInputTokens)
			require.Equal(t, tc.write, state.CacheCreationInputTokens)
			require.Equal(t, tc.input+tc.read+tc.write, usage.InputTokens)
			require.Equal(t, 10, usage.OutputTokens)
			require.Equal(t, usage.InputTokens+10, usage.TotalTokens)
		})
	}
}
