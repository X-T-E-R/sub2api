package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// These fabricated upstream shapes reproduced success with only downstream
// thinking on 02df6c7d. They do not identify native16's unrecorded raw response.
func TestAntigravityDetectorMatchesDeliveredProtocol(t *testing.T) {
	thinking := `{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}`
	for _, shape := range []struct{ name, part, suffix, finish string }{
		{"control_thinking_STOP", thinking, "", "STOP"},
		{"null_functionCall", `{"text":"plan","thought":true,"thoughtSignature":"sig-plan","functionCall":null}`, "", "STOP"},
		{"nonWeb_grounding", thinking, `,"groundingMetadata":{"groundingChunks":[{"retrievedContext":{"uri":"fixture://document","title":"document"}}]}`, "STOP"},
		{"unspecified_terminal", thinking, "", "FINISH_REASON_UNSPECIFIED"},
	} {
		for _, protocol := range []string{"messages", "chat_completions", "responses", "gemini"} {
			t.Run(shape.name+"/"+protocol, func(t *testing.T) {
				line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` + shape.part + `]},"finishReason":"` + shape.finish + `"` + shape.suffix + `}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
				body := []byte(`{"model":"client-alias","stream":true,"stream_options":{"include_usage":true},"max_tokens":64,"messages":[{"role":"user","content":"question"}],"input":"question","contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
				account := antigravityThoughtAccount(nil)
				var result *ForwardResult
				var err error
				switch protocol {
				case "messages":
					result, err = svc.Forward(context.Background(), c, account, body, false)
				case "chat_completions":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				case "responses":
					result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				case "gemini":
					result, err = svc.ForwardGemini(context.Background(), c, account, "gemini-3.1-pro-high", "streamGenerateContent", true, body, false)
				}
				wire := recorder.Body.String()
				require.NotNil(t, result)
				require.Equal(t, 1, upstream.callCount)
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				if protocol == "gemini" && shape.name == "nonWeb_grounding" {
					require.NoError(t, err, "native Gemini receives consumable non-Web metadata verbatim")
					require.Contains(t, wire, "retrievedContext")
					require.Contains(t, wire, `"finishReason":"STOP"`)
					fields := antigravityThoughtContinuationFromGin(c).summaryFields(c)
					require.Equal(t, "grounding", fields["a_reason"])
					require.Equal(t, true, fields["detector_grounding"])
					require.Equal(t, false, fields["detector_rendered_grounding"])
				} else {
					require.True(t, IsAntigravityNoUsableAnswerError(err), "err=%v", err)
					for _, success := range []string{`"stop_reason":"end_turn"`, `"type":"message_stop"`, `"finish_reason":"stop"`, "data: [DONE]", "response.completed", `"finishReason":"STOP"`} {
						require.NotContains(t, wire, success)
					}
					if shape.name == "unspecified_terminal" {
						require.Equal(t, "missing_terminal", SnapshotOpsUpstreamErrors(c)[0].Reason)
					}
				}
			})
		}
	}
}

func TestAntigravityRenderableGroundingAndContentRemainUsable(t *testing.T) {
	for _, shape := range []struct{ name, part, suffix, finish, want string }{
		{"web_grounding", `{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}`, `,"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://example.com","title":"Source"}}]}`, "STOP", "text_delta"},
		{"visible_text", `{"text":"answer"}`, "", "STOP", "text_delta"},
		{"real_tool", `{"functionCall":{"name":"echo","args":{}}}`, "", "STOP", "tool_use"},
		{"output_limit", `{"text":"plan","thought":true}`, "", "MAX_TOKENS", "max_tokens"},
		{"safety", `{"text":"plan","thought":true}`, "", "SAFETY", "end_turn"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` + shape.part + `]},"finishReason":"` + shape.finish + `"` + shape.suffix + `}]}}`
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(true)
			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			if shape.finish == "SAFETY" {
				// A refusal is a distinct non-retryable terminal, not a normal end_turn.
				require.True(t, IsAntigravityNoUsableAnswerError(err))
				require.Equal(t, "antigravity_safety", antigravityThoughtContinuationFromGin(c).responseFailure().Code)
				require.NotContains(t, recorder.Body.String(), `"stop_reason":"end_turn"`)
				require.Contains(t, recorder.Body.String(), `"code":"antigravity_safety"`)
				return
			}
			require.NoError(t, err)
			require.Contains(t, recorder.Body.String(), shape.want)
			require.NotContains(t, recorder.Body.String(), "event: error")
		})
	}
}
