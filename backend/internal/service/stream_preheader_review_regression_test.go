package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStreamPreheaderAGTruncatedBodyRetainsUsage(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			server, upstream, outcomes := preheaderAGServer(t, protocol, true, func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n")
			}, nil)
			response, err := (&http.Client{Timeout: 5 * time.Second}).Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody(protocol, true)))
			require.NoError(t, err)
			defer response.Body.Close()
			readPreheaderPing(t, response, protocol)
			unblock()
			wire, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			out := awaitPreheaderOutcome(t, outcomes)
			require.Error(t, out.err)
			require.True(t, out.finalized)
			require.NotNil(t, out.result)
			require.Equal(t, 8, out.result.Usage.InputTokens)
			require.Equal(t, 3, out.result.Usage.OutputTokens)
			require.EqualValues(t, 1, upstream.calls.Load())
			if protocol == "gemini" {
				require.Equal(t, 1, strings.Count(string(wire), "GATEWAY_STREAM_TERMINAL_ERROR"))
			} else {
				require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
			}
			require.NotContains(t, string(wire), "message_stop")
			require.NotContains(t, string(wire), "response.completed")
			require.NotContains(t, string(wire), "[DONE]")
		})
	}
}

func TestStreamPreheaderAGSmartRetryWaitsForSecondHead(t *testing.T) {
	var attempts atomic.Int32
	server, upstream, outcomes := preheaderAGServer(t, "messages", true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"model":"gemini-4-argon"},"reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`)
			return
		}
		<-r.Context().Done()
	}, nil)
	response, err := (&http.Client{Timeout: 3500 * time.Millisecond}).Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("messages", true)))
	require.NoError(t, err)
	readPreheaderPing(t, response, "messages")
	require.NoError(t, response.Body.Close())
	out := awaitPreheaderOutcome(t, outcomes)
	require.Error(t, out.err)
	require.EqualValues(t, 2, upstream.calls.Load())
}

func TestStreamPreheaderBodyRetryInfoWithoutHTTPHint(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server, upstream, outcomes := preheaderAGServer(t, "gemini", true, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"slow down","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17.250s"}]}}`)
	}, nil)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("gemini", true)))
	require.NoError(t, err)
	defer response.Body.Close()
	readPreheaderPing(t, response, "gemini")
	unblock()
	wire, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(wire), `"retryDelay":"17.250s"`)
	require.Contains(t, string(wire), `"code":429`)
	require.Error(t, awaitPreheaderOutcome(t, outcomes).err)
	require.EqualValues(t, 1, upstream.calls.Load())
}

type preheaderRedactionUpstream struct{}

const preheaderFakeCredential = "review-fixture-credential-NOT-REAL"

func (preheaderRedactionUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	select {
	case <-time.After(30 * time.Millisecond):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"fixture echoed ` + preheaderFakeCredential + `"}}`))}, nil
}
func (u preheaderRedactionUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}
func TestStreamPreheaderLateAgentIdentityRedactsWireAndOps(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderResponses, interval: 5 * time.Millisecond})
	account := &Account{ID: 9302, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity, "access_token": preheaderFakeCredential}}
	svc := &OpenAIGatewayService{httpUpstream: preheaderRedactionUpstream{}}
	_, err, terminal := svc.doOpenAIStreamUpstream(c, httptest.NewRequest("POST", "http://fixture.invalid/v1/responses", nil), "", account)
	require.Error(t, err)
	require.True(t, terminal)
	require.NotContains(t, rec.Body.String(), preheaderFakeCredential)
	require.Contains(t, rec.Body.String(), "[redacted]")
	ops, err := json.Marshal(c.Keys)
	require.NoError(t, err)
	require.NotContains(t, string(ops), preheaderFakeCredential)
}

func TestStreamPreheaderAGReadFailureKeepsUsage(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			c, rec := newAntigravityCompatContext("POST", "/v1/messages", nil)
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1, MaxLineSize: defaultMaxLineSize}}
			ConfigureStreamPreheader(c, cfg, protocol, true)
			policy, _ := c.Get(streamPreheaderPolicyKey)
			_, _ = c.Writer.WriteString(streamPreheaderPing(policy.(streamPreheaderPolicy).protocol))
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			upstream := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: 200, Header: http.Header{}, Body: &antigravityCompatErrorReader{data: []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n"), err: io.ErrUnexpectedEOF}}}
			result, err := preheaderAGForward(newAntigravityCompatService(cfg.Gateway, upstream), c, protocol, true, preheaderAGBody(protocol, true))
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.True(t, StreamPreheaderFinalized(c))
			if protocol == "gemini" {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "GATEWAY_STREAM_TERMINAL_ERROR"))
			} else {
				require.Equal(t, 1, strings.Count(rec.Body.String(), streamPreheaderTerminalCode))
			}
		})
	}
}

func TestStreamPreheaderGeminiSSEErrorGetsTerminalMarker(t *testing.T) {
	for _, protocol := range []string{"gemini", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			c, rec := newAntigravityCompatContext("POST", "/v1beta/models/test:streamGenerateContent", nil)
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			ConfigureStreamPreheader(c, cfg, protocol, true)
			_, _ = c.Writer.WriteString(": gateway heartbeat\n\n")
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}\n\ndata: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"message\":\"rate limited\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.RetryInfo\",\"retryDelay\":\"17.250s\"}]}}\n\n"))}
			svc := &GeminiMessagesCompatService{cfg: cfg}
			if protocol == "gemini" {
				result, err := svc.handleNativeStreamingResponse(c, resp, time.Now(), false)
				require.Error(t, err)
				require.Equal(t, 8, result.usage.InputTokens)
				require.Equal(t, 3, result.usage.OutputTokens)
				require.Contains(t, rec.Body.String(), `"code":429`)
				require.Contains(t, rec.Body.String(), `"status":"RESOURCE_EXHAUSTED"`)
				require.Contains(t, rec.Body.String(), `"retryDelay":"17.250s"`)
				require.Equal(t, 1, strings.Count(rec.Body.String(), "GATEWAY_STREAM_TERMINAL_ERROR"))
			} else {
				result, err := svc.handleChatCompletionsStreamingResponseFromGemini(c, resp, time.Now(), "gemini-test", false, true)
				require.Error(t, err)
				require.Equal(t, 8, result.usage.InputTokens)
				require.Equal(t, 3, result.usage.OutputTokens)
				require.Contains(t, rec.Body.String(), `"retry_after":"17.250"`)
				require.Equal(t, 1, strings.Count(rec.Body.String(), streamPreheaderTerminalCode))
				require.NotContains(t, rec.Body.String(), "[DONE]")
			}
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"error":`))
			require.True(t, StreamPreheaderFinalized(c))
		})
	}
}

func TestStreamPreheaderGeminiChatWaitAndTruncation(t *testing.T) {
	for _, phase := range []string{"headers", "first_token"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "first_token" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}\n\n")
			}))
			t.Cleanup(upstreamServer.Close)
			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			finished := make(chan preheaderForwardOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				ConfigureStreamPreheader(c, cfg, "chat", true)
				account := &Account{ID: 102, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}, Concurrency: 1}
				result, err := (&GeminiMessagesCompatService{cfg: cfg, httpUpstream: upstream}).ForwardAsChatCompletions(r.Context(), c, account, []byte(`{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
				finished <- preheaderForwardOutcome{result, err, StreamPreheaderFinalized(c)}
			}))
			t.Cleanup(server.Close)
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(server.URL)
			require.NoError(t, err)
			defer resp.Body.Close()
			readPreheaderPing(t, resp, "chat")
			unblock()
			wire, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			out := awaitPreheaderOutcome(t, finished)
			require.Error(t, out.err)
			require.True(t, out.finalized)
			require.NotNil(t, out.result)
			require.Equal(t, 8, out.result.Usage.InputTokens)
			require.Equal(t, 3, out.result.Usage.OutputTokens)
			require.EqualValues(t, 1, upstream.calls.Load())
			require.Contains(t, string(wire), "partial")
			require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
			require.NotContains(t, string(wire), "[DONE]")
		})
	}
}

func TestStreamPreheaderLegalContinuationKeepsHeadWaitAlive(t *testing.T) {
	var attempts atomic.Int32
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			_, _ = io.WriteString(w, antigravityThoughtGenerationOneLine+"\n\n")
			return
		}
		select {
		case <-time.After(45 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, antigravityThoughtGenerationTwoLine+"\n\n")
	}))
	t.Cleanup(upstreamServer.Close)
	base, err := url.Parse(upstreamServer.URL)
	require.NoError(t, err)
	upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
	c, rec := newAntigravityCompatContext("POST", "/v1/messages", nil)
	c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderMessages, interval: 5 * time.Millisecond})
	// Existing semantic output has already committed the stream. The append's
	// head wait must remain live without restarting the whole inference request.
	_, _ = c.Writer.WriteString(": already committed\n\n")
	c.Writer.Flush()
	result, err := svc.Forward(c.Request.Context(), c, antigravityThoughtAccount(nil), []byte(`{"model":"client-alias","stream":true,"messages":[{"role":"user","content":"hi"}]}`), false)
	require.NoError(t, err)
	require.EqualValues(t, 2, upstream.calls.Load())
	require.Equal(t, 28, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "event: ping")
	require.Contains(t, rec.Body.String(), "answer")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
	require.NotContains(t, rec.Body.String(), streamPreheaderTerminalCode)
}

func TestStreamPreheaderRetryHintPrecedence(t *testing.T) {
	for _, tc := range []struct{ header, expected string }{{"", "17.250"}, {"23", "23"}} {
		c, rec := newAntigravityCompatContext("POST", "/v1/messages", nil)
		c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: streamPreheaderMessages, interval: time.Second})
		_, _ = c.Writer.WriteString(": committed\n\n")
		c.Writer.Flush()
		err := finishStreamPreheaderSSEError(c, []byte(`{"error":{"type":"rate_limit_error","status":429,"message":"busy","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17.250s"}]}}`), http.Header{"Retry-After": {tc.header}})
		require.Error(t, err)
		require.Contains(t, rec.Body.String(), `"retry_after":"`+tc.expected+`"`)
		require.Contains(t, rec.Body.String(), `"type":"rate_limit_error"`)
		require.Contains(t, rec.Body.String(), `"status":429`)
	}
}
