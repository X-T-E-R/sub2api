package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Null stop_reason plus clean EOF is not an error contract: an intermediary may
// synthesize end_turn. A must send an explicit retryable Anthropic error instead.
func TestAntigravityThoughtSuppressionSignalsRetryAndAcceptsClientRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)
	account := antigravityThoughtAccount(nil)
	result, err := svc.Forward(context.Background(), c, account, body, false)

	require.Error(t, err, "A must return a billable typed failure, not a successful EOF")
	require.True(t, IsAntigravityNoUsableAnswerError(err))
	require.True(t, IsAntigravityPartialResponseError(err))
	require.False(t, IsAntigravityThoughtContinuationError(err), "A is not a failed B append")
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, 1, upstream.callCount, "B is off: no gateway append or failover")
	wire := recorder.Body.String()
	require.Contains(t, wire, `"thinking":"plan"`)
	require.Contains(t, wire, "sig-plan")
	require.Contains(t, wire, `"stop_reason":null`)
	require.Contains(t, wire, `"output_tokens":5`)
	require.Contains(t, wire, "event: error")
	require.Contains(t, wire, `"type":"api_error"`, "normal Anthropic consumers can retry this error")
	require.NotContains(t, wire, `"stop_reason":"end_turn"`)
	require.NotContains(t, wire, `"type":"message_stop"`)

	// The existing B-off recovery converter handles a subsequent client request
	// that retained the failed thinking. No new continuation prompt/user turn is
	// introduced, and no unsupported terminal model turn reaches Antigravity.
	retryBody := []byte(`{"model":"client-alias","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig-plan"}]}]}`)
	retryContext, retryRecorder := newAntigravityCompatContext("POST", "/v1/messages", retryBody)
	retryResult, retryErr := svc.Forward(context.Background(), retryContext, account, retryBody, false)
	require.NoError(t, retryErr)
	require.NotNil(t, retryResult)
	require.Equal(t, 2, upstream.callCount, "one upstream request for each client attempt")
	contents := gjson.GetBytes(upstream.requestBodies[1], "request.contents").Array()
	require.Len(t, contents, 1)
	require.Equal(t, "user", contents[0].Get("role").String())
	require.Equal(t, "question", contents[0].Get("parts.0.text").String())
	require.NotContains(t, string(upstream.requestBodies[1]), "[sub2api:")
	require.Contains(t, retryRecorder.Body.String(), `"text":"answer"`)
	require.Contains(t, retryRecorder.Body.String(), `"stop_reason":"end_turn"`)
}

func TestAntigravityThoughtSuppressionDoesNotReclassifyReadFailure(t *testing.T) {
	for _, readErr := range []error{context.Canceled, errors.New("connection reset by peer")} {
		t.Run(readErr.Error(), func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &antigravityCompatErrorReader{data: []byte(antigravityThoughtGenerationOneLine), err: readErr},
			}}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(true)
			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			require.False(t, IsAntigravityNoUsableAnswerError(err), "only clean EOF qualifies for A")
			require.Equal(t, 1, upstream.callCount)
			require.NotContains(t, recorder.Body.String(), `"type":"api_error"`)
			require.NotContains(t, recorder.Body.String(), `"type":"message_stop"`)
		})
	}
}

const antigravityEmptyStopLine = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":0,"thoughtsTokenCount":0}}}`

func TestAntigravityEmptyStopSignalsRetryOnClaudeForward(t *testing.T) {
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityEmptyStopLine)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)
	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
	require.True(t, IsAntigravityNoUsableAnswerError(err), "empty normal STOP must not be successful EOF")
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
	require.Equal(t, 1, upstream.callCount)
	wire := recorder.Body.String()
	require.Contains(t, wire, `"type":"api_error"`)
	require.Contains(t, wire, `"input_tokens":8`)
	require.Contains(t, wire, `"output_tokens":0`)
	require.NotContains(t, wire, `"stop_reason":"end_turn"`, "success cannot precede the error")
	require.NotContains(t, wire, `"type":"message_stop"`)
	events := SnapshotOpsUpstreamErrors(c)
	require.Len(t, events, 1)
	require.Equal(t, "empty", events[0].Reason)
}

func TestAntigravityEmptyStopProtocolsPreserveUsageWithoutSuccessfulTerminal(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
			name := protocol + "/collect"
			if stream {
				name = protocol + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityEmptyStopLine)}}
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
				require.True(t, IsAntigravityNoUsableAnswerError(err), "err=%v", err)
				require.NotNil(t, result)
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Zero(t, result.Usage.OutputTokens)
				require.Equal(t, 1, upstream.callCount)
				if !stream {
					require.Equal(t, http.StatusBadGateway, recorder.Code)
				}
				wire := recorder.Body.String()
				require.Contains(t, wire, antigravityNoUsableAnswerMessage)
				for _, success := range []string{`"stop_reason":"end_turn"`, `"type":"message_stop"`, `"finish_reason":"stop"`, "data: [DONE]", "response.completed", `"finishReason":"STOP"`} {
					require.NotContains(t, wire, success)
				}
				switch protocol {
				case "messages":
					require.Contains(t, wire, `"input_tokens":8`)
				case "responses":
					require.Contains(t, wire, `"input_tokens":8`)
					if !stream {
						require.Equal(t, "failed", gjson.Get(wire, "status").String(), "root response terminal, not the closed output item's status")
					}
				case "chat":
					require.Contains(t, wire, `"prompt_tokens":8`)
				case "gemini":
					require.Contains(t, wire, `"promptTokenCount":8`)
				}
			})
		}
	}
}

func TestAntigravityThinkingMissingTerminalSignalsRetryOnClaudeForward(t *testing.T) {
	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)
	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
	require.True(t, IsAntigravityNoUsableAnswerError(err), "missing terminal after actual thinking must not synthesize success")
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, 1, upstream.callCount)
	wire := recorder.Body.String()
	require.Contains(t, wire, "sig-plan")
	require.Contains(t, wire, `"type":"api_error"`)
	require.NotContains(t, wire, `"stop_reason":"end_turn"`)
	require.NotContains(t, wire, `"type":"message_stop"`)
	events := SnapshotOpsUpstreamErrors(c)
	require.Len(t, events, 1)
	require.Equal(t, "missing_terminal", events[0].Reason)
}

func TestAntigravityEmptyOrMissingTerminalNeverAppendsThinkingContinuation(t *testing.T) {
	missing := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}]}}]}}`
	for _, line := range []string{antigravityEmptyStopLine, missing} {
		for _, suppress := range []bool{false, true} {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: suppress, ContinueThoughtOnly: true}, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(true)
			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			if suppress {
				require.True(t, IsAntigravityNoUsableAnswerError(err))
			} else {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), `"type":"message_stop"`)
			}
			require.Equal(t, 1, upstream.callCount, "B still requires actual thinking with STOP")
		}
	}
}
