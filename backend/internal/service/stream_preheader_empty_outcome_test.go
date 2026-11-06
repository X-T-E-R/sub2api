package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// These structural fixtures reproduce the observed metadata, not the private
// provider payload: clean EOF, no actual content, no finish and real cache usage.
// C off removes the gateway's extra dispatch, not the failed outcome of an empty
// first generation. Preheader framing must preserve A's existing classification.
func TestStreamPreheaderEmptyOutcomeKeepsAClassification(t *testing.T) {
	const first = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata":{"promptTokenCount":204928,"cachedContentTokenCount":202879,"candidatesTokenCount":0}}}`
	const second = `data: {"response":{"usageMetadata":{"promptTokenCount":204930,"cachedContentTokenCount":202881,"candidatesTokenCount":0}}}`
	for _, continueZero := range []bool{true, false} {
		t.Run("continue_zero="+strconv.FormatBool(continueZero), func(t *testing.T) {
			var generations atomic.Int32
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				generation := generations.Add(1)
				select {
				case <-time.After(80 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				line := first
				if generation == 2 {
					line = second
				}
				_, _ = io.WriteString(w, line+"\n\n")
			}))
			t.Cleanup(upstreamServer.Close)
			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: continueZero}, upstream)
			core, logs := observer.New(zapcore.InfoLevel)
			outcomes := make(chan preheaderForwardOutcome, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r.WithContext(logger.IntoContext(r.Context(), zap.New(core)))
				c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderMessages, interval: 10 * time.Millisecond})
				result, forwardErr := svc.Forward(c.Request.Context(), c, antigravityThoughtAccount(nil), preheaderAGBody("messages", true), false)
				outcomes <- preheaderForwardOutcome{result: result, err: forwardErr, finalized: StreamPreheaderFinalized(c)}
			}))
			t.Cleanup(gateway.Close)
			response, err := (&http.Client{Timeout: 3 * time.Second}).Post(gateway.URL+"/v1/messages", "application/json", nil)
			require.NoError(t, err)
			defer response.Body.Close()
			wire, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			out := awaitPreheaderOutcome(t, outcomes)
			wantCalls, wantInput, wantCache := int32(1), 2049, 202879
			if continueZero {
				wantCalls, wantInput, wantCache = 2, 4098, 405760
			}
			require.EqualValues(t, wantCalls, upstream.calls.Load())
			require.NotNil(t, out.result)
			require.Equal(t, wantInput, out.result.Usage.InputTokens)
			require.Equal(t, wantCache, out.result.Usage.CacheReadInputTokens)
			require.Zero(t, out.result.Usage.OutputTokens)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			t.Logf("wire_fixture status=%d content_type=%s body=%q", response.StatusCode, response.Header.Get("Content-Type"), wire)
			require.GreaterOrEqual(t, strings.Count(string(wire), "event: ping\n"), 2)
			require.NotContains(t, string(wire), "event: message_stop")
			require.NotContains(t, string(wire), `"stop_reason":"end_turn"`)
			var failure *AntigravityNoUsableAnswerError
			require.ErrorAs(t, out.err, &failure)
			require.Equal(t, "antigravity_missing_terminal", failure.Code)
			require.Equal(t, 1, strings.Count(string(wire), "event: error\n"))
			require.Contains(t, string(wire), `"code":"antigravity_missing_terminal"`)
			require.Contains(t, string(wire), `"retryable":false`)
			require.NotContains(t, string(wire), streamPreheaderTerminalCode)
			entries := logs.FilterMessage("antigravity.response_summary").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, "missing", fields["terminal_authority"])
			require.Equal(t, "antigravity_missing_terminal", fields["error_code"])
			require.Equal(t, false, fields["continuation_eligible"], "missing authority never grants thinking-only continuation")
			require.Equal(t, continueZero, fields["zero_output_continuation_used"])
		})
	}
}

func TestStreamPreheaderEmptyOutcomeAcrossProtocols(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		for _, tc := range []struct {
			candidate, code, authority string
			continuationEligible       bool
		}{
			{`{"content":{"parts":[]}}`, "antigravity_missing_terminal", "missing", false},
			{`{"content":{"parts":[]},"finishReason":"STOP"}`, "antigravity_empty_response", "explicit", false},
			{`{"content":{"parts":[{"text":"fixture thought","thought":true}]},"finishReason":"STOP"}`, antigravityThinkingOnlyCode, "explicit", true},
		} {
			t.Run(protocol+"/"+tc.code, func(t *testing.T) {
				line := `data: {"response":{"candidates":[` + tc.candidate + `],"usageMetadata":{"promptTokenCount":8}}}`
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
				body := preheaderAGBody(protocol, true)
				c, rec := newAntigravityCompatContext("POST", "/fixture", body)
				core, logs := observer.New(zapcore.InfoLevel)
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
				ConfigureStreamPreheader(c, cfg, protocol, true)
				_, _ = c.Writer.WriteString(": committed\n\n")
				c.Writer.Flush()
				markStreamPreheaderBodyHeartbeat(c)
				result, err := preheaderAGForward(svc, c, protocol, true, body)
				var failure *AntigravityNoUsableAnswerError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, tc.code, failure.Code)
				require.Equal(t, 1, upstream.callCount)
				require.NotNil(t, result)
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Equal(t, 1, strings.Count(rec.Body.String(), tc.code))
				for _, success := range []string{"event: message_stop", "[DONE]", "response.completed", `"finishReason":"STOP"`} {
					require.NotContains(t, rec.Body.String(), success)
				}
				entries := logs.FilterMessage("antigravity.response_summary").All()
				require.Len(t, entries, 1)
				require.Equal(t, tc.authority, entries[0].ContextMap()["terminal_authority"])
				require.Equal(t, tc.continuationEligible, entries[0].ContextMap()["continuation_eligible"])
			})
		}
	}
}

func TestStreamPreheaderEmptyOutcomeDoesNotHideTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, line, authority string
		readErr               error
		wantFailure           bool
	}{
		{"partial_text", `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`, "missing", nil, true},
		{"partial_tool", `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`, "missing", nil, true},
		{"read_error", `data: {"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`, "read_error", errors.New("connection reset by peer"), true},
		{"tool_stop_with_trailing_usage", `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}}` + "\n\n" + `data: {"response":{"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`, "explicit", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := antigravityThoughtStreamResponse(tc.line)
			if tc.readErr != nil {
				resp.Body = &antigravityCompatErrorReader{data: []byte(tc.line + "\n\n"), err: tc.readErr}
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{resp}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
			core, logs := observer.New(zapcore.InfoLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			body, c, rec := newAntigravityThoughtClaudeRequest(true)
			c.Request = c.Request.WithContext(ctx)
			c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderMessages, interval: time.Second})
			_, _ = c.Writer.WriteString(": committed\n\n")
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			result, err := svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
			require.NotNil(t, result)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.Equal(t, 1, upstream.callCount)
			if tc.wantFailure {
				require.Error(t, err)
				require.False(t, IsAntigravityPartialResponseError(err))
				require.True(t, StreamPreheaderFinalized(c))
				require.Equal(t, 1, strings.Count(rec.Body.String(), streamPreheaderTerminalCode))
				require.NotContains(t, rec.Body.String(), "event: message_stop")
			} else {
				require.NoError(t, err)
				require.False(t, StreamPreheaderFinalized(c))
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
			}
			entries := logs.FilterMessage("antigravity.response_summary").All()
			require.Len(t, entries, 1)
			require.Equal(t, tc.authority, entries[0].ContextMap()["terminal_authority"])
		})
	}
}
