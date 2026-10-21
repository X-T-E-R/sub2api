package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func streamChunk(t *testing.T, p *StreamingProcessor, response string) []byte {
	t.Helper()
	require.True(t, json.Valid([]byte(response)))
	return p.ProcessLine("data: " + response)
}

// Reconstruct inputs the way an SSE consumer does: concatenate partial_json
// within each block, then parse it once at content_block_stop.
func streamBlocks(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var blocks []map[string]any
	var active map[string]any
	var input strings.Builder
	for _, event := range decodeSSEData(payload) {
		switch event["type"] {
		case "content_block_start":
			require.Nil(t, active, "previous block must close before a new one starts")
			require.Equal(t, float64(len(blocks)), event["index"])
			block, ok := event["content_block"].(map[string]any)
			require.True(t, ok, "content_block must be an object")
			active = block
			if active["type"] == "tool_use" {
				require.NotEmpty(t, active["name"], "a continuation is not a new unnamed tool")
				require.NotEmpty(t, active["id"])
			}
			input.Reset()
		case "content_block_delta":
			require.NotNil(t, active)
			require.Equal(t, float64(len(blocks)), event["index"])
			delta, ok := event["delta"].(map[string]any)
			require.True(t, ok, "delta must be an object")
			switch delta["type"] {
			case "input_json_delta":
				require.Equal(t, "tool_use", active["type"])
				partialJSON, ok := delta["partial_json"].(string)
				require.True(t, ok, "partial_json must be a string")
				_, _ = input.WriteString(partialJSON)
			case "signature_delta":
				active["signature"] = delta["signature"]
			case "thinking_delta":
				thinking, ok := active["thinking"].(string)
				require.True(t, ok, "thinking block must contain a string")
				fragment, ok := delta["thinking"].(string)
				require.True(t, ok, "thinking delta must contain a string")
				active["thinking"] = thinking + fragment
			}
		case "content_block_stop":
			require.NotNil(t, active)
			require.Equal(t, float64(len(blocks)), event["index"])
			if input.Len() > 0 {
				var args map[string]any
				require.NoError(t, json.Unmarshal([]byte(input.String()), &args), "tool input: %s", input.String())
				active["input"] = args
			}
			blocks = append(blocks, active)
			active = nil
		}
	}
	require.Nil(t, active, "finalization must close the last block")
	return blocks
}

func TestStreamingFunctionCallNameThenArgs(t *testing.T) {
	for _, finishInChunk := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "STOP"}[finishInChunk], func(t *testing.T) {
			p := NewStreamingProcessor("gemini-3.8-flash")
			payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_lookup","name":"lookup"},"thoughtSignature":"sig-tool"}]}}]}}`)
			require.NotContains(t, string(payload), "content_block_stop", "name-only part must stay open")
			payload = append(payload, streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"query":"Tokyo"}}}]}}]}}`)...)
			if finishInChunk {
				payload = append(payload, streamChunk(t, p, `{"response":{"candidates":[{"finishReason":"STOP"}]}}`)...)
			}
			final, _ := p.Finish()
			payload = append(payload, final...)
			blocks := streamBlocks(t, payload)
			require.Len(t, blocks, 1)
			require.Equal(t, "lookup", blocks[0]["name"])
			require.Equal(t, "call_lookup", blocks[0]["id"])
			require.Equal(t, "sig-tool", blocks[0]["signature"])
			require.Equal(t, map[string]any{"query": "Tokyo"}, blocks[0]["input"])
			require.True(t, p.HasContent())
			require.Equal(t, 1, strings.Count(string(payload), "event: message_stop\n"))
			require.Contains(t, string(payload), `"stop_reason":"tool_use"`)
			again, _ := p.Finish()
			require.Empty(t, again)
		})
	}
}

func TestStreamingFunctionCallObjectContinuationsAndTransitions(t *testing.T) {
	p := NewStreamingProcessor("gemini-3.8-flash")
	payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"},{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"sig-lookup"}]}}]}}`)
	payload = append(payload, streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"query":"Tokyo"}}},{"functionCall":{"args":{"limit":2}}},{"functionCall":{"name":"lookup","id":"second","args":{"query":"Osaka"}}},{"text":"","thoughtSignature":"sig-after-tool"},{"text":"done"}]},"finishReason":"STOP"}]}}`)...)
	blocks := streamBlocks(t, payload)
	require.Len(t, blocks, 5)
	require.Equal(t, "sig-plan", blocks[0]["signature"])
	require.Equal(t, "plan", blocks[0]["thinking"])
	require.Equal(t, "sig-lookup", blocks[1]["signature"])
	require.Equal(t, map[string]any{"query": "Tokyo", "limit": float64(2)}, blocks[1]["input"])
	require.Equal(t, "lookup", blocks[2]["name"], "a named part always starts a distinct call, even with the same name")
	require.Equal(t, "second", blocks[2]["id"])
	require.Equal(t, map[string]any{"query": "Osaka"}, blocks[2]["input"])
	require.Equal(t, "thinking", blocks[3]["type"])
	require.Equal(t, "sig-after-tool", blocks[3]["signature"])
	require.Equal(t, "text", blocks[4]["type"])
}

func TestStreamingFunctionCallCompleteAndParameterless(t *testing.T) {
	p := NewStreamingProcessor("gemini-3.8-flash")
	payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"first","args":{"nested":{"x":[1,true,"a"]}}}},{"functionCall":{"name":"empty","args":{}}},{"functionCall":{"name":"no_args"}}]},"finishReason":"STOP"}]}}`)
	blocks := streamBlocks(t, payload)
	require.Len(t, blocks, 3)
	require.Equal(t, map[string]any{"nested": map[string]any{"x": []any{float64(1), true, "a"}}}, blocks[0]["input"])
	require.Equal(t, map[string]any{}, blocks[1]["input"])
	require.Equal(t, map[string]any{}, blocks[2]["input"])
	require.NotEqual(t, blocks[0]["id"], blocks[1]["id"])
}

func TestStreamingUsageCacheAccounting(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		metadata              string
		input, output, cached int
	}{
		{"normal", `{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":7,"thoughtsTokenCount":3,"totalTokenCount":110}`, 20, 10, 80},
		{"cached prompt is not output", `{"promptTokenCount":142871,"cachedContentTokenCount":140830,"totalTokenCount":142871}`, 2041, 0, 140830},
		{"cached exceeds reported prompt", `{"promptTokenCount":5,"cachedContentTokenCount":8,"candidatesTokenCount":2}`, 0, 2, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewStreamingProcessor("gemini-3.8-flash")
			payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":`+tc.metadata+`}}`)
			final, usage := p.Finish()
			payload = append(payload, final...)
			for _, event := range decodeSSEData(payload) {
				var usage map[string]any
				var ok bool
				switch event["type"] {
				case "message_start":
					message, isObject := event["message"].(map[string]any)
					require.True(t, isObject, "message must be an object")
					usage, ok = message["usage"].(map[string]any)
				case "message_delta":
					usage, ok = event["usage"].(map[string]any)
				default:
					continue
				}
				require.True(t, ok, "usage must be an object")
				require.Equal(t, float64(tc.input), usage["input_tokens"])
				require.Equal(t, float64(tc.output), usage["output_tokens"])
				require.Equal(t, float64(tc.cached), usage["cache_read_input_tokens"])
			}
			require.Equal(t, tc.input, usage.InputTokens)
			require.Equal(t, tc.output, usage.OutputTokens)
			require.Equal(t, tc.cached, usage.CacheReadInputTokens)
		})
	}
}

func TestStreamingThinkingOnlyTerminalPolicyUnchanged(t *testing.T) {
	for _, withhold := range []bool{false, true} {
		p := NewStreamingProcessor("gemini-3.8-flash")
		p.SetWithholdSuccessfulTerminal(withhold)
		payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"plan","thoughtSignature":"sig-plan"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":3}}}`)
		p.SetUsageOnlyTerminal(withhold)
		final, usage := p.Finish()
		payload = append(payload, final...)
		blocks := streamBlocks(t, payload)
		require.Len(t, blocks, 1)
		require.Equal(t, "thinking", blocks[0]["type"])
		require.Equal(t, "plan", blocks[0]["thinking"])
		require.Equal(t, "sig-plan", blocks[0]["signature"])
		require.Equal(t, 3, usage.OutputTokens)
		if withhold {
			require.NotContains(t, string(payload), "event: message_stop\n")
			require.NotContains(t, string(payload), `"stop_reason":"end_turn"`)
			require.Contains(t, string(payload), `"stop_reason":null`)
		} else {
			require.Contains(t, string(payload), `"stop_reason":"end_turn"`)
			require.Equal(t, 1, strings.Count(string(payload), "event: message_stop\n"))
		}
	}
}
