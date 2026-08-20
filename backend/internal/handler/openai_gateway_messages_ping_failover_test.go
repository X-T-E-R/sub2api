//go:build unit

package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func delayedMessagesSSE(delay time.Duration, payload string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		_, _ = io.WriteString(writer, payload)
		_ = writer.Close()
	}()
	return reader
}

func messagesSSEFailureAfterPingResponse() *http.Response {
	payload := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_retryable\",\"status\":\"failed\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"temporary upstream failure\"},\"output\":[]}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       delayedMessagesSSE(1400*time.Millisecond, payload),
	}
}

func messagesSSESuccessResponse(text string) *http.Response {
	payload := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_recovered","model":"grok-4.5","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":` + strconv.Quote(text) + `}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_recovered","model":"grok-4.5","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
	}, "\n")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
}

func messagesSSESemanticThenFailureResponse() *http.Response {
	payload := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_partial","model":"grok-4.5","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`,
		"",
		`data: {"type":"response.failed","response":{"id":"resp_partial","status":"failed","error":{"type":"rate_limit_error","message":"late failure"},"output":[]}}`,
		"",
	}, "\n")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
}

func TestMessagesHandlerRetriesAfterPingOnlyThenSucceeds(t *testing.T) {
	h, _, upstream, router, cleanup := newGrokCredentialFailoverHandler(t, "first_402")
	defer cleanup()
	upstream.mu.Lock()
	upstream.streamFactory = func(accountID int64) (*http.Response, bool) {
		if accountID == 801 {
			return messagesSSEFailureAfterPingResponse(), true
		}
		if accountID == 802 {
			return messagesSSESuccessResponse("recovered"), true
		}
		return nil, false
	}
	upstream.mu.Unlock()

	// The handler and service share this config pointer.
	// Keepalive is intentionally one second so the controlled first response
	// remains silent long enough to emit exactly transport-only output.
	h.cfg.Gateway.StreamKeepaliveInterval = 1

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/messages", bytes.NewBufferString(`{"model":"grok","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	require.Equal(t, []int64{801, 802}, upstream.accountHits())
	require.Contains(t, recorder.Body.String(), "event: ping")
	require.Contains(t, recorder.Body.String(), "recovered")
}

func TestMessagesHandlerDoesNotRetryAfterSemanticOutput(t *testing.T) {
	_, _, upstream, router, cleanup := newGrokCredentialFailoverHandler(t, "first_402")
	defer cleanup()
	upstream.mu.Lock()
	upstream.streamFactory = func(accountID int64) (*http.Response, bool) {
		if accountID == 801 {
			return messagesSSESemanticThenFailureResponse(), true
		}
		return messagesSSESuccessResponse("must-not-run"), true
	}
	upstream.mu.Unlock()

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/messages", bytes.NewBufferString(`{"model":"grok","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	require.Equal(t, []int64{801}, upstream.accountHits())
	require.Contains(t, recorder.Body.String(), "partial")
	require.NotContains(t, recorder.Body.String(), "must-not-run")
}
