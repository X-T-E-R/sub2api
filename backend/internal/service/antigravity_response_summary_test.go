package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestAntigravityResponseSummaryIsOnceAndPayloadFree(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	ctx = context.WithValue(ctx, ctxkey.RequestID, "public-gateway-id")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "public-client-id")
	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"private-thinking-sentinel","thought":true,"thoughtSignature":"private-signature-sentinel","functionCall":null}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
	require.True(t, IsAntigravityNoUsableAnswerError(err))
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error"), "one internal retry carrier")
	entries := logs.FilterMessage("antigravity.response_summary").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.EqualValues(t, 1, fields["downstream_thinking_events"])
	require.EqualValues(t, 0, fields["downstream_text_events"])
	require.EqualValues(t, 0, fields["downstream_tool_events"])
	require.Equal(t, true, fields["actual_upstream_thinking"])
	require.Equal(t, true, fields["actual_upstream_content"])
	require.Equal(t, true, fields["effective_a"])
	require.Equal(t, false, fields["effective_b"])
	require.Equal(t, "messages", fields["protocol"])
	require.Equal(t, "STOP", fields["upstream_finish_reason"])
	require.Equal(t, "explicit_error", fields["a_decision"])
	require.Equal(t, "thinking_only", fields["a_reason"])
	require.Equal(t, "antigravity_thinking_only", fields["error_code"])
	require.Equal(t, true, fields["continuation_eligible"])
	require.Equal(t, "public-gateway-id", fields["request_id"])
	require.Equal(t, "public-client-id", fields["client_request_id"])
	for key, value := range fields {
		switch value.(type) {
		case string, bool, int, int64:
		default:
			t.Fatalf("unexpected non-scalar summary field %s=%T", key, value)
		}
	}
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, secret := range []string{"private-thinking-sentinel", "private-signature-sentinel", "prompt", "token", "authorization"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Equal(t, "UNRECOGNIZED", safeAntigravityFinishReason("private-finish-sentinel"))
	t.Logf("payload-free summary: %s", encoded)
	// Simulate another Forward on this same Gin request. Request completion summary
	// is not duplicated even if an outer caller re-enters the service.
	_, err = svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
	require.NoError(t, err)
	require.Len(t, logs.FilterMessage("antigravity.response_summary").All(), 1)
}

func TestAntigravityResponseSummaryRecordsPolicyOffUsableContent(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{}, upstream)
	body, c, _ := newAntigravityThoughtClaudeRequest(true)
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
	require.NoError(t, err)
	entries := logs.FilterMessage("antigravity.response_summary").All()
	require.Len(t, entries, 1)
	require.EqualValues(t, 1, entries[0].ContextMap()["downstream_text_events"])
	require.Equal(t, "policy_off", entries[0].ContextMap()["a_decision"])
	require.Equal(t, "visible_content", entries[0].ContextMap()["a_reason"])
	require.Equal(t, "", entries[0].ContextMap()["error_code"])
	require.Equal(t, false, entries[0].ContextMap()["continuation_eligible"])
}

func TestAntigravityResponseSummaryCountsActualProtocolContent(t *testing.T) {
	for _, tt := range []struct {
		protocol          string
		stream            bool
		wire              string
		think, text, tool int
	}{
		{"messages", true, "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"thinking\"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"signature_delta\",\"signature\":\"private\"}}\n\n", 1, 0, 0},
		{"chat_completions", true, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private\",\"content\":\"answer\",\"tool_calls\":[{\"index\":0}]}}]}\n\n", 1, 1, 1},
		{"responses", true, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n", 0, 1, 0},
		{"gemini", true, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"private\",\"thought\":true},{\"functionCall\":{\"name\":\"echo\"}},{\"inlineData\":{\"data\":\"private\"}}]}}]}\n\n", 1, 1, 1},
		{"responses", false, `{"output":[{"type":"reasoning","encrypted_content":"private"},{"type":"message","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","name":"echo"}]}`, 1, 1, 1},
		{"messages", false, `{"content":[{"type":"thinking","signature":"private"},{"type":"text","text":"answer"},{"type":"tool_use","name":"echo"}]}`, 1, 1, 1},
	} {
		t.Run(tt.protocol, func(t *testing.T) {
			s := antigravityDeliverySummary{Protocol: tt.protocol, Stream: tt.stream}
			s.observe([]byte(tt.wire))
			require.Equal(t, tt.think, s.ThinkingEvents)
			require.Equal(t, tt.text, s.TextEvents)
			require.Equal(t, tt.tool, s.ToolEvents)
		})
	}
}

func TestAntigravityResponseSummarySeparatesEmptyCarrierFromActualThinking(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	line := `data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"","thoughtSignature":"private-signature-only"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"thoughtsTokenCount":3}}}`
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{}, upstream)
	body, c, _ := newAntigravityThoughtClaudeRequest(true)
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
	require.NoError(t, err)
	entries := logs.FilterMessage("antigravity.response_summary").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, false, fields["actual_upstream_thinking"])
	require.Equal(t, false, fields["actual_upstream_content"])
	require.Equal(t, false, fields["zero_output_continuation_used"])
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-signature-only")
}
