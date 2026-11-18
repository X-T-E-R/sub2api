package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type handlerWarmupAccountRepoStub struct {
	service.AccountRepository
	accounts map[int64]*service.Account
}

func (r *handlerWarmupAccountRepoStub) GetByID(_ context.Context, id int64) (*service.Account, error) {
	acc, ok := r.accounts[id]
	if !ok {
		return nil, service.ErrAccountNotFound
	}
	return acc, nil
}

func (r *handlerWarmupAccountRepoStub) ClearModelRateLimits(_ context.Context, _ int64) error {
	return nil
}

func (r *handlerWarmupAccountRepoStub) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

type handlerWarmupTokenCacheStub struct{}

func (handlerWarmupTokenCacheStub) GetAccessToken(context.Context, string) (string, error) {
	return "token-admin-test", nil
}
func (handlerWarmupTokenCacheStub) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}
func (handlerWarmupTokenCacheStub) DeleteAccessToken(context.Context, string) error { return nil }
func (handlerWarmupTokenCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (handlerWarmupTokenCacheStub) ReleaseRefreshLock(context.Context, string) error { return nil }

type handlerWarmupUpstreamStub struct{}

func (handlerWarmupUpstreamStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return handlerWarmupOKResponse(), nil
}

func (handlerWarmupUpstreamStub) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return handlerWarmupOKResponse(), nil
}

func handlerWarmupOKResponse() *http.Response {
	sse := "data: {\"response\":{\"responseId\":\"r1\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"999\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2}}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

type handlerWarmupSettingRepoStub struct {
	service.SettingRepository
}

func (handlerWarmupSettingRepoStub) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func setupAccountWarmupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	repo := &handlerWarmupAccountRepoStub{
		accounts: map[int64]*service.Account{
			70: {
				ID:          70,
				Name:        "brad",
				Platform:    service.PlatformAntigravity,
				Type:        service.AccountTypeOAuth,
				Schedulable: false,
				Credentials: map[string]any{
					"project_id": "proj-70",
					"model_mapping": map[string]any{
						"gemini-3-flash":    "gemini-3-flash",
						"claude-sonnet-4-6": "claude-sonnet-4-6",
					},
				},
			},
			75: {
				ID:          75,
				Name:        "alice",
				Platform:    service.PlatformAntigravity,
				Type:        service.AccountTypeOAuth,
				Schedulable: true,
				Credentials: map[string]any{
					"project_id": "proj-75",
					"model_mapping": map[string]any{
						"gemini-3-flash": "gemini-3-flash",
					},
				},
			},
		},
	}

	tokenProvider := service.NewAntigravityTokenProvider(repo, handlerWarmupTokenCacheStub{}, nil)
	gateway := service.NewAntigravityGatewayService(
		repo,
		nil,
		nil,
		tokenProvider,
		nil,
		handlerWarmupUpstreamStub{},
		service.NewSettingService(handlerWarmupSettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1024 * 1024}}),
		nil,
		service.NewProxyStreamCircuit(&config.Config{}),
		nil,
	)
	warmupSvc := service.NewAccountWarmupService(
		repo,
		gateway,
		nil,
		service.WithWarmupSleep(func(context.Context, time.Duration) error { return nil }),
	)

	h := NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetAccountWarmupService(warmupSvc)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/warmup", h.Warmup)
	router.POST("/api/v1/admin/accounts/batch-warmup", h.BatchWarmup)
	return router
}

func TestAccountHandler_WarmupSingleAccountEmptyBodyAndOverrideID(t *testing.T) {
	router := setupAccountWarmupRouter()

	for _, body := range []string{"", `{}` /* empty JSON */, `{"account_ids":[999],"bucket_mode":"gemini_only"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/70/warmup", bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s resp=%s", body, rec.Body.String())

		var envelope struct {
			Code int                            `json:"code"`
			Data service.WarmupAccountsResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.Equal(t, 0, envelope.Code)
		require.Equal(t, 1, envelope.Data.TotalAccounts)
		require.Equal(t, 1, envelope.Data.SucceededAccounts)
		require.Equal(t, int64(70), envelope.Data.Results[0].AccountID)
	}
}

func TestAccountHandler_BatchWarmupAndValidation(t *testing.T) {
	router := setupAccountWarmupRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/admin/accounts/batch-warmup",
		bytes.NewBufferString(`{"account_ids":[70,75],"bucket_mode":"gemini_only","concurrency":2,"refresh_usage":false}`),
	)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var envelope struct {
		Code int                            `json:"code"`
		Data service.WarmupAccountsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.Equal(t, 2, envelope.Data.TotalAccounts)
	require.Equal(t, 2, envelope.Data.SucceededAccounts)
	require.Equal(t, "gemini_only", envelope.Data.BucketMode)

	// Invalid requests return 400.
	for _, invalid := range []string{`{"account_ids":[]}`, `{"account_ids":[70],"bucket_mode":"unknown"}`, `{"account_ids":[70],"bucket_mode":"custom_models","models":[]}`} {
		badRec := httptest.NewRecorder()
		badReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-warmup", bytes.NewBufferString(invalid))
		badReq.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(badRec, badReq)
		require.Equal(t, http.StatusBadRequest, badRec.Code, "payload=%s", invalid)
	}
}
