package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type telemetryLocalHTTP struct {
	client *http.Client
	host   string
	calls  atomic.Int32
}

func (h *telemetryLocalHTTP) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if request.URL.Host != h.host {
		return nil, fmt.Errorf("fixture rejects non-local upstream %s", request.URL.Host)
	}
	h.calls.Add(1)
	return h.client.Do(request)
}

func (h *telemetryLocalHTTP) DoWithTLS(request *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(request, proxy, accountID, concurrency)
}

type telemetryLocalWSDialer struct {
	host     string
	delegate openAIWSClientDialer
}

func (d *telemetryLocalWSDialer) Dial(ctx context.Context, rawURL string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	target, err := url.Parse(rawURL)
	if err != nil || target.Host != d.host || proxy != "" {
		return nil, 0, nil, fmt.Errorf("fixture rejects non-local websocket %s", rawURL)
	}
	return d.delegate.Dial(ctx, rawURL, headers, "")
}

func telemetryContext(enabled bool) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "telemetry-local-fixture")
	c.Request.Header.Set("session_id", "telemetry-local-session")
	SetCodexTelemetryCapturePolicy(c, func() bool { return enabled })
	return c, recorder
}

func telemetryFrames(id string) []string {
	return []string{
		fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":"gpt-5.1"}}`, id),
		`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["opaque-engine","opaque-engine"]}}`,
		fmt.Sprintf(`{"type":"response.metadata","response_id":%q,"headers":{"x-codex-safety-buffering-faster-model":"buffering-hint","x-codex-active-limit":"primary","x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300","authorization":"secret-sentinel"}}`, id),
		fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":0}}}`, id),
	}
}

func telemetryService(cfg *config.Config, upstream HTTPUpstream) *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, toolCorrector: NewCodexToolCorrector(), openaiWSResolver: NewOpenAIWSProtocolResolver(cfg)}
}

func telemetryAccount(baseURL string) *Account {
	return &Account{ID: 9931, Name: "local-telemetry", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "synthetic-key", "base_url": baseURL}, Extra: map[string]any{}}
}

func TestCodexTelemetryHTTPAndSSEForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"json", "sse", "sse_nonstream", "passthrough", "bridge"} {
		t.Run(mode, func(t *testing.T) {
			frames := telemetryFrames("resp-http")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("X-Request-Id", "request-http")
				w.Header().Set("X-Codex-Secondary-Used-Percent", "25")
				w.Header().Set("X-Codex-Secondary-Window-Minutes", "10080")
				if mode == "json" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp-http","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":0}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range frames {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			httpUpstream := &telemetryLocalHTTP{client: upstream.Client(), host: target.Host}
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			svc := telemetryService(cfg, httpUpstream)
			account := telemetryAccount(upstream.URL)
			if mode == "passthrough" {
				account.Extra["openai_passthrough"] = true
			}
			var baseline string
			for _, enabled := range []bool{false, true} {
				c, recorder := telemetryContext(enabled)
				BeginOpenAIRequestObservation(c, time.Now())
				var result *OpenAIForwardResult
				var err error
				if mode == "bridge" {
					payload := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"synthetic"}`)
					result, err = svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "synthetic-key", payload, len(payload), "gpt-5.1", "", "", "", "", 1, func(frame []byte) error {
						_, _ = recorder.Write(frame)
						return nil
					})
				} else {
					stream := mode != "json" && mode != "sse_nonstream"
					result, err = svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"synthetic"}`, stream)))
				}
				require.NoError(t, err)
				require.Equal(t, "gpt-5.1", result.Model)
				require.Equal(t, "gpt-5.1", result.UpstreamResponseModel)
				require.Equal(t, 2, result.Usage.InputTokens)
				require.Nil(t, result.FirstTokenMs, "metadata and terminal-only traffic is not a token event")
				if !enabled {
					baseline = recorder.Body.String()
					require.Nil(t, result.CodexTelemetry)
				} else {
					require.Equal(t, baseline, recorder.Body.String(), "capture must preserve forwarded bytes")
					require.NotNil(t, result.CodexTelemetry)
					raw := string(codextelemetry.Marshal(result.CodexTelemetry))
					require.Contains(t, raw, `"secondary_used_percent":25`)
					require.NotContains(t, raw, "secret-sentinel")
					if mode != "json" {
						require.Contains(t, raw, "opaque-engine")
						require.Contains(t, raw, "buffering-hint")
					}
				}
			}
			require.Equal(t, int32(2), httpUpstream.calls.Load(), "one upstream request for each capture-on/off fixture")
		})
	}
}

func TestCodexTelemetryHTTP429AndAttemptIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: 429, Header: http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Safety-Buffering-Faster-Model": {"failed-attempt-hint"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"synthetic quota"}}`))},
		{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp-success","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":0}}`))},
	}}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	svc := telemetryService(cfg, upstream)
	c, _ := telemetryContext(true)
	account := telemetryAccount("http://127.0.0.1:1")
	body := []byte(`{"model":"gpt-5.1","input":"synthetic"}`)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Len(t, upstream.requests, 1, "exercise the upstream 429 branch, not request validation")
	entry := &OpsInsertErrorLogInput{StatusCode: 429, AccountID: &account.ID, UpstreamErrors: SnapshotOpsUpstreamErrors(c)}
	AttachCodexTelemetryToOpsEntry(c, entry)
	require.NotEmpty(t, entry.UpstreamErrors)
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	require.Contains(t, *entry.UpstreamErrorsJSON, "failed-attempt-hint")
	second, _ := telemetryContext(true)
	account.ID++
	result, err = svc.Forward(context.Background(), second, account, body)
	require.NoError(t, err)
	require.Nil(t, result.CodexTelemetry)
	require.Len(t, upstream.requests, 2)
}

func TestCodexTelemetryRealWebSocketModesAndTurnToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"http_ingress", "native", "ctx_pool"} {
		t.Run(mode, func(t *testing.T) {
			var requests, upgrades atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, http.Header{"X-Codex-Secondary-Used-Percent": {"7"}})
				if err != nil {
					return
				}
				upgrades.Add(1)
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					n := requests.Add(1)
					for _, frame := range telemetryFrames(fmt.Sprintf("resp-ws-%d", n)) {
						if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
							return
						}
					}
				}
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			dialer := &telemetryLocalWSDialer{host: target.Host, delegate: newDefaultOpenAIWSClientDialer()}
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 0
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.QueueLimitPerConn = 4
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			httpUpstream := &httpUpstreamRecorder{}
			svc := telemetryService(cfg, httpUpstream)
			svc.openaiWSPassthroughDialer = dialer
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc.openaiWSPool = pool
			account := telemetryAccount(upstream.URL)
			account.Extra["responses_websockets_v2_enabled"] = true
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
			if mode == "native" {
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			}
			if mode == "http_ingress" {
				for turn := 1; turn <= 2; turn++ {
					c, recorder := telemetryContext(true)
					previous := ""
					if turn == 2 {
						previous = `,"previous_response_id":"resp-ws-1"`
					}
					result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"synthetic"`+previous+`}`))
					require.NoError(t, err)
					require.NotNil(t, result.CodexTelemetry)
					require.Equal(t, fmt.Sprintf("resp-ws-%d", turn), result.CodexTelemetry.ResponseID)
					require.Nil(t, result.FirstTokenMs)
					require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"response.completed"`))
					raw := string(codextelemetry.Marshal(result.CodexTelemetry))
					if turn == 1 {
						require.Contains(t, raw, codextelemetry.WSUpgradeHeaders)
					} else {
						require.NotContains(t, raw, codextelemetry.WSUpgradeHeaders)
						require.True(t, result.CodexTelemetry.ConnectionReused)
					}
				}
			} else {
				controlCtx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				var enabled atomic.Bool
				enabled.Store(true)
				results := make(chan *OpenAIForwardResult, 3)
				server, serverErr := startPassthroughLifecycleServerWithHooks(t, controlCtx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
					SetCodexTelemetryCapturePolicy(c, enabled.Load)
					return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) { results <- result }}
				})
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer client.CloseNow()
				for turn := 1; turn <= 3; turn++ {
					if turn > 1 {
						enabled.Store(turn == 3)
						require.NoError(t, client.Write(controlCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`)))
					}
					for _, expected := range telemetryFrames(fmt.Sprintf("resp-ws-%d", turn)) {
						frame, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
						require.NoError(t, err)
						require.JSONEq(t, expected, string(frame))
					}
					select {
					case result := <-results:
						require.NotNil(t, result)
						require.Nil(t, result.FirstTokenMs)
						if turn == 2 {
							require.Nil(t, result.CodexTelemetry)
						} else {
							require.NotNil(t, result.CodexTelemetry)
							require.Equal(t, fmt.Sprintf("resp-ws-%d", turn), result.CodexTelemetry.ResponseID)
						}
						if turn == 3 {
							require.NotContains(t, string(codextelemetry.Marshal(result.CodexTelemetry)), codextelemetry.WSUpgradeHeaders)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("missing turn result")
					}
				}
				_ = client.Close(coderws.StatusNormalClosure, "fixture complete")
				select {
				case <-serverErr:
				case <-time.After(3 * time.Second):
					t.Fatal("relay did not stop")
				}
			}
			require.Equal(t, int32(1), upgrades.Load(), "connection reuse fixture")
			require.Empty(t, httpUpstream.requests, "telemetry must not add HTTP requests")
		})
	}
}

func TestCodexTelemetryRealWS429BeforeResponseCreated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"http_ingress", "native", "ctx_pool"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				requests.Add(1)
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"headers":{"x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300","x-codex-safety-buffering-faster-model":"ws-retry-hint"},"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"synthetic quota"}}`))
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			dialer := &telemetryLocalWSDialer{host: target.Host, delegate: newDefaultOpenAIWSClientDialer()}
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.QueueLimitPerConn = 4
			svc := telemetryService(cfg, &httpUpstreamRecorder{})
			svc.openaiWSPassthroughDialer = dialer
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc.openaiWSPool = pool
			account := telemetryAccount(upstream.URL)
			account.Extra["responses_websockets_v2_enabled"] = true
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
			if mode == "native" {
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			}
			var c *gin.Context
			if mode == "http_ingress" {
				c, _ = telemetryContext(true)
				result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"synthetic"}`))
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				controlCtx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				contexts := make(chan *gin.Context, 1)
				server, serverErr := startPassthroughLifecycleServerWithHooks(t, controlCtx, svc, account, func(current *gin.Context) *OpenAIWSIngressHooks {
					SetCodexTelemetryCapturePolicy(current, func() bool { return true })
					contexts <- current
					return nil
				})
				defer server.Close()
				client := dialPassthroughLifecycleClient(t, server)
				defer client.CloseNow()
				select {
				case c = <-contexts:
				case <-time.After(3 * time.Second):
					t.Fatal("missing gateway context")
				}
				select {
				case err := <-serverErr:
					require.Error(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("429 did not finish relay")
				}
			}
			entry := &OpsInsertErrorLogInput{StatusCode: 429, AccountID: &account.ID, UpstreamErrors: SnapshotOpsUpstreamErrors(c)}
			AttachCodexTelemetryToOpsEntry(c, entry)
			require.NoError(t, sanitizeOpsUpstreamErrors(entry))
			require.NotNil(t, entry.UpstreamErrorsJSON, "the actual error frame's headers must reach the existing Ops path")
			require.Contains(t, *entry.UpstreamErrorsJSON, "ws-retry-hint")
			require.Contains(t, *entry.UpstreamErrorsJSON, `"association":"upstream_attempt"`)
			require.NotContains(t, *entry.UpstreamErrorsJSON, "engine_ids")
			require.Equal(t, int32(1), requests.Load(), "capturing a 429 adds no retry or probe")
		})
	}
}
