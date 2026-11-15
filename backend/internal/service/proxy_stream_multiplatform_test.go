package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testSharedProxyCircuit() *openAIProxyStreamCircuit {
	return newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{failureThreshold: 1, failureWindow: time.Minute, quarantineTTL: time.Minute, maxEntries: 16})
}

func TestProxyCircuitAllPlatformsTransportStreamAndSuccess(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformAntigravity, PlatformGemini, PlatformGrok} {
		t.Run(platform, func(t *testing.T) {
			circuit := testSharedProxyCircuit()
			proxyID := int64(71)
			account := &Account{ID: 1, Platform: platform, ProxyID: &proxyID}
			ctx := context.Background()
			circuit.observeTransport(ctx, account, nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
			require.True(t, circuit.quarantined(ctx, account))
			for _, status := range []int{400, 401, 403, 429, 500} {
				circuit.observeTransport(ctx, account, &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("provider error"))}, nil)
				require.True(t, circuit.quarantined(ctx, account), "HTTP status must not reset or increment observations")
			}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: complete\n\n"))}
			circuit.observeTransport(ctx, account, response, nil)
			_, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.True(t, circuit.quarantined(ctx, account), "EOF is not proof of SSE completion")
			observeProxyStreamCompletion(response, true)
			require.False(t, circuit.quarantined(ctx, account))
			response = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: partial\n\n"))}
			circuit.observeTransport(ctx, account, response, nil)
			_, err = io.ReadAll(response.Body)
			require.NoError(t, err)
			observeProxyStreamCompletion(response, false)
			require.True(t, circuit.quarantined(ctx, account), "EOF before terminal must trip the bound proxy")
			response = &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}
			circuit.observeTransport(ctx, account, response, nil)
			_, err = io.ReadAll(response.Body)
			require.NoError(t, err)
			require.False(t, circuit.quarantined(ctx, account), "completed HTTP body resets proxy")
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			circuit.observeTransport(canceled, account, nil, &net.OpError{Op: "read", Err: io.ErrUnexpectedEOF})
			circuit.observeTransport(ctx, account, nil, context.Canceled)
			circuit.observeTransport(ctx, account, nil, errors.New("invalid upstream URL"))
			require.False(t, circuit.quarantined(ctx, account))
		})
	}
}

type proxySelectionRepo struct {
	AccountRepository
	accounts []Account
}

func (r *proxySelectionRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]Account, error) {
	return r.accounts, nil
}
func (r *proxySelectionRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, p string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, p)
}
func (r *proxySelectionRepo) ListSchedulableUngroupedByPlatforms(_ context.Context, platforms []string) ([]Account, error) {
	var accounts []Account
	for _, account := range r.accounts {
		for _, platform := range platforms {
			if account.Platform == platform {
				accounts = append(accounts, account)
				break
			}
		}
	}
	return accounts, nil
}
func (r *proxySelectionRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, p string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, p)
}
func (r *proxySelectionRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, errors.New("missing account")
}

func TestProxyCircuitGatewayAndGeminiSchedulingFailOpen(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformAntigravity, PlatformGemini} {
		t.Run(platform, func(t *testing.T) {
			bad, good := int64(7), int64(8)
			circuit := testSharedProxyCircuit()
			repo := &proxySelectionRepo{accounts: []Account{{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, ProxyID: &bad}, {ID: 2, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2, ProxyID: &good}}}
			ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, platform)
			circuit.recordFailure(bad, time.Now())
			gateway := &GatewayService{accountRepo: repo, cfg: &config.Config{}, proxyStreamCircuit: circuit}
			selected, err := gateway.SelectAccountForModelWithExclusions(ctx, nil, "", "", nil)
			require.NoError(t, err)
			require.EqualValues(t, 2, selected.ID)
			circuit.recordFailure(good, time.Now())
			selected, err = gateway.SelectAccountForModelWithExclusions(ctx, nil, "", "", nil)
			require.NoError(t, err)
			require.EqualValues(t, 1, selected.ID)
			result, err := gateway.SelectAccountWithLoadAwareness(ctx, nil, "", "", nil, "", 0)
			require.NoError(t, err)
			require.EqualValues(t, 1, result.Account.ID)
			_, err = gateway.SelectAccountForModelWithExclusions(ctx, nil, "", "", map[int64]struct{}{1: {}, 2: {}})
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			if platform == PlatformGemini {
				gemini := &GeminiMessagesCompatService{accountRepo: repo, cfg: &config.Config{}, proxyStreamCircuit: circuit}
				selected, err = gemini.SelectAccountForModelWithExclusions(ctx, nil, "", "", nil)
				require.NoError(t, err)
				require.EqualValues(t, 1, selected.ID)
				circuit.recordSuccess(good)
				selected, err = gemini.SelectAccountForModelWithExclusions(ctx, nil, "", "", nil)
				require.NoError(t, err)
				require.EqualValues(t, 2, selected.ID)
			}
		})
	}
}

func TestProxyCircuitSharedGatewayTransportSeams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	id := int64(9)
	account := &Account{ID: 1, ProxyID: &id, Concurrency: 1}
	circuit := testSharedProxyCircuit()
	upstream := &geminiCompatHTTPUpstreamStub{err: &net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}}
	ctx := context.Background()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	req := httptest.NewRequest("POST", "http://upstream.invalid/v1/messages", nil)
	gateway := &GatewayService{httpUpstream: upstream, proxyStreamCircuit: circuit}
	gemini := &GeminiMessagesCompatService{httpUpstream: upstream, proxyStreamCircuit: circuit}
	antigravity := &AntigravityGatewayService{httpUpstream: upstream, proxyStreamCircuit: circuit}
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformAntigravity} {
		account.Platform = platform
		circuit.recordSuccess(id)
		switch platform {
		case PlatformAnthropic:
			_, _, _ = gateway.doStreamUpstream(c, req, "http://proxy.invalid", account, nil, "")
		case PlatformGemini:
			_, _, _ = gemini.doStreamUpstream(c, req, "http://proxy.invalid", account, "")
		case PlatformAntigravity:
			_, _, _ = antigravity.doAntigravityInference(antigravityRetryLoopParams{ctx: ctx, c: c, account: account, httpUpstream: upstream}, req, nil)
		}
		require.True(t, circuit.quarantined(ctx, account))
	}
}

// Exercise real protocol readers rather than merely calling the circuit helper.
func TestProxyCircuitAntigravityProtocolReaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reader := range []string{"native_stream", "claude_stream", "native_collect", "claude_collect", "upstream_claude"} {
		for _, outcome := range []string{"complete", "truncated", "provider_error"} {
			t.Run(reader+"/"+outcome, func(t *testing.T) {
				circuit := testSharedProxyCircuit()
				id := int64(5)
				account := &Account{ID: 1, Platform: PlatformAntigravity, ProxyID: &id}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				beginUpstreamResponseModelObservation(c)
				payload := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}]}}` + "\n\n"
				if reader == "upstream_claude" {
					payload = "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
				}
				switch outcome {
				case "complete":
					circuit.recordFailure(id, time.Now())
					if reader == "upstream_claude" {
						payload += "data: {\"type\":\"message_stop\"}\n\n"
					} else {
						payload += "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n"
					}
				case "provider_error":
					payload += "data: {\"error\":{\"code\":429,\"message\":\"provider quota\"}}\n\n"
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
				circuit.observeTransport(c.Request.Context(), account, resp, nil)
				s := newAntigravityCompatService(config.GatewayConfig{}, nil)
				switch reader {
				case "native_stream":
					_, _ = s.handleGeminiStreamingResponse(c, resp, time.Now())
				case "claude_stream":
					_, _ = s.handleClaudeStreamingResponse(c, resp, time.Now(), "claude-test")
				case "native_collect":
					_, _ = s.handleGeminiStreamToNonStreaming(c, resp, time.Now())
				case "claude_collect":
					_, _, _ = s.collectClaudeStreamResponse(c, resp, time.Now(), "claude-test")
				case "upstream_claude":
					_ = s.streamUpstreamResponse(c, resp, time.Now())
				}
				require.Equal(t, outcome == "truncated", circuit.quarantined(c.Request.Context(), account))
			})
		}
	}
}

func TestProxyCircuitGeminiNativeDefaultAndAIStudioEligibility(t *testing.T) {
	circuit := testSharedProxyCircuit()
	id := int64(11)
	account := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, ProxyID: &id, Credentials: map[string]any{"api_key": "test-only"}}
	circuit.recordFailure(id, time.Now())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1beta/models/test:streamGenerateContent", nil)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n"))}
	circuit.observeTransport(c.Request.Context(), account, resp, nil)
	s := &GeminiMessagesCompatService{proxyStreamCircuit: circuit}
	_, err := s.handleNativeStreamingResponse(c, resp, time.Now(), false)
	require.NoError(t, err)
	require.False(t, circuit.quarantined(c.Request.Context(), account), "default preheader setting must still observe terminal")
	circuit.recordFailure(id, time.Now())
	s.accountRepo = &proxySelectionRepo{accounts: []Account{*account, {ID: 2, Platform: PlatformGemini, Type: AccountTypeServiceAccount}}}
	selected, err := s.SelectAccountForAIStudioEndpoints(context.Background(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, selected.ID, "ineligible Vertex account must not suppress the all-open fallback")
}

type proxyLocallyClosedBody struct{ closed chan struct{} }

func (b *proxyLocallyClosedBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("http2: response body closed")
}
func (b *proxyLocallyClosedBody) Close() error { close(b.closed); return nil }

func TestProxyCircuitIntentionalCloseIsNeutral(t *testing.T) {
	circuit := testSharedProxyCircuit()
	id := int64(4)
	account := &Account{ID: 1, Platform: PlatformOpenAI, ProxyID: &id}
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &proxyLocallyClosedBody{closed: make(chan struct{})}}
	circuit.observeTransport(context.Background(), account, resp, nil)
	readDone := make(chan error, 1)
	go func() { _, err := resp.Body.Read(make([]byte, 32)); readDone <- err }()
	require.NoError(t, resp.Body.Close())
	require.Error(t, <-readDone)
	require.False(t, circuit.quarantined(context.Background(), account))
}

func TestProxyCircuitOpenAIBufferedTerminalAndGatewayScannerOutcomes(t *testing.T) {
	for _, kind := range []string{"openai_complete", "openai_truncated", "claude_complete", "claude_truncated", "claude_error", "claude_oversize"} {
		t.Run(kind, func(t *testing.T) {
			circuit := testSharedProxyCircuit()
			id := int64(17)
			account := &Account{ID: 1, Platform: PlatformOpenAI, ProxyID: &id}
			payload := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
			if strings.HasPrefix(kind, "claude_") {
				payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"test\",\"role\":\"assistant\",\"content\":[]}}\n\n"
			}
			switch kind {
			case "openai_complete":
				circuit.recordFailure(id, time.Now())
				payload += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n"
			case "claude_complete":
				circuit.recordFailure(id, time.Now())
				payload += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			case "claude_error":
				payload += "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
			case "claude_oversize":
				payload += "event: content_block_delta\ndata: " + strings.Repeat("x", 128*1024) + "\n\n"
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
			circuit.observeTransport(c.Request.Context(), account, resp, nil)
			if strings.HasPrefix(kind, "openai_") {
				s := &OpenAIGatewayService{}
				_, _, _, _ = s.readOpenAICompatBufferedTerminal(resp, c, "test", "test")
			} else {
				s := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 512}}}
				_, _ = s.handleCCStreamingFromAnthropic(resp, c, "test", "test", nil, time.Now(), false)
			}
			require.Equal(t, strings.HasSuffix(kind, "truncated"), circuit.quarantined(c.Request.Context(), account))
		})
	}
}

func TestProxyCircuitTokenCountAllOpenPreservesCapability(t *testing.T) {
	id := int64(19)
	circuit := testSharedProxyCircuit()
	circuit.recordFailure(id, time.Now())
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, ProxyID: &id, Credentials: map[string]any{"openai_capabilities": []any{"chat_completions"}}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, ProxyID: &id, Credentials: map[string]any{"openai_capabilities": []any{"embeddings"}}},
	}
	s := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: &schedulerTestGatewayCache{}, cfg: &config.Config{}, openaiProxyStreamCircuit: circuit}
	group := int64(20)
	account, err := s.SelectAccountForTokenCount(context.Background(), &group, "", "gpt-5.1", OpenAIEndpointCapabilityChatCompletions, PlatformOpenAI)
	require.NoError(t, err)
	require.EqualValues(t, 1, account.ID)
}

func TestProxyCircuitConversionReaders(t *testing.T) {
	for _, reader := range []string{"gemini_messages", "gemini_chat", "openai_messages", "gateway_responses"} {
		for _, outcome := range []string{"complete", "truncated", "provider_error"} {
			t.Run(reader+"/"+outcome, func(t *testing.T) {
				circuit := testSharedProxyCircuit()
				id := int64(27)
				account := &Account{ID: 1, Platform: PlatformOpenAI, ProxyID: &id}
				if outcome == "complete" {
					circuit.recordFailure(id, time.Now())
				}
				var frames [3]string
				switch reader {
				case "gemini_messages", "gemini_chat":
					frames = [3]string{`data: {"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, `data: {"candidates":[{"finishReason":"STOP"}]}`, `data: {"error":{"code":429,"message":"quota"}}`}
				case "openai_messages":
					frames = [3]string{`data: {"type":"response.output_text.delta","delta":"hello"}`, `data: {"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[]}}`, `data: {"type":"error","message":"quota"}`}
				case "gateway_responses":
					frames = [3]string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"model\":\"test\",\"role\":\"assistant\",\"content\":[]}}", "event: message_stop\ndata: {\"type\":\"message_stop\"}", "event: error\ndata: {\"type\":\"error\"}"}
				}
				payload := frames[0] + "\n\n"
				if outcome == "complete" {
					payload += frames[1] + "\n\n"
				}
				if outcome == "provider_error" {
					payload += frames[2] + "\n\n"
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
				circuit.observeTransport(c.Request.Context(), account, resp, nil)
				switch reader {
				case "gemini_messages":
					_, _ = (&GeminiMessagesCompatService{}).handleStreamingResponse(c, resp, time.Now(), "test")
				case "gemini_chat":
					_, _ = (&GeminiMessagesCompatService{}).handleChatCompletionsStreamingResponseFromGemini(c, resp, time.Now(), "test", false, false)
				case "openai_messages":
					_, _ = (&OpenAIGatewayService{}).handleAnthropicStreamingResponse(resp, c, account, "test", "test", "test", time.Now())
				case "gateway_responses":
					_, _ = (&GatewayService{}).handleResponsesBufferedStreamingResponse(resp, c, "test", "test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				}
				require.Equal(t, outcome == "truncated", circuit.quarantined(c.Request.Context(), account))
			})
		}
	}
}
