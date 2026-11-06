//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type preheaderAPIKeyPairUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *preheaderAPIKeyPairUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n" +
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_failed\",\"usage\":{\"input_tokens\":9,\"output_tokens\":2},\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}}\n\n"))}, nil
}

func TestStreamPreheaderFollowupAPIKeyPairBillsOnce(t *testing.T) {
	cfg := &config.Config{RunMode: config.RunModeSimple, Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
	cfg.Default.RateMultiplier = 1
	account := service.Account{ID: 9302, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fixture-token"}}
	require.False(t, account.IsOpenAIPassthroughEnabled())
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	upstream := &preheaderAPIKeyPairUpstream{}
	usage := &thoughtUsageLogRepoStub{}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(accountRepo, usage, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.2","input":"fixture question","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	groupID := int64(9300)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 9301, GroupID: &groupID,
		User: &service.User{ID: 9303, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9303, Concurrency: 0})
	// Model the already-committed stream at the reviewed API-key entry point.
	_, _ = c.Writer.WriteString(": committed\n\n")
	c.Writer.Flush()
	h.Responses(c)
	require.True(t, service.StreamPreheaderFinalized(c))
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "gateway_stream_terminal_error"))
	require.NotContains(t, rec.Body.String(), "event: error\n")
	require.Len(t, usage.created, 1)
	require.Equal(t, 9, usage.created[0].InputTokens)
	require.Equal(t, 2, usage.created[0].OutputTokens)
	require.Equal(t, account.ID, usage.created[0].AccountID)
}
