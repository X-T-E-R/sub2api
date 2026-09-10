package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type sessionOwnerFixture struct {
	dailyPoolFixtureRepo
	ownerMu            sync.Mutex
	owners             map[string]CodexSessionAccountOwner
	history            map[string][]string
	ownerReads, claims int
	ownerErr           error
	claimBarrier       func()
}

func (r *sessionOwnerFixture) FindSessionOwner(_ context.Context, key string) (*CodexSessionAccountOwner, error) {
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()
	r.ownerReads++
	if r.ownerErr != nil {
		return nil, r.ownerErr
	}
	owner, ok := r.owners[key]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}
func (r *sessionOwnerFixture) ClaimSessionOwner(_ context.Context, key string, owner CodexSessionAccountOwner) (*CodexSessionAccountOwner, error) {
	if r.claimBarrier != nil {
		r.claimBarrier()
	}
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()
	r.claims++
	if r.ownerErr != nil {
		return nil, r.ownerErr
	}
	if r.owners == nil {
		r.owners = make(map[string]CodexSessionAccountOwner)
	}
	if existing, ok := r.owners[key]; ok {
		return &existing, nil
	}
	r.owners[key] = owner
	return &owner, nil
}
func (r *sessionOwnerFixture) FindSessionBindingScopes(_ context.Context, key string) ([]string, error) {
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()
	return r.history[key], r.ownerErr
}

type sessionOwnerAccountRepo struct {
	schedulerGroupAwareOpenAIAccountRepo
}

func (r sessionOwnerAccountRepo) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	return r.accounts, nil
}

func sessionAffinityTestContext(t *testing.T, key int64, root string) (context.Context, *gin.Context) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("session_id", root)
	c.Set("api_key", &APIKey{ID: key})
	ctx := WithCodexSessionAffinity(c.Request.Context(), c, nil)
	c.Request = c.Request.WithContext(ctx)
	return ctx, c
}

func sessionAffinityTestAccount(id int64, credential string) Account {
	a := *dailyPoolAccount()
	a.ID = id
	a.Credentials = maps.Clone(a.Credentials)
	a.Credentials["chatgpt_account_id"] = credential
	a.Status = StatusActive
	a.Schedulable = true
	a.GroupIDs = []int64{1}
	return a
}

func sessionAffinityTestService(t *testing.T, repo *sessionOwnerFixture, accounts ...Account) *OpenAIGatewayService {
	t.Helper()
	pool := NewCodexDailySessionPool(repo, nil)
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{}, codexDailySessionPool: pool,
		accountRepo: sessionOwnerAccountRepo{schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}}},
		cache:       &schedulerTestGatewayCache{}, settingService: &SettingService{},
	}
	svc.settingService.codexSessionAffinityCache.Store(CodexSessionAffinitySettings{GroupIDs: []int64{1}})
	return svc
}

func sessionAffinitySelect(svc *OpenAIGatewayService, ctx context.Context, groupID int64, previous string, excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, previous, "legacy-sticky", "gpt-5.6-luna", excluded,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses, false, true, false, PlatformOpenAI)
}

func TestCodexSessionAffinityCanonicalRoot(t *testing.T) {
	ctx, c := sessionAffinityTestContext(t, 7, "old-header")
	_ = ctx
	body := []byte(`{"client_metadata":{"session_id":"old-flat","x-codex-turn-metadata":"{\"session_id\":\"native-root\",\"thread_id\":\"child\"}"}}`)
	ctx = WithCodexSessionAffinity(c.Request.Context(), c, body)
	state, ok := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	require.True(t, ok)
	require.Equal(t, "native-root", state.root)
	require.Equal(t, codexSessionBindingKey(7, "native-root"), state.binding)
	require.NotEqual(t, codexSessionBindingKey(8, "native-root"), state.binding)
	c.Request.Header.Del("session_id")
	ctx = WithCodexSessionAffinity(context.Background(), c, []byte(`{"prompt_cache_key":"reusable-prompt","input":"not-an-identity"}`))
	require.Nil(t, ctx.Value(codexSessionAffinityContextKey{}))
}

func TestCodexSessionAffinityMixedCandidatesAndPersistentOwner(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	pooled := sessionAffinityTestAccount(11, "A")
	legacy := sessionAffinityTestAccount(12, "B")
	legacy.Extra = maps.Clone(legacy.Extra)
	legacy.Extra[CodexDailySessionEnabledKey] = false
	legacy.Priority = -100
	repo := &sessionOwnerFixture{}
	svc := sessionAffinityTestService(t, repo, pooled, legacy)
	ctx, _ := sessionAffinityTestContext(t, 7, "root")
	selection, decision, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, pooled.ID, selection.Account.ID)
	require.Equal(t, "codex_session_new", decision.Layer)
	releaseCodexSessionSelection(selection)
	require.True(t, CodexSessionAffinityActive(ctx))
	require.Equal(t, 1, repo.claims)

	// Existing trees remain fixed with enrollment disabled and the legacy
	// scheduler enabled. Warm owner reads do not issue claim writes or SQL.
	svc.settingService.codexSessionAffinityCache.Store(CodexSessionAffinitySettings{})
	svc.codexDailySessionPool.cache.Wait()
	reads := repo.ownerReads
	ctx, _ = sessionAffinityTestContext(t, 7, "root")
	selection, decision, err = sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.NoError(t, err)
	require.Equal(t, pooled.ID, selection.Account.ID)
	require.Equal(t, "codex_session_owner", decision.Layer)
	require.Equal(t, reads, repo.ownerReads)
	require.Equal(t, 1, repo.claims)
	releaseCodexSessionSelection(selection)

	svc.codexDailySessionPool.cache.Clear()
	ctx, _ = sessionAffinityTestContext(t, 7, "root")
	selection, _, err = sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.NoError(t, err)
	require.Equal(t, pooled.ID, selection.Account.ID)
	require.Greater(t, repo.ownerReads, reads)
	releaseCodexSessionSelection(selection)

	ctx, _ = sessionAffinityTestContext(t, 7, "root")
	selection, _, err = sessionAffinitySelect(svc, ctx, 2, "", nil)
	require.ErrorIs(t, err, ErrCodexSessionAffinity)
	require.Nil(t, selection)
	ctx, _ = sessionAffinityTestContext(t, 7, "root")
	selection, _, err = sessionAffinitySelect(svc, ctx, 1, "", map[int64]struct{}{pooled.ID: {}})
	require.ErrorIs(t, err, ErrCodexSessionAffinity)
	require.Nil(t, selection)
}

func TestCodexSessionAffinityNoEscapeAndBoundedWait(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
	repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID}}}
	svc := sessionAffinityTestService(t, repo, a, b)
	svc.cfg.Gateway.Scheduling.StickySessionWaitTimeout = 3 * time.Second
	svc.cfg.Gateway.Scheduling.StickySessionMaxWaiting = 4
	svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{a.ID: false, b.ID: true}})
	svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "true")
	scheduler := svc.getOpenAIAccountScheduler(context.Background())
	for range 8 {
		n := 120000
		scheduler.ReportResult(a.ID, false, &n)
	}
	ctx, _ := sessionAffinityTestContext(t, 7, "root")
	selection, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.NoError(t, err)
	require.Equal(t, a.ID, selection.Account.ID)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, a.ID, selection.WaitPlan.AccountID)
	require.Equal(t, 3*time.Second, selection.WaitPlan.Timeout)
	require.Zero(t, scheduler.SnapshotMetrics().AccountSwitchTotal)
}

func TestCodexSessionAffinityLegacyRecoveryAndConflicts(t *testing.T) {
	a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
	for _, tc := range []struct {
		name     string
		scopes   []string
		previous int64
		want     int64
		fail     bool
	}{
		{"unique history", []string{CodexDailySessionScope(&a)}, 0, a.ID, false},
		{"ambiguous history", []string{CodexDailySessionScope(&a), CodexDailySessionScope(&b)}, 0, 0, true},
		{"previous resolves history", []string{CodexDailySessionScope(&a), CodexDailySessionScope(&b)}, b.ID, b.ID, false},
		{"previous conflicts", []string{CodexDailySessionScope(&a)}, b.ID, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &sessionOwnerFixture{history: map[string][]string{codexSessionBindingKey(7, "root"): tc.scopes}}
			svc := sessionAffinityTestService(t, repo, a, b)
			previous := ""
			if tc.previous > 0 {
				previous = "resp_prior"
				require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(context.Background(), 1, previous, tc.previous, time.Hour))
			}
			ctx, _ := sessionAffinityTestContext(t, 7, "root")
			selection, _, err := sessionAffinitySelect(svc, ctx, 1, previous, nil)
			if tc.fail {
				require.ErrorIs(t, err, ErrCodexSessionAffinity)
				require.Nil(t, selection)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, selection.Account.ID)
			releaseCodexSessionSelection(selection)
		})
	}
}

func TestCodexSessionAffinityConcurrentFirstClaim(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
	var arrivals sync.WaitGroup
	arrivals.Add(2)
	repo := &sessionOwnerFixture{claimBarrier: func() { arrivals.Done(); arrivals.Wait() }}
	services := []*OpenAIGatewayService{sessionAffinityTestService(t, repo, a, b), sessionAffinityTestService(t, repo, a, b)}
	type outcome struct {
		selection *AccountSelectionResult
		err       error
	}
	results := make(chan outcome, 2)
	for i, svc := range services {
		ctx, _ := sessionAffinityTestContext(t, 7, "root")
		// Force each initial candidate to be different. Losing the claim must
		// never return its candidate even if the winner is excluded for it.
		excluded := map[int64]struct{}{b.ID: {}}
		if i == 1 {
			excluded = map[int64]struct{}{a.ID: {}}
		}
		go func() {
			selection, _, err := sessionAffinitySelect(svc, ctx, 1, "", excluded)
			results <- outcome{selection, err}
		}()
	}
	ownerIDs := map[int64]bool{}
	for range 2 {
		r := <-results
		if r.err != nil {
			require.ErrorIs(t, r.err, ErrCodexSessionAffinity)
			continue
		}
		require.NotNil(t, r.selection)
		ownerIDs[r.selection.Account.ID] = true
		releaseCodexSessionSelection(r.selection)
	}
	require.Len(t, ownerIDs, 1)
	require.Len(t, repo.owners, 1)
}

func TestCodexSessionAffinityStorageFailureAndFinalIdentity(t *testing.T) {
	a := sessionAffinityTestAccount(11, "A")
	repo := &sessionOwnerFixture{ownerErr: errors.New("database unavailable")}
	svc := sessionAffinityTestService(t, repo, a)
	ctx, _ := sessionAffinityTestContext(t, 7, "root")
	selection, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.ErrorIs(t, err, ErrCodexSessionAffinity)
	require.Nil(t, selection)
	require.Zero(t, repo.claims)
	state, ok := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	require.True(t, ok)
	state.strict.Store(true)
	state.owner.Store(&CodexSessionAccountOwner{AccountScope: CodexDailySessionScope(&a), AccountID: a.ID})
	require.NoError(t, validateCodexSessionProjection(ctx, &a, "root"))
	require.ErrorIs(t, validateCodexSessionProjection(ctx, &a, "another-root"), ErrCodexSessionAffinity)
	b := sessionAffinityTestAccount(12, "B")
	require.ErrorIs(t, validateCodexSessionProjection(ctx, &b, "root"), ErrCodexSessionAffinity)
}

func TestCodexSessionAffinityAPIKeyIsolation(t *testing.T) {
	a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
	repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{
		codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID},
		codexSessionBindingKey(8, "root"): {AccountScope: CodexDailySessionScope(&b), AccountID: b.ID},
	}}
	svc := sessionAffinityTestService(t, repo, a, b)
	for key, want := range map[int64]int64{7: a.ID, 8: b.ID} {
		t.Run(fmt.Sprint(key), func(t *testing.T) {
			ctx, _ := sessionAffinityTestContext(t, key, "root")
			selected, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
			require.NoError(t, err)
			require.Equal(t, want, selected.Account.ID)
			releaseCodexSessionSelection(selected)
		})
	}
}

func TestCodexSessionAffinityForwardingRoutes(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		t.Run(route, func(t *testing.T) {
			a := sessionAffinityTestAccount(11, "A")
			a.Extra["openai_oauth_passthrough"] = route == "passthrough"
			repo := &sessionOwnerFixture{}
			svc := sessionAffinityTestService(t, repo, a)
			svc.toolCorrector = NewCodexToolCorrector()
			var pooledSession string
			for _, thread := range []string{"parent", "child", "nested-child"} {
				ctx, c := sessionAffinityTestContext(t, 7, "root")
				c.Request.Header.Set("thread-id", thread)
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
				selection, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
				require.NoError(t, err)
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_"+thread, "gpt-5.6-luna")}
				svc.httpUpstream = upstream
				body := identityFixture(t, "root", thread, "turn-"+thread, 1, nil)
				body["model"], body["instructions"], body["input"] = "gpt-5.6-luna", "fixture", []any{}
				raw, marshalErr := json.Marshal(body)
				require.NoError(t, marshalErr)
				switch route {
				case "chat":
					raw = []byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello"}]}`)
					_, err = svc.ForwardAsChatCompletions(ctx, c, selection.Account, raw, "root", "gpt-5.6-luna")
				case "messages":
					raw = []byte(`{"model":"gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
					_, err = svc.ForwardAsAnthropic(ctx, c, selection.Account, raw, "root", "gpt-5.6-luna")
				default:
					_, err = svc.Forward(ctx, c, selection.Account, raw)
				}
				releaseCodexSessionSelection(selection)
				require.NoError(t, err)
				require.NotNil(t, upstream.lastReq)
				session := upstream.lastReq.Header.Get("session-id")
				require.NotEmpty(t, session)
				if pooledSession == "" {
					pooledSession = session
				}
				require.Equal(t, pooledSession, session)
				require.NotEqual(t, session, upstream.lastReq.Header.Get("thread-id"))
				if route == "responses" || route == "passthrough" {
					require.Equal(t, session, gjson.GetBytes(upstream.lastBody, "client_metadata.session_id").String())
				}
			}
			require.Len(t, repo.owners, 1)
			require.Equal(t, 1, repo.allocations)
		})
	}
}

func TestCodexSessionAffinityRefusesChangedCredentialBeforeSend(t *testing.T) {
	a := sessionAffinityTestAccount(11, "A")
	repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID}}}
	svc := sessionAffinityTestService(t, repo, a)
	ctx, c := sessionAffinityTestContext(t, 7, "root")
	selected, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
	require.NoError(t, err)
	defer releaseCodexSessionSelection(selected)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200}}
	svc.httpUpstream = upstream
	changed := *selected.Account
	changed.Credentials = maps.Clone(changed.Credentials)
	changed.Credentials["chatgpt_account_id"] = "B"
	_, err = svc.Forward(ctx, c, &changed, []byte(`{"model":"gpt-5.6-luna","input":"hello"}`))
	require.ErrorIs(t, err, ErrCodexSessionAffinity)
	require.Nil(t, upstream.lastReq)
	changed = *selected.Account
	changed.Extra = maps.Clone(changed.Extra)
	changed.Extra[codexFingerprintModeExtraKey] = "device"
	_, err = svc.Forward(ctx, c, &changed, []byte(`{"model":"gpt-5.6-luna","input":"hello"}`))
	require.ErrorIs(t, err, ErrCodexSessionAffinity)
	require.Nil(t, upstream.lastReq)
}

func TestCodexSessionAffinityUnknownPreviousAndUnavailableOwner(t *testing.T) {
	a, b := sessionAffinityTestAccount(11, "A"), sessionAffinityTestAccount(12, "B")
	t.Run("unowned continuation does not choose account", func(t *testing.T) {
		repo := &sessionOwnerFixture{}
		svc := sessionAffinityTestService(t, repo, a, b)
		ctx, _ := sessionAffinityTestContext(t, 7, "root")
		selected, _, err := sessionAffinitySelect(svc, ctx, 1, "resp_unknown", nil)
		require.ErrorIs(t, err, ErrCodexSessionAffinity)
		require.Nil(t, selected)
		require.Zero(t, repo.claims)
	})
	for _, reason := range []string{"disabled", "error", "quota", "mode", "credential", "proxy circuit"} {
		t.Run(reason, func(t *testing.T) {
			bound := a
			bound.Extra = maps.Clone(a.Extra)
			bound.Credentials = maps.Clone(a.Credentials)
			repo := &sessionOwnerFixture{owners: map[string]CodexSessionAccountOwner{codexSessionBindingKey(7, "root"): {AccountScope: CodexDailySessionScope(&a), AccountID: a.ID}}}
			switch reason {
			case "disabled":
				bound.Schedulable = false
			case "error":
				bound.Status = StatusError
			case "quota":
				deadline := time.Now().Add(time.Hour)
				bound.RateLimitResetAt = &deadline
			case "mode":
				bound.Extra[codexFingerprintModeExtraKey] = "device"
			case "credential":
				bound.Credentials["chatgpt_account_id"] = "changed"
			case "proxy circuit":
				id := int64(1)
				bound.ProxyID = &id
			}
			svc := sessionAffinityTestService(t, repo, bound, b)
			if reason == "proxy circuit" {
				circuit := svc.getOpenAIProxyStreamCircuit()
				circuit.mu.Lock()
				circuit.entries[1] = openAIProxyStreamCircuitEntry{blockedUntil: time.Now().Add(time.Minute), lastTouched: time.Now()}
				circuit.mu.Unlock()
			}
			ctx, _ := sessionAffinityTestContext(t, 7, "root")
			selected, _, err := sessionAffinitySelect(svc, ctx, 1, "", nil)
			require.ErrorIs(t, err, ErrCodexSessionAffinity)
			require.Nil(t, selected)
			require.Zero(t, repo.claims)
		})
	}
}

func TestCodexSessionAffinityCompactRetryRequiresExplicitRejection(t *testing.T) {
	ctx, c := sessionAffinityTestContext(t, 7, "root")
	state, ok := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	require.True(t, ok)
	state.strict.Store(true)
	c.Request.URL.Path = "/v1/responses/compact"
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAICompactModel = "gpt-5.6-luna"
	body := []byte(`{"model":"gpt-6-astra","input":[]}`)
	_, _, retry := svc.prepareOpenAICompactFallbackRetry(c, nil, "gpt-6-astra", body, 400, "", []byte(`{"response":{"status":"failed"}}`), false)
	require.False(t, retry, "an empty failed shell does not establish nonexecution")
	_, _, retry = svc.prepareOpenAICompactFallbackRetry(c, nil, "gpt-6-astra", body, 404, "model not found", []byte(`{"error":{"code":"model_not_found"}}`), false)
	require.True(t, retry, "an explicit pre-execution rejection retains same-account compatibility repair")
}
