//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type preheaderHandlerUpstream struct {
	status    int
	truncated bool
	calls     atomic.Int32
}

func (s *preheaderHandlerUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.calls.Add(1)
	select {
	case <-time.After(1100 * time.Millisecond):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	if s.truncated {
		return handlerThoughtStream(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`), nil
	}
	if s.status == 200 {
		return handlerThoughtStream(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"actual answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}`), nil
	}
	return &http.Response{StatusCode: s.status, Header: http.Header{"Retry-After": []string{"17"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture rejection"}}`))}, nil
}
func (s *preheaderHandlerUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, id, concurrency)
}

func TestStreamPreheaderHandlerMessagesBillsOnlyRealUsage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		truncated bool
	}{
		{"success", 200, false}, {"authentication error", 401, false}, {"rate limit", 429, false}, {"partial usage then EOF", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(9300)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive}
			account := handlerThoughtAccount()
			account.Extra = map[string]any{}
			account.Schedulable = true
			account.AccountGroups = []service.AccountGroup{{AccountID: account.ID, GroupID: groupID}}
			cfg := &config.Config{RunMode: config.RunModeSimple, Gateway: config.GatewayConfig{MaxLineSize: 1 << 20, StreamKeepaliveInterval: 1, StreamPreheaderKeepaliveEnabled: true}}
			upstream := &preheaderHandlerUpstream{status: tc.status, truncated: tc.truncated}
			antigravity := service.NewAntigravityGatewayService(nil, nil, nil,
				service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil), nil, upstream,
				service.NewSettingService(handlerThoughtSettingRepo{}, cfg), nil)
			cache := &countingThoughtConcurrencyCache{fakeConcurrencyCache: &fakeConcurrencyCache{}}
			concurrency := service.NewConcurrencyService(cache)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			usage := &thoughtUsageLogRepoStub{}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
			gateway := service.NewGatewayService(
				nil, &fakeGroupRepo{group: group}, usage, nil, nil, nil, nil, nil, cfg, snapshot, concurrency, service.NewBillingService(cfg, nil), nil, billing, nil, nil,
				service.NewDeferredService(nil, nil, 0), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			h := &GatewayHandler{gatewayService: gateway, antigravityGatewayService: antigravity, billingCacheService: billing,
				concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0), maxAccountSwitches: 1, maxAccountSwitchesGemini: 1, cfg: cfg}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := `{"model":"client-alias","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"question"}]}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(context.WithValue(context.Background(), ctxkey.Group, group))
			key := &service.APIKey{ID: 9302, UserID: 9303, GroupID: &groupID, Group: group, Status: service.StatusActive, User: &service.User{ID: 9303, Concurrency: 10, Balance: 100}}
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.UserID, Concurrency: 10})
			h.Messages(c)
			require.Equal(t, 200, recorder.Code)
			require.True(t, strings.HasPrefix(recorder.Body.String(), "event: ping\n"), recorder.Body.String())
			require.EqualValues(t, 1, upstream.calls.Load())
			require.EqualValues(t, 1, cache.userAcquires.Load())
			require.EqualValues(t, 1, cache.userReleases.Load())
			require.EqualValues(t, 1, cache.accountAcquires.Load())
			require.EqualValues(t, 1, cache.accountReleases.Load())
			if tc.status == 200 {
				require.Len(t, usage.created, 1)
				require.Equal(t, 8, usage.created[0].InputTokens)
				require.Equal(t, 3, usage.created[0].OutputTokens)
				require.Equal(t, account.ID, usage.created[0].AccountID)
				if tc.truncated {
					require.True(t, service.StreamPreheaderFinalized(c))
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "gateway_stream_terminal_error"))
					require.NotContains(t, recorder.Body.String(), "event: message_stop")
				} else {
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: message_stop"))
				}
			} else {
				require.Empty(t, usage.created, "a preheader heartbeat is neither model output nor measured usage")
				require.Equal(t, 1, strings.Count(recorder.Body.String(), "gateway_stream_terminal_error"))
				require.NotContains(t, recorder.Body.String(), "message_start")
				require.NotContains(t, recorder.Body.String(), "message_stop")
			}
		})
	}
}

func TestStreamPreheaderHandlersRequireAuthentication(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 10}}
	gateway := &GatewayHandler{cfg: cfg}
	openai := &OpenAIGatewayHandler{cfg: cfg}
	for _, tc := range []struct {
		name, path string
		call       func(*gin.Context)
	}{
		{"messages", "/v1/messages", gateway.Messages},
		{"chat", "/v1/chat/completions", gateway.ChatCompletions},
		{"responses", "/v1/responses", gateway.Responses},
		{"gemini", "/v1beta/models/fixture:streamGenerateContent", gateway.GeminiV1BetaModels},
		{"openai-messages", "/v1/messages", openai.Messages},
		{"openai-chat", "/v1/chat/completions", openai.ChatCompletions},
		{"openai-responses", "/v1/responses", openai.Responses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewBufferString(`{"stream":true}`))
			c.Request.Header.Set("X-Task49-Private-Preheader-SSE", "1")
			tc.call(c)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
			require.NotContains(t, recorder.Header().Get("Content-Type"), "event-stream")
			require.NotContains(t, recorder.Body.String(), "ping")
			require.False(t, service.StreamPreheaderConfigured(c))
		})
	}
}
