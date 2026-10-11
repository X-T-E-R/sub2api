package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The rejection text is observed in production account 69's ordinary 400s
// (ops rows 31909/31914/31916/31917). Row 31913 proves a continuation 400,
// but its original body was lost: this fixture does not claim the rows share a
// request, or that the real endpoint can accept a thinking-only model tail.
func TestAntigravityThoughtContinuation400PreservesBoundedDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const message = "Requests ending with a model turn are not supported."
	const providerID = "upstream-rejected-append"
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		{
			StatusCode: http.StatusBadRequest,
			Header: http.Header{
				"X-Request-Id":  []string{providerID},
				"Authorization": []string{"Bearer do-not-record"},
			},
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"` + message + `","details":[{"thoughtSignature":"do-not-record-signature","contents":[{"text":"do-not-record-prompt"}]}]}}`)),
		},
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)
	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
	require.Error(t, err)
	require.True(t, IsAntigravityThoughtContinuationError(err))
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, 2, upstream.callCount, "no retry, signature rectifier or third generation")

	var event *OpsUpstreamErrorEvent
	for _, candidate := range SnapshotOpsUpstreamErrors(c) {
		if candidate != nil && candidate.Kind == "thought_continuation_error" {
			require.Nil(t, event, "exactly one continuation failure")
			event = candidate
		}
	}
	require.NotNil(t, event)
	require.Equal(t, http.StatusBadRequest, event.UpstreamStatusCode)
	require.Equal(t, message, event.Message)
	require.Equal(t, "INVALID_ARGUMENT", event.Reason)
	require.Equal(t, "claude_stream", event.Stage)
	require.Equal(t, providerID, event.UpstreamRequestID)
	require.Empty(t, event.Detail)
	require.Empty(t, event.UpstreamResponseBody)
	require.ErrorContains(t, err, message, "returned diagnostic must not lose the actual rejection")
	for _, secret := range []string{"do-not-record", "thoughtSignature", "contents"} {
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, event.Message+event.Detail+event.Reason+event.UpstreamRequestID, secret)
	}
	require.NotContains(t, recorder.Body.String(), message, "do not replace the protocol's safe generic error")
	require.NotContains(t, recorder.Body.String(), `"stop_reason":"end_turn"`)
	require.Contains(t, recorder.Body.String(), `"stop_reason":null`)
}

func TestAntigravityThoughtContinuationDiagnosticsExcludePayloadAndBoundMetadata(t *testing.T) {
	t.Run("nested provider message keeps only the diagnostic leaf", func(t *testing.T) {
		err := newAntigravityThoughtContinuationStatusError(400, nil, []byte(`{"error":{"code":400,"message":"{\"error\":{\"message\":\"Corrupted thought signature.\",\"thoughtSignature\":\"private-signature\"},\"request\":{\"contents\":[{\"text\":\"private-prompt\"}]}}"}}`))
		require.Equal(t, "Corrupted thought signature.", err.Message)
		require.Equal(t, "400", err.Code)
		require.NotContains(t, err.Error(), "private-")
	})
	t.Run("structured non-message and invalid code are not persisted", func(t *testing.T) {
		err := newAntigravityThoughtContinuationStatusError(400, nil, []byte(`{"error":{"message":"{\"thoughtSignature\":\"private-signature\",\"contents\":[{\"text\":\"private-prompt\"}]}","status":{"token":"private-token"}}}`))
		require.Empty(t, err.Message)
		require.Empty(t, err.Code)
		require.NotContains(t, err.Error(), "private-")
	})
	t.Run("leaf uses existing sanitizer and finite limits", func(t *testing.T) {
		message := "https://example.test/?access_token=private-token&key=private-key " + strings.Repeat("x", 800)
		body, marshalErr := json.Marshal(map[string]any{"error": map[string]any{"message": message, "status": strings.Repeat("A", 65)}})
		require.NoError(t, marshalErr)
		err := newAntigravityThoughtContinuationStatusError(400, http.Header{"X-Request-Id": []string{strings.Repeat("i", 180)}}, body)
		require.LessOrEqual(t, len(err.Message), 512)
		require.LessOrEqual(t, len(err.RequestID), 128)
		require.Empty(t, err.Code)
		require.Contains(t, err.Message, "access_token=***&key=***")
		require.NotContains(t, err.Error(), "private-")
	})
}
