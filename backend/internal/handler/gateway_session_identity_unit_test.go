//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type identityBindingKey struct {
	group int64
	key   string
}

type identityBindings struct {
	testutil.StubGatewayCache
	owners  map[identityBindingKey]int64
	lookups []identityBindingKey
}

func (s *identityBindings) GetSessionAccountID(_ context.Context, group int64, key string) (int64, error) {
	k := identityBindingKey{group, key}
	s.lookups = append(s.lookups, k)
	if owner := s.owners[k]; owner > 0 {
		return owner, nil
	}
	return 0, service.ErrStickySessionNotFound
}
func (s *identityBindings) SetSessionAccountID(_ context.Context, group int64, key string, owner int64, _ time.Duration) error {
	s.owners[identityBindingKey{group, key}] = owner
	return nil
}
func (s *identityBindings) DeleteSessionAccountID(_ context.Context, group int64, key string) error {
	delete(s.owners, identityBindingKey{group, key})
	return nil
}

type identityConcurrency struct {
	fakeConcurrencyCache
	busy74 bool
}

func (s *identityConcurrency) AcquireAccountSlot(_ context.Context, id int64, _ int, _ string) (bool, error) {
	return id != 74 || !s.busy74, nil
}

type identityFixture struct {
	h      *GatewayHandler
	cache  *identityBindings
	slots  *identityConcurrency
	group  *service.Group
	apiKey *service.APIKey
	digest *service.DigestSessionStore
	a, b   *service.Account
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 100, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := func(id int64, priority int) *service.Account {
		return &service.Account{
			ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: priority,
			Credentials:   map[string]any{"intercept_warmup_requests": true, "model_mapping": map[string]any{"client-model": "upstream-model"}},
			AccountGroups: []service.AccountGroup{{AccountID: id, GroupID: group.ID}},
		}
	}
	f := &identityFixture{group: group, a: account(74, 1), b: account(73, 2), cache: &identityBindings{owners: map[identityBindingKey]int64{}}, slots: &identityConcurrency{}}
	f.apiKey = &service.APIKey{ID: 200, GroupID: &group.ID, Group: group, UserID: 300, User: &service.User{ID: 300, Balance: 100, Concurrency: 10}, Status: service.StatusActive}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = time.Second
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{f.a, f.b}}, nil, nil, nil, nil)
	concurrency := service.NewConcurrencyService(f.slots)
	f.digest = service.NewDigestSessionStore()
	gw := service.NewGatewayService(nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, f.cache, cfg,
		snapshot, concurrency, nil, nil, nil, nil, nil, nil, nil, nil, nil, f.digest, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	f.h = &GatewayHandler{gatewayService: gw, billingCacheService: billing,
		concurrencyHelper: NewConcurrencyHelper(concurrency, SSEPingFormatClaude, 0), maxAccountSwitches: 1, cfg: cfg}
	return f
}

func identityBody(t *testing.T, session string, round int) []byte {
	t.Helper()
	messages := []any{}
	// Preserve the prior content prefix while moving the cache marker to the
	// newest message. The selected cacheable text is S1 -> S2 -> S3 -> S4.
	for i := 1; i <= round; i++ {
		part := map[string]any{"type": "text", "text": "S" + strconv.Itoa(i)}
		if i == round {
			part["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages = append(messages, map[string]any{"role": "user", "content": []any{part}})
	}
	// Exercise the real handler and selector without a provider connection; the
	// established warmup interceptor returns only after actual account selection.
	messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Warmup"}}})
	body, err := json.Marshal(map[string]any{"model": "client-model", "max_tokens": 256, "metadata": map[string]any{"user_id": session}, "messages": messages})
	require.NoError(t, err)
	return body
}

func (f *identityFixture) requestContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), ctxkey.Group, f.group)
	c.Request = httptest.NewRequest("POST", path, bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), f.apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: f.apiKey.UserID, Concurrency: 10})
	return c, recorder
}

func (f *identityFixture) request(t *testing.T, body []byte, header string) (int64, identityBindingKey) {
	t.Helper()
	c, recorder := f.requestContext("/v1/messages", body)
	if header != "" {
		c.Request.Header.Set("session_id", header)
	}
	start := len(f.cache.lookups)
	f.h.Messages(c)
	require.Equal(t, 200, recorder.Code, recorder.Body.String())
	selected, ok := c.Get(opsAccountIDKey)
	require.True(t, ok)
	require.Greater(t, len(f.cache.lookups), start)
	return selected.(int64), f.cache.lookups[start]
}

func TestGatewayMessagesStableSessionSurvivesMovingCacheMarker(t *testing.T) {
	for _, carrier := range []string{"metadata", "header"} {
		t.Run(carrier, func(t *testing.T) {
			f := newIdentityFixture(t)
			const session = "session_123e4567-e89b-12d3-a456-426614174000"
			metadata, header := session, ""
			if carrier == "header" {
				metadata, header = "ordinary-user-id", session
			}
			// The pre-fix cache-control key is already owned by 73. Do not import it
			// into this independent explicit session, even when S4 later appears.
			legacy := identityBindingKey{f.group.ID, strconv.FormatUint(xxhash.Sum64String("S4"), 36)}
			f.cache.owners[legacy] = 73
			var first identityBindingKey
			for i, round := range []int{1, 2, 3, 4, 4} {
				selected, key := f.request(t, identityBody(t, metadata, round), header)
				require.Equal(t, int64(74), selected, "round %d", i)
				if i == 0 {
					first = key
					f.b.Priority = 0 // a cache miss would now choose 73, not 74
				}
				require.Equal(t, first, key)
				require.True(t, strings.HasPrefix(key.key, "v2:"))
				require.Equal(t, int64(74), f.cache.owners[first])
			}
			require.Equal(t, int64(73), f.cache.owners[legacy])

			f.apiKey.ID++
			selected, otherKey := f.request(t, identityBody(t, metadata, 4), header)
			require.Equal(t, int64(73), selected, "another API key must not inherit 74")
			require.NotEqual(t, first, otherKey)
			f.apiKey.ID--
			f.group.ID++
			selected, otherKey = f.request(t, identityBody(t, metadata, 4), header)
			require.Equal(t, int64(73), selected, "another group must not inherit 74")
			require.NotEqual(t, first, otherKey)
			f.group.ID--
			metadata = "session_223e4567-e89b-12d3-a456-426614174000"
			if carrier == "header" {
				header, metadata = metadata, "ordinary-user-id"
			}
			selected, otherKey = f.request(t, identityBody(t, metadata, 4), header)
			require.Equal(t, int64(73), selected, "another explicit session sharing S4 must remain independent")
			require.NotEqual(t, first, otherKey)
		})
	}
}

func TestGatewayStableIdentityPreservesEligibilityWaitAndExclusions(t *testing.T) {
	f := newIdentityFixture(t)
	_, key := f.request(t, identityBody(t, "session_123e4567-e89b-12d3-a456-426614174000", 1), "")
	ctx := context.WithValue(context.Background(), ctxkey.Group, f.group)
	selectAccount := func(excluded map[int64]struct{}) *service.AccountSelectionResult {
		result, err := f.h.gatewayService.SelectAccountWithLoadAwareness(ctx, &f.group.ID, key.key, "client-model", excluded, "", 0)
		require.NoError(t, err)
		if result.Acquired && result.ReleaseFunc != nil {
			t.Cleanup(result.ReleaseFunc)
		}
		return result
	}
	f.slots.busy74 = true
	waiting := selectAccount(nil)
	require.False(t, waiting.Acquired)
	require.NotNil(t, waiting.WaitPlan)
	require.Equal(t, int64(74), waiting.WaitPlan.AccountID)
	f.slots.busy74 = false
	require.Equal(t, int64(73), selectAccount(map[int64]struct{}{74: {}}).Account.ID, "failover exclusion remains authoritative")

	for _, reason := range []string{"model mapping", "quota"} {
		t.Run(reason, func(t *testing.T) {
			f.cache.owners[key] = 74
			credentials := f.a.Credentials
			if reason == "model mapping" {
				f.a.Credentials = map[string]any{"model_mapping": map[string]any{"another-model": "upstream"}}
			} else {
				f.a.Extra = map[string]any{"quota_limit": float64(10), "quota_used": float64(10)}
			}
			require.Equal(t, int64(73), selectAccount(nil).Account.ID, "sticky identity must not bypass %s", reason)
			f.a.Credentials, f.a.Extra = credentials, nil
		})
	}
}

// These are real ingress calls with a deliberately missing provider API key.
// They stop at the forward boundary after selection, not at a mock selector.
func TestGatewayCompatibleIngressSharesAuthenticatedSession(t *testing.T) {
	for _, carrier := range []string{"metadata", "header"} {
		t.Run(carrier, func(t *testing.T) {
			f := newIdentityFixture(t)
			metadata, header := "session_123e4567-e89b-12d3-a456-426614174000", ""
			if carrier == "header" {
				metadata, header = "ordinary-user", metadata
			}
			_, key := f.request(t, identityBody(t, metadata, 1), header)
			f.b.Priority = 0
			for _, ingress := range []struct {
				path string
				body string
				call func(*gin.Context)
			}{
				{"/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"changed"}]}`, f.h.ChatCompletions},
				{"/v1/responses", `{"model":"client-model","input":"changed"}`, f.h.Responses},
				{"/v1/messages/count_tokens", `{"model":"client-model","messages":[{"role":"user","content":"changed"}]}`, f.h.CountTokens},
			} {
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(ingress.body), &body))
				body["metadata"] = map[string]any{"user_id": metadata}
				wire, err := json.Marshal(body)
				require.NoError(t, err)
				c, recorder := f.requestContext(ingress.path, wire)
				c.Request.Header.Set("session_id", header)
				start := len(f.cache.lookups)
				ingress.call(c)
				require.GreaterOrEqual(t, recorder.Code, 400, "fixture must fail before a provider call")
				selected, ok := c.Get(opsAccountIDKey)
				require.True(t, ok, "%s body=%s", ingress.path, recorder.Body.String())
				require.Equal(t, int64(74), selected, ingress.path)
				require.Greater(t, len(f.cache.lookups), start)
				require.Equal(t, key, f.cache.lookups[start], ingress.path)
			}
		})
	}
}

func TestGatewayNativeGeminiExplicitSessionCannotBeAdoptedByDigest(t *testing.T) {
	f := newIdentityFixture(t)
	f.group.Platform, f.a.Platform, f.b.Platform = service.PlatformGemini, service.PlatformGemini, service.PlatformGemini
	f.h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, nil, nil, f.h.cfg)
	const body = `{"contents":[{"role":"user","parts":[{"text":"shared prompt"}]}]}`
	invoke := func(session string) (int64, identityBindingKey) {
		c, recorder := f.requestContext("/v1beta/models/client-model:generateContent", []byte(body))
		c.Params = gin.Params{{Key: "modelAction", Value: "client-model:generateContent"}}
		c.Request.Header.Set("session_id", session)
		// The legacy content digest is owned by 73. A genuinely new explicit
		// session must not inherit it, even on its very first cache miss.
		var req antigravity.GeminiRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		prefix := service.GenerateGeminiPrefixHash(f.apiKey.UserID, f.apiKey.ID, ip.GetClientIP(c), "", service.PlatformGemini, "client-model")
		f.digest.Save(f.group.ID, prefix, service.BuildGeminiDigestChain(&req), "older-conversation", 73, "")
		start := len(f.cache.lookups)
		f.h.GeminiV1BetaModels(c)
		require.GreaterOrEqual(t, recorder.Code, 400, "fixture has no provider credentials")
		selected, ok := c.Get(opsAccountIDKey)
		require.True(t, ok, recorder.Body.String())
		require.Greater(t, len(f.cache.lookups), start)
		return selected.(int64), f.cache.lookups[start]
	}
	selected, first := invoke("new-explicit-session")
	require.Equal(t, int64(74), selected, "digest must not import 73 into a new explicit session")
	f.b.Priority = 0
	selected, again := invoke("new-explicit-session")
	require.Equal(t, int64(74), selected)
	require.Equal(t, first, again)
	f.b.Priority = 2 // without digest recovery a fresh selection would choose 74
	selected, _ = invoke("")
	require.Equal(t, int64(73), selected, "content-only clients retain digest recovery")
}

func TestGatewayNativeGeminiCLISessionRetainsAPIKeyScope(t *testing.T) {
	f := newIdentityFixture(t)
	f.group.Platform, f.a.Platform, f.b.Platform = service.PlatformGemini, service.PlatformGemini, service.PlatformGemini
	f.h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, nil, nil, f.h.cfg)
	const body = `{"contents":[{"role":"user","parts":[{"text":"/home/test/.gemini/tmp/f7851b009ed314d1baee62e83115f486160283f4a55a582d89fdac8b9fe3b740"}]}]}`
	invoke := func() (int64, identityBindingKey) {
		c, recorder := f.requestContext("/v1beta/models/client-model:generateContent", []byte(body))
		c.Params = gin.Params{{Key: "modelAction", Value: "client-model:generateContent"}}
		c.Request.Header.Set("x-gemini-api-privileged-user-id", "native-user")
		start := len(f.cache.lookups)
		f.h.GeminiV1BetaModels(c)
		require.GreaterOrEqual(t, recorder.Code, 400)
		selected, ok := c.Get(opsAccountIDKey)
		require.True(t, ok, recorder.Body.String())
		require.Greater(t, len(f.cache.lookups), start)
		return selected.(int64), f.cache.lookups[start]
	}
	selected, first := invoke()
	require.Equal(t, int64(74), selected)
	require.True(t, strings.HasPrefix(first.key, "gemini:v2:"))
	f.b.Priority = 0
	selected, again := invoke()
	require.Equal(t, int64(74), selected)
	require.Equal(t, first, again)
	f.apiKey.ID++
	selected, other := invoke()
	require.Equal(t, int64(73), selected)
	require.NotEqual(t, first, other, "native CLI seed must not bypass API key isolation")
}

func TestGatewaySessionContextUsesEffectiveTargetPlatform(t *testing.T) {
	f := newIdentityFixture(t)
	c, _ := f.requestContext("/v1/messages", nil)
	parsed := &service.ParsedRequest{MetadataUserID: "session_123e4567-e89b-12d3-a456-426614174000"}
	setGatewaySessionContext(c, f.apiKey, parsed)
	base := f.h.gatewayService.GenerateSessionHash(parsed)
	f.group.Platform = service.PlatformComposite
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformAnthropic))
	setGatewaySessionContext(c, f.apiKey, parsed)
	require.Equal(t, base, f.h.gatewayService.GenerateSessionHash(parsed), "resolved target, not composite container, defines scope")
	// Match ForcePlatform middleware's two projections (handler + service).
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.ForcePlatform, service.PlatformAntigravity))
	c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
	setGatewaySessionContext(c, f.apiKey, parsed)
	require.NotEqual(t, base, f.h.gatewayService.GenerateSessionHash(parsed))
	require.Equal(t, service.PlatformAntigravity, parsed.SessionContext.Platform, "forced route takes precedence")
}
