package service

import (
	"context"
	"errors"
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
