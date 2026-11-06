package service

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// API-key accounts default to the native reasoning reader, not passthrough.
// Its opt-in terminal policy is independent of the legacy OAuth drain policy.
func TestStreamPreheaderFollowupAPIKeyPairForward(t *testing.T) {
	const payload = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2},\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}}\n\n"
	for _, enabled := range []bool{true, false} {
		name := "opt_in"
		if !enabled {
			name = "flag_off"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := newAntigravityCompatContext("POST", "/v1/responses", nil)
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: enabled, StreamKeepaliveInterval: 1}}
			ConfigureStreamPreheader(c, cfg, "responses", true)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, _ = c.Writer.WriteString(": committed\n\n")
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			upstream := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"17"}}, Body: io.NopCloser(strings.NewReader(payload))}}
			account := &Account{ID: 9302, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture-token"}}
			require.False(t, account.IsOpenAIPassthroughEnabled())
			result, err := (&OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}).Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.2","input":"fixture question","stream":true}`))
			require.Error(t, err)
			require.Equal(t, 1, upstream.calls)
			wire := rec.Body.String()
			if !enabled {
				require.False(t, StreamPreheaderFinalized(c))
				require.Nil(t, result, "flag-off retains the existing Forward error contract")
				require.Contains(t, wire, "event: error\n")
				require.NotContains(t, wire, streamPreheaderTerminalCode)
				return
			}
			require.True(t, StreamPreheaderFinalized(c))
			require.NotNil(t, result)
			require.Equal(t, 9, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.Equal(t, "resp_failed", result.ResponseID)
			require.Equal(t, 1, strings.Count(wire, streamPreheaderTerminalCode))
			require.Equal(t, 1, strings.Count(wire, "event: response.failed\n"))
			require.NotContains(t, wire, "event: error\n")
			require.NotContains(t, wire, "[DONE]")
			require.Contains(t, wire, `"retry_after":"17"`)
		})
	}
}

// Like the review's two-HTTP counterexample: append headers are fast, while its
// rejection body is withheld. The request writer must remain live until body or cancel.
func TestStreamPreheaderFollowupAppendRejectedBody(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		name := "delayed_body"
		if cancelClient {
			name = "cancel_body"
		}
		t.Run(name, func(t *testing.T) {
			const delay = 150 * time.Millisecond
			const rejection = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","type":"rate_limit_error","message":"append throttled","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17.250s"}]}}`
			var attempts atomic.Int32
			headersAt := make(chan time.Time, 1)
			canceled := make(chan struct{})
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if attempts.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, antigravityThoughtGenerationOneLine+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				w.(http.Flusher).Flush()
				headersAt <- time.Now()
				if cancelClient {
					<-r.Context().Done()
					close(canceled)
					return
				}
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, rejection)
			}))
			t.Cleanup(upstreamServer.Close)
			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
			outcomes := make(chan preheaderForwardOutcome, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderMessages, interval: 10 * time.Millisecond})
				_, _ = c.Writer.WriteString(": committed\n\n")
				c.Writer.Flush()
				markStreamPreheaderBodyHeartbeat(c)
				result, forwardErr := svc.Forward(r.Context(), c, antigravityThoughtAccount(nil), preheaderAGBody("messages", true), false)
				outcomes <- preheaderForwardOutcome{result: result, err: forwardErr, finalized: StreamPreheaderFinalized(c)}
			}))
			t.Cleanup(gateway.Close)
			response, err := (&http.Client{Timeout: 3 * time.Second}).Post(gateway.URL+"/v1/messages", "application/json", nil)
			require.NoError(t, err)
			defer response.Body.Close()
			committed := make([]byte, len(": committed\n\n"))
			_, err = io.ReadFull(response.Body, committed)
			require.NoError(t, err)
			select {
			case <-headersAt:
			case <-time.After(time.Second):
				t.Fatal("append did not flush the rejection headers")
			}
			if cancelClient {
				scanner := bufio.NewScanner(response.Body)
				sawPing := false
				for scanner.Scan() {
					if scanner.Text() == "event: ping" {
						sawPing = true
						break
					}
				}
				require.NoError(t, scanner.Err())
				require.True(t, sawPing, "append rejection body must emit a heartbeat before cancellation")
				require.NoError(t, response.Body.Close())
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("append rejection body did not cancel upstream")
				}
				out := awaitPreheaderOutcome(t, outcomes)
				var terminal *AntigravityThoughtContinuationError
				require.ErrorAs(t, out.err, &terminal)
				require.ErrorIs(t, terminal.Err, context.Canceled)
				require.NotNil(t, out.result)
				require.Equal(t, 8, out.result.Usage.InputTokens)
				require.Equal(t, 5, out.result.Usage.OutputTokens)
			} else {
				wire, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				out := awaitPreheaderOutcome(t, outcomes)
				require.Error(t, out.err)
				require.True(t, out.finalized)
				require.NotNil(t, out.result)
				require.Equal(t, 8, out.result.Usage.InputTokens)
				require.Equal(t, 5, out.result.Usage.OutputTokens)
				require.Contains(t, string(wire), `"status":429`)
				require.Contains(t, string(wire), `"type":"rate_limit_error"`)
				require.Contains(t, string(wire), `"retry_after":"17.250"`)
				require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
				require.NotContains(t, string(wire), "event: message_stop")
				require.GreaterOrEqual(t, strings.Count(string(wire), "event: ping\n"), 2)
			}
			require.EqualValues(t, 2, upstream.calls.Load())
			require.EqualValues(t, 2, attempts.Load())
		})
	}
}

func TestStreamPreheaderFollowupErrorBodyKeepsCallerLimit(t *testing.T) {
	c, _ := newAntigravityCompatContext("POST", "/v1/messages", nil)
	// Appends may use a larger configured error-body limit than initial requests.
	limit := int64(gatewayUpstreamErrorBodyReadLimit + 17)
	reader := strings.NewReader(strings.Repeat("x", int(limit)+1))
	body, err := readStreamPreheaderErrorBody(c, reader, streamPreheaderMessages, time.Second, limit)
	require.NoError(t, err)
	require.Len(t, body, int(limit))
	require.Equal(t, 1, reader.Len())
}
