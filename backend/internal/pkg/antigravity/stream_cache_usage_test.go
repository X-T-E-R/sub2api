package antigravity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Gemini may report its final usage in a separate frame after finishReason.
// An Anthropic consumer stops at message_stop, so usage must precede that event.
func TestStreamingFinalCacheUsageBeforeMessageStop(t *testing.T) {
	for _, finishReason := range []string{"STOP", "MAX_TOKENS"} {
		t.Run(finishReason, func(t *testing.T) {
			p := NewStreamingProcessor("configured-model")
			wire := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"`+finishReason+`"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":1}}}`)
			wire = append(wire, streamChunk(t, p, `{"response":{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":7,"thoughtsTokenCount":3}}}`)...)
			final, usage := p.Finish()
			wire = append(wire, final...)
			require.Equal(t, 20, usage.InputTokens)
			require.Equal(t, 80, usage.CacheReadInputTokens)
			require.Equal(t, 10, usage.OutputTokens)
			var downstreamUsage map[string]any
			for _, event := range decodeSSEData(wire) {
				if event["type"] == "message_stop" {
					break
				}
				if event["type"] == "message_delta" {
					downstreamUsage, _ = event["usage"].(map[string]any)
				}
			}
			require.NotNil(t, downstreamUsage)
			require.Equal(t, float64(20), downstreamUsage["input_tokens"])
			require.Equal(t, float64(80), downstreamUsage["cache_read_input_tokens"])
			require.Equal(t, float64(10), downstreamUsage["output_tokens"])
			require.Equal(t, 1, strings.Count(string(wire), "event: message_stop\n"))
			if finishReason == "MAX_TOKENS" {
				require.Contains(t, string(wire), `"stop_reason":"max_tokens"`)
			}
		})
	}
}
