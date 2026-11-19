package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// capturedUpstreamRequest stores the exact outbound HTTP request emitted by the
// gateway forwarder toward an upstream provider.
type capturedUpstreamRequest struct {
	Attempt   int
	AccountID int64
	Method    string
	URL       string
	Headers   http.Header
	Body      []byte
	ctx       context.Context
}

func (r capturedUpstreamRequest) ContextDone() <-chan struct{} {
	if r.ctx == nil {
		return context.Background().Done()
	}
	return r.ctx.Done()
}

// upstreamResponder produces a deterministic in-process upstream HTTP response.
type upstreamResponder func(attempt int, req capturedUpstreamRequest) (*http.Response, error)

// gatewayHarnessConfig configures a self-contained in-process Gin gateway instance.
type gatewayHarnessConfig struct {
	Group    *service.Group
	Accounts []*service.Account
	Channels []service.Channel
	Settings map[string]string
	Config   *config.Config
	APIKeyID int64
	UserID   int64
	Respond  upstreamResponder
}

// gatewayHarness wires the real Gin router, gateway middlewares, handlers,
// scheduler snapshot, concurrency limiter, channel mapping service, usage log
// recorder, and a recording mock HTTPUpstream with zero external dependencies.
type gatewayHarness struct {
	router      *gin.Engine
	upstream    *recordingHTTPUpstream
	usageLogs   *recordingUsageLogRepo
	concurrency *countingConcurrencyCache
	accounts    *memAccountRepo
	settings    *service.SettingService
	group       *service.Group
	apiKey      *service.APIKey
}

func newGatewayHarness(t *testing.T, opts gatewayHarnessConfig) *gatewayHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	group := opts.Group
	if group == nil {
		group = &service.Group{
			ID:       100,
			Name:     "test-group",
			Platform: service.PlatformAntigravity,
			Status:   service.StatusActive,
			Hydrated: true,
		}
	}
	group.Hydrated = true
	if group.Status == "" {
		group.Status = service.StatusActive
	}

	// Ensure every supplied account is active, schedulable, and bound to the group.
	accounts := make([]*service.Account, 0, len(opts.Accounts))
	for _, acc := range opts.Accounts {
		if acc == nil {
			continue
		}
		if acc.Status == "" {
			acc.Status = service.StatusActive
		}
		if acc.Concurrency <= 0 {
			acc.Concurrency = 4
		}
		if len(acc.AccountGroups) == 0 {
			acc.AccountGroups = []service.AccountGroup{{AccountID: acc.ID, GroupID: group.ID}}
		}
		accounts = append(accounts, acc)
	}

	cfg := opts.Config
	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.RunMode == "" {
		cfg.RunMode = config.RunModeSimple
	}
	if cfg.Gateway.MaxBodySize == 0 {
		cfg.Gateway.MaxBodySize = 16 << 20
	}
	if cfg.Gateway.TextMaxBodySize == 0 {
		cfg.Gateway.TextMaxBodySize = 16 << 20
	}
	if cfg.Gateway.MaxLineSize == 0 {
		cfg.Gateway.MaxLineSize = 1 << 20
	}
	if cfg.Gateway.MaxAccountSwitches == 0 {
		cfg.Gateway.MaxAccountSwitches = 3
	}
	cfg.Security.URLAllowlist.Enabled = false

	settingRepo := &memSettingRepo{values: opts.Settings}
	settingService := service.NewSettingService(settingRepo, cfg)
	groupRepo := &memGroupRepo{group: group}
	accountRepo := &memAccountRepo{accounts: accounts}
	schedulerCache := &memSchedulerCache{accounts: accounts}
	schedulerSnapshot := service.NewSchedulerSnapshotService(schedulerCache, nil, accountRepo, nil, nil)
	concurrencyCache := &countingConcurrencyCache{}
	concurrencyService := service.NewConcurrencyService(concurrencyCache)
	usageRepo := &recordingUsageLogRepo{}
	billingService := service.NewBillingService(cfg, nil)
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)
	deferredService := service.NewDeferredService(nil, nil, 0)

	var channelService *service.ChannelService
	if len(opts.Channels) > 0 {
		channelRepo := &memChannelRepo{
			channels:       opts.Channels,
			groupPlatforms: map[int64]string{group.ID: group.Platform},
		}
		channelService = service.NewChannelService(channelRepo, groupRepo, nil, nil)
	}

	upstream := &recordingHTTPUpstream{respond: opts.Respond}
	agTokenProvider := service.NewAntigravityTokenProvider(accountRepo, staticAntigravityTokenCache{}, nil)
	agService := service.NewAntigravityGatewayService(
		accountRepo,
		nil,
		schedulerSnapshot,
		agTokenProvider,
		nil,
		upstream,
		settingService,
		nil,
		nil,
		nil,
	)

	gatewayService := service.NewGatewayService(
		accountRepo,
		groupRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		schedulerSnapshot,
		concurrencyService,
		billingService,
		nil,
		billingCacheService,
		nil,
		upstream,
		deferredService,
		nil,
		nil,
		nil,
		nil,
		settingService,
		nil,
		channelService,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	openAIService := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		schedulerSnapshot,
		concurrencyService,
		billingService,
		nil,
		billingCacheService,
		upstream,
		deferredService,
		nil,
		nil,
		nil,
		channelService,
		nil,
		settingService,
		nil,
		nil,
	)

	gatewayHandler := handler.NewGatewayHandler(
		gatewayService,
		openAIService,
		nil,
		agService,
		nil,
		concurrencyService,
		billingCacheService,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		settingService,
	)
	apiKeyID := opts.APIKeyID
	if apiKeyID == 0 {
		apiKeyID = 9001
	}
	userID := opts.UserID
	if userID == 0 {
		userID = 9002
	}
	groupID := group.ID
	group.Hydrated = true
	apiKey := &service.APIKey{
		ID:      apiKeyID,
		Key:     "sk-test-gateway-key",
		UserID:  userID,
		GroupID: &groupID,
		Group:   group,
		Status:  service.StatusActive,
		User: &service.User{
			ID:          userID,
			Concurrency: 64,
			Balance:     1000,
			Status:      service.StatusActive,
		},
	}
	apiKeyRepo := &memAPIKeyRepo{byKey: map[string]*service.APIKey{apiKey.Key: apiKey}}
	apiKeyService := service.NewAPIKeyService(apiKeyRepo, nil, groupRepo, nil, nil, nil, cfg)
	openAIHandler := handler.NewOpenAIGatewayHandler(
		openAIService,
		concurrencyService,
		billingCacheService,
		apiKeyService,
		nil,
		nil,
		nil,
		nil,
		cfg,
	)

	router := gin.New()
	authMW := middleware.NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg)
	routes.RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       gatewayHandler,
			OpenAIGateway: openAIHandler,
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		authMW,
		nil,
		nil,
		nil,
		settingService,
		nil,
		cfg,
	)

	return &gatewayHarness{
		router:      router,
		upstream:    upstream,
		usageLogs:   usageRepo,
		concurrency: concurrencyCache,
		accounts:    accountRepo,
		settings:    settingService,
		group:       group,
		apiKey:      apiKey,
	}
}

func (h *gatewayHarness) post(t *testing.T, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Authorization") == "" && req.Header.Get("x-api-key") == "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey.Key)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *gatewayHarness) get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Authorization") == "" && req.Header.Get("x-api-key") == "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey.Key)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *gatewayHarness) requests() []capturedUpstreamRequest {
	return h.upstream.snapshot()
}

func (h *gatewayHarness) usageRecords() []*service.UsageLog {
	return h.usageLogs.snapshot()
}

func (h *gatewayHarness) assertSlotsBalanced(t *testing.T) {
	t.Helper()
	require.Equal(t, h.concurrency.userAcquires.Load(), h.concurrency.userReleases.Load(), "user concurrency slots leaked")
	require.Equal(t, h.concurrency.accountAcquires.Load(), h.concurrency.accountReleases.Load(), "account concurrency slots leaked")
}

func requireUpstreamModel(t *testing.T, log *service.UsageLog, want string) {
	t.Helper()
	require.NotNil(t, log, "usage log is nil")
	require.NotNil(t, log.UpstreamModel, "usage log UpstreamModel is nil")
	require.Equal(t, want, *log.UpstreamModel)
}

// ---------------------------------------------------------------------------
// Response builders
// ---------------------------------------------------------------------------

func jsonHTTPResponse(status int, body string, extraHeaders ...map[string]string) *http.Response {
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	for _, m := range extraHeaders {
		for k, v := range m {
			hdr.Set(k, v)
		}
	}
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseHTTPResponse(lines ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req-mock-sse"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")),
	}
}

// antigravityTextSSE builds a minimal v1internal SSE stream returning text and usage.
func antigravityTextSSE(text string, promptTokens, outputTokens, cachedTokens int) *http.Response {
	line := `data: {"response":{"responseId":"resp_ag_ok","candidates":[{"content":{"role":"model","parts":[{"text":` +
		jsonQuote(text) + `}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":` +
		itoa(promptTokens) + `,"cachedContentTokenCount":` + itoa(cachedTokens) +
		`,"candidatesTokenCount":` + itoa(outputTokens) + `}}}`
	return sseHTTPResponse(line)
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// ---------------------------------------------------------------------------
// Minimal in-memory stubs (interface-embedded to avoid boilerplate bloat)
// ---------------------------------------------------------------------------

type recordingHTTPUpstream struct {
	mu      sync.Mutex
	calls   int
	reqs    []capturedUpstreamRequest
	respond upstreamResponder
}

func (u *recordingHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	var bodyBytes []byte
	if req != nil && req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	u.mu.Lock()
	attempt := u.calls
	u.calls++
	captured := capturedUpstreamRequest{
		Attempt:   attempt,
		AccountID: accountID,
		Method:    req.Method,
		URL:       req.URL.String(),
		Headers:   req.Header.Clone(),
		Body:      append([]byte(nil), bodyBytes...),
		ctx:       req.Context(),
	}
	u.reqs = append(u.reqs, captured)
	fn := u.respond
	u.mu.Unlock()

	if fn == nil {
		return nil, errors.New("gatewayHarness: Respond callback not configured")
	}
	return fn(attempt, captured)
}

func (u *recordingHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *recordingHTTPUpstream) snapshot() []capturedUpstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]capturedUpstreamRequest, len(u.reqs))
	copy(out, u.reqs)
	return out
}

type recordingUsageLogRepo struct {
	service.UsageLogRepository
	mu   sync.Mutex
	logs []*service.UsageLog
}

func (r *recordingUsageLogRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return true, nil
}

func (r *recordingUsageLogRepo) snapshot() []*service.UsageLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*service.UsageLog, len(r.logs))
	copy(out, r.logs)
	return out
}

func cloneTestAccount(a *service.Account) *service.Account {
	if a == nil {
		return nil
	}
	cp := *a
	if a.Credentials != nil {
		cp.Credentials = make(map[string]any, len(a.Credentials))
		for k, v := range a.Credentials {
			if m, ok := v.(map[string]any); ok {
				mc := make(map[string]any, len(m))
				for mk, mv := range m {
					mc[mk] = mv
				}
				cp.Credentials[k] = mc
			} else {
				cp.Credentials[k] = v
			}
		}
	}
	if a.Extra != nil {
		cp.Extra = make(map[string]any, len(a.Extra))
		for k, v := range a.Extra {
			cp.Extra[k] = v
		}
	}
	if a.AccountGroups != nil {
		cp.AccountGroups = append([]service.AccountGroup(nil), a.AccountGroups...)
	}
	return &cp
}

type memSchedulerCache struct {
	service.SchedulerCache
	mu       sync.RWMutex
	accounts []*service.Account
}

func (c *memSchedulerCache) GetSnapshot(_ context.Context, _ service.SchedulerBucket) ([]*service.Account, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*service.Account, 0, len(c.accounts))
	for _, a := range c.accounts {
		out = append(out, cloneTestAccount(a))
	}
	return out, true, nil
}

func (c *memSchedulerCache) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.accounts {
		if a != nil && a.ID == id {
			return cloneTestAccount(a), nil
		}
	}
	return nil, nil
}

func (c *memSchedulerCache) SetAccount(_ context.Context, _ *service.Account) error           { return nil }
func (c *memSchedulerCache) UpdateLastUsed(_ context.Context, _ map[int64]time.Time) error    { return nil }
func (c *memSchedulerCache) ListBuckets(_ context.Context) ([]service.SchedulerBucket, error) { return nil, nil }

type memAccountRepo struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts []*service.Account
}

func (r *memAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a != nil && a.ID == id {
			return cloneTestAccount(a), nil
		}
	}
	return nil, service.ErrAccountNotFound
}

func (r *memAccountRepo) ListSchedulableByGroupID(_ context.Context, _ int64) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		if a != nil && a.Schedulable {
			out = append(out, *cloneTestAccount(a))
		}
	}
	return out, nil
}

func (r *memAccountRepo) ListByGroup(_ context.Context, _ int64) ([]service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		if a != nil {
			out = append(out, *cloneTestAccount(a))
		}
	}
	return out, nil
}

func (r *memAccountRepo) SetRateLimited(_ context.Context, _ int64, _ time.Time) error { return nil }
func (r *memAccountRepo) SetModelRateLimit(_ context.Context, _ int64, _ string, _ time.Time, _ ...string) error {
	return nil
}
func (r *memAccountRepo) SetOverloaded(_ context.Context, _ int64, _ time.Time) error { return nil }
func (r *memAccountRepo) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	return nil
}
func (r *memAccountRepo) ClearTempUnschedulable(_ context.Context, _ int64) error       { return nil }
func (r *memAccountRepo) ClearAntigravityQuotaScopes(_ context.Context, _ int64) error  { return nil }
func (r *memAccountRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error { return nil }
func (r *memAccountRepo) SetError(_ context.Context, _ int64, _ string) error            { return nil }
func (r *memAccountRepo) ClearRateLimit(_ context.Context, _ int64) error                { return nil }
func (r *memAccountRepo) UpdateLastUsed(_ context.Context, _ int64) error                { return nil }

type memGroupRepo struct {
	service.GroupRepository
	group *service.Group
}

func (r *memGroupRepo) GetByID(_ context.Context, _ int64) (*service.Group, error) {
	return r.group, nil
}

func (r *memGroupRepo) GetByIDLite(_ context.Context, _ int64) (*service.Group, error) {
	return r.group, nil
}

type memChannelRepo struct {
	service.ChannelRepository
	channels       []service.Channel
	groupPlatforms map[int64]string
}

func (r *memChannelRepo) ListAll(_ context.Context) ([]service.Channel, error) {
	out := make([]service.Channel, len(r.channels))
	copy(out, r.channels)
	return out, nil
}

func (r *memChannelRepo) GetGroupPlatforms(_ context.Context, _ []int64) (map[int64]string, error) {
	return r.groupPlatforms, nil
}

type memSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *memSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[key]
	if !ok {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: v}, nil
}

func (r *memSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}

func (r *memSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func (r *memSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *memSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = make(map[string]string)
	}
	for k, v := range values {
		r.values[k] = v
	}
	return nil
}

func (r *memSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *memSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

type memAPIKeyRepo struct {
	service.APIKeyRepository
	byKey map[string]*service.APIKey
}

func (r *memAPIKeyRepo) GetByKey(_ context.Context, key string) (*service.APIKey, error) {
	if ak, ok := r.byKey[key]; ok && ak != nil {
		cp := *ak
		if ak.User != nil {
			u := *ak.User
			cp.User = &u
		}
		if ak.Group != nil {
			g := *ak.Group
			cp.Group = &g
		}
		return &cp, nil
	}
	return nil, service.ErrAPIKeyNotFound
}

func (r *memAPIKeyRepo) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	return r.GetByKey(ctx, key)
}

func (r *memAPIKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error {
	return nil
}

type staticAntigravityTokenCache struct{}

func (staticAntigravityTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return "mock-ag-access-token", nil
}
func (staticAntigravityTokenCache) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}
func (staticAntigravityTokenCache) DeleteAccessToken(context.Context, string) error { return nil }
func (staticAntigravityTokenCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (staticAntigravityTokenCache) ReleaseRefreshLock(context.Context, string) error { return nil }

type countingConcurrencyCache struct {
	service.ConcurrencyCache
	userAcquires    atomic.Int64
	userReleases    atomic.Int64
	accountAcquires atomic.Int64
	accountReleases atomic.Int64
}

func (c *countingConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	c.accountAcquires.Add(1)
	return true, nil
}

func (c *countingConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.accountReleases.Add(1)
	return nil
}

func (c *countingConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	c.userAcquires.Add(1)
	return true, nil
}

func (c *countingConcurrencyCache) ReleaseUserSlot(context.Context, int64, string) error {
	c.userReleases.Add(1)
	return nil
}

func (c *countingConcurrencyCache) GetAccountConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}

func (c *countingConcurrencyCache) GetUserConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}

func (c *countingConcurrencyCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	out := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, a := range accounts {
		out[a.ID] = &service.AccountLoadInfo{AccountID: a.ID}
	}
	return out, nil
}

func (c *countingConcurrencyCache) GetAccountConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	for _, id := range ids {
		out[id] = 0
	}
	return out, nil
}

func (c *countingConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (c *countingConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error { return nil }
func (c *countingConcurrencyCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (c *countingConcurrencyCache) DecrementWaitCount(context.Context, int64) error { return nil }
