package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerminalFailureCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		reason string
		code   string
	}{
		{reason: "", code: ""},
		{reason: "STOP", code: ""},
		{reason: "MAX_TOKENS", code: ""},
		{reason: "FINISH_REASON_UNSPECIFIED", code: ""},
		{reason: "SAFETY", code: "antigravity_safety"},
		{reason: "RECITATION", code: "antigravity_safety"},
		{reason: "LANGUAGE", code: "antigravity_safety"},
		{reason: "BLOCKLIST", code: "antigravity_safety"},
		{reason: "PROHIBITED_CONTENT", code: "antigravity_safety"},
		{reason: "SPII", code: "antigravity_safety"},
		{reason: "IMAGE_SAFETY", code: "antigravity_safety"},
		{reason: "IMAGE_PROHIBITED_CONTENT", code: "antigravity_safety"},
		{reason: "MALFORMED_FUNCTION_CALL", code: "antigravity_malformed_function_call"},
		{reason: "MALFORMED_RESPONSE", code: "antigravity_malformed_response"},
		{reason: "OTHER", code: "antigravity_upstream_terminal_error"},
		{reason: "UNKNOWN", code: "antigravity_upstream_terminal_error"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.code, TerminalFailureCode(tc.reason))
			if tc.code != "" {
				require.NotEmpty(t, TerminalFailureMessage(tc.code))
			}
		})
	}
}

func TestStreamingAbnormalTerminalEmitsPartialError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		code   string
	}{
		{name: "malformed function", reason: "MALFORMED_FUNCTION_CALL", code: "antigravity_malformed_function_call"},
		{name: "malformed response", reason: "MALFORMED_RESPONSE", code: "antigravity_malformed_response"},
		{name: "other", reason: "OTHER", code: "antigravity_upstream_terminal_error"},
		{name: "unknown", reason: "SOMETHING_NEW", code: "antigravity_upstream_terminal_error"},
		{name: "safety", reason: "SAFETY", code: "antigravity_safety"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewStreamingProcessor("gemini-3.8-flash")
			wire := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"text":"partial","thoughtSignature":"sig-partial"}]},"finishReason":"`+tc.reason+`"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3}}}`)
			final, usage := p.Finish()
			wire = append(wire, final...)

			require.Equal(t, 11, usage.InputTokens)
			require.Equal(t, 3, usage.OutputTokens)
			events := decodeSSEData(wire)
			var errorEvent map[string]any
			var sawPartial bool
			for _, event := range events {
				switch event["type"] {
				case "content_block_start":
					block, _ := event["content_block"].(map[string]any)
					if block["type"] == "text" {
						sawPartial = true
					}
				case "error":
					errorEvent = event
				}
			}
			require.True(t, sawPartial)
			require.NotNil(t, errorEvent)
			require.Equal(t, "error", errorEvent["type"])
			detail, ok := errorEvent["error"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "api_error", detail["type"])
			require.Equal(t, tc.code, detail["code"])
			require.Equal(t, TerminalFailureMessage(tc.code), detail["message"])
			require.Equal(t, 1, strings.Count(string(wire), "event: error\n"))
			require.Equal(t, 1, strings.Count(string(wire), "event: message_delta\n"))
			require.NotContains(t, string(wire), "event: message_stop\n")
			require.NotContains(t, string(wire), `"stop_reason":"end_turn"`)
			require.NotContains(t, string(json.RawMessage(mustJSON(t, errorEvent))), "partial")
			require.NotContains(t, string(json.RawMessage(mustJSON(t, errorEvent))), "sig-partial")

			again, _ := p.Finish()
			require.Empty(t, again)
		})
	}
}

func TestStreamingAbnormalTerminalWithholdKeepsExistingUsageOnlyPolicy(t *testing.T) {
	p := NewStreamingProcessor("gemini-3.8-flash")
	p.SetWithholdSuccessfulTerminal(true)
	p.SetUsageOnlyTerminal(true)
	wire := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"text":"partial","thoughtSignature":"sig-partial"}]},"finishReason":"MALFORMED_RESPONSE"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}}`)
	final, usage := p.Finish()
	wire = append(wire, final...)

	require.Equal(t, 5, usage.InputTokens)
	require.Equal(t, 2, usage.OutputTokens)
	require.Contains(t, string(wire), `"stop_reason":null`)
	require.NotContains(t, string(wire), "event: error\n")
	require.NotContains(t, string(wire), "event: message_stop\n")
}

func TestStreamingMaxTokensTakesPrecedenceOverToolUse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		finish     string
		stopReason string
	}{
		{name: "max tokens", finish: "MAX_TOKENS", stopReason: "max_tokens"},
		{name: "normal stop", finish: "STOP", stopReason: "tool_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewStreamingProcessor("gemini-3.8-flash")
			wire := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"`+tc.finish+`"}]}}`)
			final, _ := p.Finish()
			wire = append(wire, final...)
			require.Contains(t, string(wire), `"stop_reason":"`+tc.stopReason+`"`)
		})
	}
}

func TestNonStreamingMaxTokensTakesPrecedenceOverToolUse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		finish     string
		stopReason string
	}{
		{name: "max tokens", finish: "MAX_TOKENS", stopReason: "max_tokens"},
		{name: "normal stop", finish: "STOP", stopReason: "tool_use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := TransformGeminiToClaude([]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"`+tc.finish+`"}]}}`), "gemini-3.8-flash")
			require.NoError(t, err)
			var response map[string]any
			require.NoError(t, json.Unmarshal(body, &response))
			require.Equal(t, tc.stopReason, response["stop_reason"])
			require.Equal(t, "message", response["type"])
		})
	}
}

func TestNonStreamingAbnormalTerminalReturnsPartialErrorBody(t *testing.T) {
	body, usage, err := TransformGeminiToClaude([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial","thoughtSignature":"sig-partial"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":3}}}`), "gemini-3.8-flash")
	require.NoError(t, err)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 3, usage.OutputTokens)

	var response map[string]any
	require.NoError(t, json.Unmarshal(body, &response))
	require.Equal(t, "error", response["type"])
	require.Nil(t, response["stop_reason"])
	failure, ok := response["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "antigravity_malformed_function_call", failure["code"])
	require.Equal(t, TerminalFailureMessage("antigravity_malformed_function_call"), failure["message"])
	responseUsage, ok := response["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(11), responseUsage["input_tokens"])
	require.NotContains(t, string(body), `"stop_reason":"end_turn"`)
	content := response["content"].([]any)
	require.Len(t, content, 1)
	require.Equal(t, "partial", content[0].(map[string]any)["text"])
	require.Equal(t, "sig-partial", content[0].(map[string]any)["signature"])
}

func TestPromptFeedbackBlockedResponsesUseSafetyFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "non streaming prompt block", body: `{"response":{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}}`},
		{name: "non streaming direct prompt block", body: `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}`},
		{name: "stream prompt block", body: `{"response":{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":4}}}`},
		{name: "candidate safety", body: `{"response":{"candidates":[{"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4}}}`},
		{name: "candidate blocked rating", body: `{"response":{"candidates":[{"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","blocked":true}]}],"usageMetadata":{"promptTokenCount":4}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "non streaming") {
				body, _, err := TransformGeminiToClaude([]byte(tc.body), "gemini-3.8-flash")
				require.NoError(t, err)
				var response map[string]any
				require.NoError(t, json.Unmarshal(body, &response))
				require.Equal(t, "antigravity_safety", response["error"].(map[string]any)["code"])
				return
			}

			p := NewStreamingProcessor("gemini-3.8-flash")
			wire := streamChunk(t, p, tc.body)
			final, _ := p.Finish()
			wire = append(wire, final...)
			var errorEvent map[string]any
			for _, event := range decodeSSEData(wire) {
				if event["type"] == "error" {
					errorEvent = event
				}
			}
			require.Equal(t, "antigravity_safety", errorEvent["error"].(map[string]any)["code"])
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
