package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Structural fixtures, not reconstructed provider payloads. Each case names the
// upstream shape, the stable client code and whether produced content survives.
func TestAntigravityTerminalOutcomeForward(t *testing.T) {
	for _, tc := range []struct{ name, candidate, code string }{
		{"thinking_stop", `{"content":{"parts":[{"thought":true,"text":"plan","thoughtSignature":"sig-plan"}]},"finishReason":"STOP"}`, "antigravity_thinking_only"},
		{"empty_stop", `{"content":{"parts":[]},"finishReason":"STOP"}`, "antigravity_empty_response"},
		{"signature_missing_terminal", `{"content":{"parts":[{"thought":true,"thoughtSignature":"sig-only"}]}}`, "antigravity_missing_terminal"},
		{"empty_missing_terminal", `{"content":{"parts":[]}}`, "antigravity_missing_terminal"},
		{"partial_malformed", `{"content":{"parts":[{"text":"partial answer"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}`, "antigravity_malformed_function_call"},
		{"malformed_response", `{"finishReason":"MALFORMED_RESPONSE"}`, "antigravity_malformed_response"},
		{"unknown_terminal", `{"finishReason":"SOME_FUTURE_FAILURE"}`, "antigravity_upstream_terminal_error"},
		{"prompt_block_without_candidate", ``, "antigravity_safety"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The adapter encodes the decision; it does not make it. Both the
			// streaming and the collected path of every protocol must agree.
			for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
				for _, stream := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%t", protocol, stream), func(t *testing.T) {
						payload := `{"candidates":[` + tc.candidate + `],"usageMetadata":{"promptTokenCount":8,"thoughtsTokenCount":2,"candidatesTokenCount":3}}`
						if tc.candidate == "" {
							payload = `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":8,"thoughtsTokenCount":2,"candidatesTokenCount":3}}`
						}
						// The trailing frame only carries usage: a terminal must not be
						// synthesized from that frame, and its usage must not be dropped.
						line := `data: {"response":` + payload + "}\n\n" + `data: {"response":{"usageMetadata":{"promptTokenCount":9,"thoughtsTokenCount":2,"candidatesTokenCount":4}}}`
						upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
						svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
						body := []byte(`{"model":"client-alias","stream":` + fmt.Sprint(stream) + `,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"question"}],"input":"question","contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
						c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
						account := antigravityThoughtAccount(nil)
						var result *ForwardResult
						var err error
						switch protocol {
						case "messages":
							result, err = svc.Forward(context.Background(), c, account, body, false)
						case "chat":
							result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
						case "responses":
							result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
						case "gemini":
							action := "generateContent"
							if stream {
								action = "streamGenerateContent"
							}
							result, err = svc.ForwardGemini(context.Background(), c, account, "gemini-3.1-pro-high", action, stream, body, false)
						}
						require.Error(t, err)
						require.True(t, IsAntigravityPartialResponseError(err), "err=%v", err)
						require.NotNil(t, result)
						require.Equal(t, 9, result.Usage.InputTokens)
						require.Equal(t, 6, result.Usage.OutputTokens)
						require.Equal(t, 1, upstream.callCount, "no failover, no gateway Continue")
						wire := recorder.Body.String()
						require.Contains(t, wire, tc.code)
						for _, success := range []string{`"stop_reason":"end_turn"`, `"type":"message_stop"`, `"finish_reason":"stop"`, "response.completed", "data: [DONE]"} {
							require.NotContains(t, wire, success)
						}
						if tc.name == "partial_malformed" {
							require.Contains(t, wire, "partial answer", "content already produced must survive the failure")
						}
						if !stream {
							if tc.code == "antigravity_safety" {
								require.Equal(t, http.StatusForbidden, recorder.Code, "a refusal is not a generic retryable 502")
							} else {
								require.Equal(t, http.StatusBadGateway, recorder.Code)
							}
						}
						if protocol == "chat" {
							require.Equal(t, gjson.Null, gjson.Get(wire, "choices.0.finish_reason").Type)
						}
					})
				}
			}
		})
	}
}

func TestAntigravityNoPayloadKeepsEmptyStreamRecovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		wantA bool
	}{
		{name: "no upstream data"},
		{name: "SSE controls only", lines: []string{": keepalive", "data: [DONE]"}},
		{name: "non-object data", lines: []string{"data: null", "data: []"}},
		{name: "candidate payload without finish", lines: []string{`data: {"response":{"candidates":[{"content":{"parts":[]}}]}}`}, wantA: true},
		{name: "usage-only payload", lines: []string{`data: {"response":{"usageMetadata":{"promptTokenCount":8}}}`}, wantA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			generation := newAntigravityThoughtGeneration()
			for _, line := range tc.lines {
				generation.observeLine(line)
			}
			require.Equal(t, tc.wantA, generation.noUsableAnswerEligible())
		})
	}
}

// A missing finish reason without text or tool is A's missing-terminal shape;
// delivered content and tool calls keep their existing compatible semantics.
func TestAntigravityMissingTerminalContentCompatibility(t *testing.T) {
	for _, part := range []string{`{"text":"normal answer"}`, `{"functionCall":{"name":"lookup","args":{}}}`} {
		line := `data: {"response":{"candidates":[{"content":{"parts":[` + part + `]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
		svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
		body, c, _ := newAntigravityThoughtClaudeRequest(true)
		result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 1, upstream.callCount)
	}
}
