package service

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStreamPreheaderAGReadLimitsKeepObservedUsage(t *testing.T) {
	for _, limit := range []string{"line_too_long", "idle_timeout"} {
		t.Run(limit, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			server, upstream, outcomes := preheaderAGServer(t, "messages", true, func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n")
				w.(http.Flusher).Flush()
				if limit == "line_too_long" {
					_, _ = io.WriteString(w, "data: "+strings.Repeat("x", 128<<10)+"\n\n")
					return
				}
				<-r.Context().Done()
			}, func(cfg *config.Config) {
				cfg.Gateway.MaxLineSize = 64 << 10
				cfg.Gateway.StreamDataIntervalTimeout = 1
			})
			response, err := (&http.Client{Timeout: 6 * time.Second}).Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("messages", true)))
			require.NoError(t, err)
			defer response.Body.Close()
			readPreheaderPing(t, response, "messages")
			unblock()
			wire, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			out := awaitPreheaderOutcome(t, outcomes)
			require.Error(t, out.err)
			if limit == "line_too_long" {
				require.ErrorIs(t, out.err, bufio.ErrTooLong)
			} else {
				require.ErrorContains(t, out.err, "stream data interval timeout")
			}
			require.True(t, out.finalized)
			require.NotNil(t, out.result)
			require.Equal(t, 8, out.result.Usage.InputTokens)
			require.Equal(t, 3, out.result.Usage.OutputTokens)
			require.EqualValues(t, 1, upstream.calls.Load())
			require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
			require.NotContains(t, string(wire), "event: message_stop")
		})
	}
}

func TestStreamPreheaderZeroOutputCanStillContinue(t *testing.T) {
	const empty = `data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"","thoughtSignature":"signature-only"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(empty), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
			body := preheaderAGBody(protocol, true)
			c, rec := newAntigravityCompatContext("POST", "/fixture", body)
			ConfigureStreamPreheader(c, &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}, protocol, true)
			_, _ = c.Writer.WriteString(": committed\n\n")
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			result, err := preheaderAGForward(svc, c, protocol, true, body)
			require.NoError(t, err)
			require.False(t, StreamPreheaderFinalized(c))
			require.Equal(t, 2, upstream.callCount)
			require.Contains(t, string(upstream.requestBodies[1]), `"text":"Continue."`)
			require.NotContains(t, string(upstream.requestBodies[1]), "signature-only")
			require.Equal(t, 28, result.Usage.InputTokens)
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.Contains(t, rec.Body.String(), "answer")
		})
	}
}

func TestStreamPreheaderCodexPairedErrorDrainsAuthoritativeUsage(t *testing.T) {
	for _, pipeline := range []string{"native", "passthrough"} {
		for _, end := range []string{"named_error_then_failed", "untyped_error_then_failed", "untyped_error_then_completed", "untyped_error_then_eof"} {
			t.Run(pipeline+"/"+end, func(t *testing.T) {
				body := "data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n"
				if end == "named_error_then_failed" {
					body = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n"
				}
				if strings.HasSuffix(end, "_failed") {
					body += "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2},\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}}\n\ndata: [DONE]\n\n"
				}
				completed := strings.HasSuffix(end, "_completed")
				if completed {
					body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_recovered\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":2}}}\n\n"
				}
				c, rec := newAntigravityCompatContext("POST", "/v1/responses", nil)
				cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
				ConfigureStreamPreheader(c, cfg, "responses", true)
				_, _ = c.Writer.WriteString(": committed\n\n")
				c.Writer.Flush()
				markStreamPreheaderBodyHeartbeat(c)
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"17"}}, Body: io.NopCloser(strings.NewReader(body))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				svc := &OpenAIGatewayService{cfg: cfg}
				var usage *OpenAIUsage
				var streamErr error
				if pipeline == "passthrough" {
					result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.2", "gpt-5.2")
					require.NotNil(t, result)
					usage, streamErr = result.usage, err
				} else {
					result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.2", "gpt-5.2")
					require.NotNil(t, result)
					usage, streamErr = result.usage, err
				}
				wire := rec.Body.String()
				if completed {
					require.NoError(t, streamErr)
					require.False(t, StreamPreheaderFinalized(c))
					require.Equal(t, 9, usage.InputTokens)
					require.Equal(t, 2, usage.OutputTokens)
					require.Contains(t, wire, `"type":"response.completed"`)
					require.NotContains(t, wire, `"error"`)
					return
				}
				require.Error(t, streamErr)
				require.True(t, StreamPreheaderFinalized(c))
				require.Equal(t, 1, strings.Count(wire, streamPreheaderTerminalCode))
				require.Equal(t, 1, strings.Count(wire, "event: response.failed\n"))
				require.NotContains(t, wire, "event: error\n")
				require.NotContains(t, wire, "[DONE]")
				require.Contains(t, wire, `"retry_after":"17"`)
				require.Contains(t, wire, `"type":"rate_limit_error"`)
				if strings.HasSuffix(end, "_failed") {
					require.Equal(t, 9, usage.InputTokens)
					require.Equal(t, 2, usage.OutputTokens)
					require.Contains(t, wire, `"id":"resp_failed"`)
				} else {
					require.Equal(t, OpenAIUsage{}, *usage)
				}
			})
		}
	}
}

func TestStreamPreheaderNativeAnthropicChatKeepsDone(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_review\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"review\",\"usage\":{\"input_tokens\":8}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	c, rec := newAntigravityCompatContext("POST", "/v1/chat/completions", nil)
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
	ConfigureStreamPreheader(c, cfg, "chat", true)
	_, _ = c.Writer.WriteString(": committed\n\n")
	c.Writer.Flush()
	markStreamPreheaderBodyHeartbeat(c)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	result, err := (&OpenAIGatewayService{cfg: cfg}).handleCCStreamingFromNativeAnthropic(resp, c, "review", "review", "review", nil, time.Now(), true)
	require.NoError(t, err)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "data: [DONE]\n\n"))
	require.NotContains(t, rec.Body.String(), streamPreheaderTerminalCode)
}

func TestStreamPreheaderAppendLateRateLimitPreservesStatusAndRetryInfo(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			const rejection = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","type":"rate_limit_error","message":"append throttled","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17.250s"}]}}`
			late := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(rejection))}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine), late}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
			body := preheaderAGBody(protocol, true)
			c, rec := newAntigravityCompatContext("POST", "/fixture", body)
			ConfigureStreamPreheader(c, &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}, protocol, true)
			_, _ = c.Writer.WriteString(": committed\n\n")
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			result, err := preheaderAGForward(svc, c, protocol, true, body)
			require.Error(t, err)
			var appendErr *AntigravityThoughtContinuationError
			require.ErrorAs(t, err, &appendErr)
			require.Equal(t, 429, appendErr.StatusCode)
			require.True(t, StreamPreheaderFinalized(c))
			require.Equal(t, 2, upstream.callCount)
			require.NotNil(t, result)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, 5, result.Usage.OutputTokens)
			wire := rec.Body.String()
			if protocol == "gemini" {
				require.Contains(t, wire, `"code":429`)
				require.Contains(t, wire, `"status":"RESOURCE_EXHAUSTED"`)
				require.Contains(t, wire, `"retryDelay":"17.250s"`)
				require.Equal(t, 1, strings.Count(wire, "GATEWAY_STREAM_TERMINAL_ERROR"))
			} else {
				require.Contains(t, wire, `"type":"rate_limit_error"`)
				require.Contains(t, wire, `"status":429`)
				require.Contains(t, wire, `"retry_after":"17.250"`)
				require.Equal(t, 1, strings.Count(wire, streamPreheaderTerminalCode))
			}
			require.NotContains(t, wire, "[DONE]")
			require.NotContains(t, wire, "event: message_stop")
			require.NotContains(t, wire, "response.completed")
			require.NotContains(t, wire, "response.incomplete")
		})
	}
}
