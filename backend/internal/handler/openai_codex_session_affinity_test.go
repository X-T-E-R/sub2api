//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	codexAffinityHandlerGroupID = int64(1901)
	codexAffinityHandlerAPIKey  = int64(1902)
	codexAffinityHandlerUserID  = int64(1903)
	codexAffinityHandlerRoot    = "explicit-root-session"
)

type codexAffinityHandlerSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *codexAffinityHandlerSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, nil
}

func (r *codexAffinityHandlerSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key], nil
}

func (r *codexAffinityHandlerSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func (r *codexAffinityHandlerSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *codexAffinityHandlerSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *codexAffinityHandlerSettingRepo) GetAll(context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *codexAffinityHandlerSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

type codexAffinityHandlerRepo struct {
	service.AccountRepository

	mu          sync.Mutex
	accounts    []service.Account
	bindings    map[string]string
	owners      map[string]service.CodexSessionAccountOwner
	allocations int
	tempIDs     []int64
}

func cloneCodexAffinityHandlerAccount(account service.Account) service.Account {
	copy := account
	copy.Credentials = cloneCredentialMap(account.Credentials)
	copy.Extra = cloneCredentialMap(account.Extra)
	copy.GroupIDs = append([]int64(nil), account.GroupIDs...)
	return copy
}

func (r *codexAffinityHandlerRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, account := range r.accounts {
		if account.ID == id {
			copy := cloneCodexAffinityHandlerAccount(account)
			return &copy, nil
		}
	}
	return nil, nil
}

func (r *codexAffinityHandlerRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.schedulableByPlatformLocked(platform), nil
}

func (r *codexAffinityHandlerRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *codexAffinityHandlerRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

func (r *codexAffinityHandlerRepo) schedulableByPlatformLocked(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform && account.IsSchedulable() {
			out = append(out, cloneCodexAffinityHandlerAccount(account))
		}
	}
	return out
}

func (r *codexAffinityHandlerRepo) ListAllWithFilters(_ context.Context, platform, _, _, _ string, _ int64, _ string) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if platform == "" || account.Platform == platform {
			out = append(out, cloneCodexAffinityHandlerAccount(account))
		}
	}
	return out, nil
}

func (r *codexAffinityHandlerRepo) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempIDs = append(r.tempIDs, id)
	return nil
}

func (r *codexAffinityHandlerRepo) SetRateLimited(context.Context, int64, time.Time) error {
	return nil
}

func (r *codexAffinityHandlerRepo) FindBinding(_ context.Context, scope, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindings[scope+":"+key], nil
}

func (r *codexAffinityHandlerRepo) Allocate(_ context.Context, scope, key, _ string, _ int64, _, _ int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bindings == nil {
		r.bindings = make(map[string]string)
	}
	mapKey := scope + ":" + key
	if existing := r.bindings[mapKey]; existing != "" {
		return existing, nil
	}
	r.allocations++
	value := "daily-session-" + strconv.Itoa(r.allocations)
	r.bindings[mapKey] = value
	return value, nil
}

func (r *codexAffinityHandlerRepo) CleanupDays(context.Context, string, int) (int64, error) {
	return 0, nil
}

func (r *codexAffinityHandlerRepo) FindSessionOwner(_ context.Context, binding string) (*service.CodexSessionAccountOwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.owners[binding]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}

func (r *codexAffinityHandlerRepo) ClaimSessionOwner(_ context.Context, binding string, proposed service.CodexSessionAccountOwner) (*service.CodexSessionAccountOwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners == nil {
		r.owners = make(map[string]service.CodexSessionAccountOwner)
	}
	if owner, ok := r.owners[binding]; ok {
		return &owner, nil
	}
	proposed.Revision = 1
	r.owners[binding] = proposed
	return &proposed, nil
}

func (r *codexAffinityHandlerRepo) MoveSessionOwner(_ context.Context, binding string, expected, proposed service.CodexSessionAccountOwner) (*service.CodexSessionAccountOwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner := r.owners[binding]
	if owner == expected {
		proposed.Revision = owner.Revision + 1
		r.owners[binding] = proposed
		owner = proposed
	}
	return &owner, nil
}

func (r *codexAffinityHandlerRepo) FindSessionBindingScopes(_ context.Context, binding string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.owners[binding]; ok {
		return []string{owner.AccountScope}, nil
	}
	return nil, nil
}

type codexAffinityHandlerAttempt struct {
	accountID int64
	body      []byte
	headers   http.Header
}

type codexAffinityHandlerUpstream struct {
	mu       sync.Mutex
	mode     string
	attempts []codexAffinityHandlerAttempt
}

func (u *codexAffinityHandlerUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	u.attempts = append(u.attempts, codexAffinityHandlerAttempt{
		accountID: accountID,
		body:      append([]byte(nil), body...),
		headers:   req.Header.Clone(),
	})
	mode := u.mode
	firstAccount := u.attempts[0].accountID
	u.mu.Unlock()
	if (mode == "http_401" || mode == "http_429") && accountID == firstAccount {
		status := http.StatusUnauthorized
		headers := http.Header{"Content-Type": {"application/json"}}
		if mode == "http_429" {
			status = http.StatusTooManyRequests
			headers.Set("x-codex-primary-used-percent", "100")
			headers.Set("x-codex-primary-window-minutes", "300")
			headers.Set("x-codex-primary-reset-after-seconds", "3600")
		}
		return &http.Response{
			StatusCode: status, Header: headers,
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture account rejected request"}}`)),
		}, nil
	}

	switch mode {
	case "http_502":
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"fixture upstream 502"}}`)),
		}, nil
	case "transport_eof":
		return nil, io.ErrUnexpectedEOF
	case "stream_eof":
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       &codexAffinityUnexpectedEOFReader{data: []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")},
		}, nil
	default:
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " +
				`{"type":"response.completed","response":{"id":"resp_fixture","object":"response","model":"gpt-5.6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n")),
		}, nil
	}
}

func (u *codexAffinityHandlerUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *codexAffinityHandlerUpstream) snapshotAttempts() []codexAffinityHandlerAttempt {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]codexAffinityHandlerAttempt, len(u.attempts))
	copy(out, u.attempts)
	return out
}

type codexAffinityUnexpectedEOFReader struct {
	data []byte
	done bool
}

func (r *codexAffinityUnexpectedEOFReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, io.ErrUnexpectedEOF
}

func (*codexAffinityUnexpectedEOFReader) Close() error { return nil }

type codexAffinityHandlerFixture struct {
	h        *OpenAIGatewayHandler
	router   *gin.Engine
	repo     *codexAffinityHandlerRepo
	upstream *codexAffinityHandlerUpstream
	pool     *service.CodexDailySessionPool
	billing  *service.BillingCacheService
	settings *service.SettingService
}

func newCodexAffinityHandlerFixture(t *testing.T, mode string) *codexAffinityHandlerFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 3

	accounts := []service.Account{
		codexAffinityHandlerAccount(19011, "credential-A", 1),
		codexAffinityHandlerAccount(19012, "credential-B", 100),
	}
	repo := &codexAffinityHandlerRepo{accounts: accounts, bindings: make(map[string]string), owners: make(map[string]service.CodexSessionAccountOwner)}
	settingRepo := &codexAffinityHandlerSettingRepo{values: map[string]string{"openai_advanced_scheduler_enabled": "false"}}
	settingService := service.NewSettingService(settingRepo, cfg)
	require.NoError(t, settingService.SetCodexSessionAffinitySettings(context.Background(), service.CodexSessionAffinitySettings{GroupIDs: []int64{codexAffinityHandlerGroupID}}))

	pool := service.NewCodexDailySessionPool(repo, cfg)
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	upstream := &codexAffinityHandlerUpstream{mode: mode}
	gateway := service.ProvideOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, nil, cfg, nil,
		nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, settingService, nil, pool,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	concurrency := service.NewConcurrencyService(cache)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	groupID := codexAffinityHandlerGroupID
	apiKey := &service.APIKey{
		ID: codexAffinityHandlerAPIKey, GroupID: &groupID, Status: service.StatusAPIKeyActive,
		User:  &service.User{ID: codexAffinityHandlerUserID, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: codexAffinityHandlerUserID, Concurrency: 1})
		c.Next()
	})
	router.POST("/openai/v1/responses", h.Responses)
	router.POST("/openai/v1/messages", h.Messages)
	router.POST("/openai/v1/chat/completions", h.ChatCompletions)

	fixture := &codexAffinityHandlerFixture{h: h, router: router, repo: repo, upstream: upstream, pool: pool, billing: billingCache, settings: settingService}
	t.Cleanup(func() {
		pool.Close()
		billingCache.Stop()
	})
	return fixture
}

func codexAffinityHandlerAccount(id int64, credential string, priority int) service.Account {
	return service.Account{
		ID: id, Name: credential, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: priority,
		Credentials: map[string]any{
			"access_token":       "token-" + credential,
			"chatgpt_account_id": credential,
			"chatgpt_user_id":    "user-" + credential,
		},
		Extra: map[string]any{
			"codex_fingerprint_mode":            "session",
			service.CodexDailySessionEnabledKey: true,
			service.CodexDailySessionMinKey:     5,
			service.CodexDailySessionMaxKey:     10,
		},
	}
}

func codexAffinityHandlerRequest(t *testing.T, route string, stream bool) *http.Request {
	t.Helper()
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	var body string
	switch route {
	case "responses":
		body = fmt.Sprintf(`{"model":"gpt-5.6-luna","input":"hello","client_metadata":{"session_id":"%s","thread_id":"thread-1"},"stream":%s}`, codexAffinityHandlerRoot, streamValue)
	case "messages":
		body = fmt.Sprintf(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":%s}`, streamValue)
	case "chat":
		body = fmt.Sprintf(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello"}],"stream":%s}`, streamValue)
	default:
		t.Fatalf("unknown route %q", route)
	}
	path := "/openai/v1/" + route
	if route == "chat" {
		path = "/openai/v1/chat/completions"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", codexAffinityHandlerRoot)
	return req
}

func TestOpenAICodexSessionAffinityHandlerStopsHTTPFailoverAcrossRoutes(t *testing.T) {
	for _, route := range []string{"responses", "messages", "chat"} {
		for _, mode := range []string{"http_502", "transport_eof"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				fixture := newCodexAffinityHandlerFixture(t, mode)
				recorder := httptest.NewRecorder()
				fixture.router.ServeHTTP(recorder, codexAffinityHandlerRequest(t, route, false))

				attempts := fixture.upstream.snapshotAttempts()
				require.Len(t, attempts, 1, "session-affine request must not replay on another pooled OAuth account")
				require.Len(t, fixture.repo.accounts, 2, "the fixture must retain a second schedulable OAuth account")
				require.NotEmpty(t, attempts[0].headers.Get("session-id"))
				require.NotEqual(t, codexAffinityHandlerRoot, attempts[0].headers.Get("session-id"))
				if route == "responses" {
					require.NotEmpty(t, gjson.GetBytes(attempts[0].body, "client_metadata.session_id").String())
					require.NotEqual(t, codexAffinityHandlerRoot, gjson.GetBytes(attempts[0].body, "client_metadata.session_id").String())
				}
				require.Len(t, fixture.repo.owners, 1, "the durable session owner must be claimed before forwarding")
				for _, owner := range fixture.repo.owners {
					require.Equal(t, owner.AccountID, attempts[0].accountID, "the only upstream attempt must use the durable owner")
				}

				require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
				require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
				require.NotContains(t, recorder.Body.String(), "event:")
			})
		}
	}
}

func TestOpenAICodexSessionAffinityHandlerWritesResponsesFailedAfterStreamEOF(t *testing.T) {
	fixture := newCodexAffinityHandlerFixture(t, "stream_eof")
	recorder := httptest.NewRecorder()
	fixture.router.ServeHTTP(recorder, codexAffinityHandlerRequest(t, "responses", true))

	attempts := fixture.upstream.snapshotAttempts()
	require.Len(t, attempts, 1, "stream transport EOF must not trigger an account replay")
	require.Len(t, fixture.repo.owners, 1)
	for _, owner := range fixture.repo.owners {
		require.Equal(t, owner.AccountID, attempts[0].accountID)
	}
	require.Contains(t, recorder.Body.String(), "response.output_text.delta")
	require.Contains(t, recorder.Body.String(), "event: response.failed")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
}

func TestOpenAICodexSessionAffinityHandlerReplayProtectionWithoutEnrollment(t *testing.T) {
	for _, route := range []string{"responses", "messages", "chat"} {
		t.Run(route, func(t *testing.T) {
			fixture := newCodexAffinityHandlerFixture(t, "transport_eof")
			require.NoError(t, fixture.settings.SetCodexSessionAffinitySettings(context.Background(), service.CodexSessionAffinitySettings{}))
			recorder := httptest.NewRecorder()
			fixture.router.ServeHTTP(recorder, codexAffinityHandlerRequest(t, route, false))
			require.Equal(t, http.StatusBadGateway, recorder.Code)
			require.Len(t, fixture.upstream.snapshotAttempts(), 1)
			require.Empty(t, fixture.repo.owners)
		})
	}
}

func TestOpenAICodexSessionAffinityHandlerMovesRejectedAccount(t *testing.T) {
	for _, route := range []string{"responses", "messages", "chat"} {
		for _, mode := range []string{"http_401", "http_429", "missing_token"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				fixture := newCodexAffinityHandlerFixture(t, mode)
				if mode == "missing_token" {
					delete(fixture.repo.accounts[0].Credentials, "access_token")
				}
				for range 2 {
					recorder := httptest.NewRecorder()
					fixture.router.ServeHTTP(recorder, codexAffinityHandlerRequest(t, route, false))
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					require.Contains(t, recorder.Body.String(), "hello")
				}
				attempts := fixture.upstream.snapshotAttempts()
				if mode == "missing_token" {
					require.Len(t, attempts, 2, "missing credential must fail over before upstream send")
					require.Equal(t, fixture.repo.accounts[1].ID, attempts[0].accountID)
					require.Equal(t, attempts[0].accountID, attempts[1].accountID)
					return
				}
				require.Len(t, attempts, 3, "reject A, succeed B, next request stays B")
				require.NotEqual(t, attempts[0].accountID, attempts[1].accountID)
				require.Equal(t, attempts[1].accountID, attempts[2].accountID)
				require.NotEqual(t, attempts[0].headers.Get("session-id"), attempts[1].headers.Get("session-id"))
				require.Equal(t, attempts[1].headers.Get("session-id"), attempts[2].headers.Get("session-id"))
				for _, owner := range fixture.repo.owners {
					require.Equal(t, attempts[1].accountID, owner.AccountID)
					require.Equal(t, int64(2), owner.Revision)
				}
				require.Len(t, fixture.repo.bindings, 2, "keep old account allocation; allocate once on replacement")
			})
		}
	}
}

var _ service.HTTPUpstream = (*codexAffinityHandlerUpstream)(nil)
