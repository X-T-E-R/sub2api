package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This fixture only checks gateway consumption. Atomicity is independently
// exercised against PostgreSQL in repository/codex_daily_session_pool_postgres_test.go.
type dailyPoolFixtureRepo struct {
	mu                 sync.Mutex
	bindings           map[string]string
	reads, allocations int
	err                error
	days               []string
}

func (r *dailyPoolFixtureRepo) FindBinding(_ context.Context, scope, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return r.bindings[scope+":"+key], r.err
}
func (r *dailyPoolFixtureRepo) Allocate(_ context.Context, scope, key, day string, _ int64, _, _ int) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	if r.bindings == nil {
		r.bindings = make(map[string]string)
	}
	if existing := r.bindings[scope+":"+key]; existing != "" {
		return existing, nil
	}
	value := uuid.NewString()
	r.bindings[scope+":"+key] = value
	r.allocations++
	r.days = append(r.days, day)
	return value, nil
}
func (*dailyPoolFixtureRepo) CleanupDays(context.Context, string, int) (int64, error) { return 0, nil }
func dailyPoolAccount() *Account {
	a := newTestOAuthAccount(101, map[string]any{codexFingerprintModeExtraKey: "session", CodexDailySessionEnabledKey: true, CodexDailySessionMinKey: 5, CodexDailySessionMaxKey: 10})
	a.Credentials = map[string]any{"chatgpt_account_id": "credential-A", "chatgpt_user_id": "user-A", "access_token": "fixture-token"}
	a.Concurrency = 1
	return a
}
func fixtureDailyPool(t *testing.T, repo *dailyPoolFixtureRepo) *CodexDailySessionPool {
	t.Helper()
	pool := NewCodexDailySessionPool(repo, &config.Config{Timezone: "Asia/Shanghai"})
	t.Cleanup(pool.cache.Close)
	return pool
}

func TestCodexDailyPoolSettingsAndAliases(t *testing.T) {
	base := dailyPoolAccount()
	for _, tc := range []struct {
		name    string
		values  map[string]any
		invalid bool
	}{
		{"valid", nil, false}, {"fixed", map[string]any{CodexDailySessionMaxKey: 5}, false},
		{"boolean string", map[string]any{CodexDailySessionEnabledKey: "true"}, true},
		{"fraction", map[string]any{CodexDailySessionMinKey: 1.5}, true},
		{"zero", map[string]any{CodexDailySessionMinKey: 0}, true},
		{"too large", map[string]any{CodexDailySessionMaxKey: 1001}, true},
		{"reversed", map[string]any{CodexDailySessionMinKey: 11}, true},
		{"missing max", map[string]any{CodexDailySessionMaxKey: nil}, true},
		{"wrong mode", map[string]any{codexFingerprintModeExtraKey: "device"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := *base
			a.Extra = maps.Clone(base.Extra)
			maps.Copy(a.Extra, tc.values)
			_, _, _, err := CodexDailySessionPolicy(&a)
			require.Equal(t, tc.invalid, err != nil)
		})
	}
	plain := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	enabled, _, _, err := CodexDailySessionPolicy(plain)
	require.NoError(t, err)
	require.False(t, enabled)
	alias := *base
	alias.ID = 102
	alias.Extra = maps.Clone(base.Extra)
	require.NoError(t, ValidateCodexDailySessionAliases([]*Account{base, &alias}))
	alias.Extra[CodexDailySessionMaxKey] = 20
	require.ErrorContains(t, ValidateCodexDailySessionAliases([]*Account{base, &alias}), "identical daily session")
	alias.Credentials = map[string]any{"chatgpt_account_id": "credential-B", "chatgpt_user_id": "user-A"}
	require.NoError(t, ValidateCodexDailySessionAliases([]*Account{base, &alias}))
	for _, credentials := range []map[string]any{{"access_token": "refreshable"}, {"chatgpt_account_id": "account-only"}} {
		incomplete := *base
		incomplete.Credentials = credentials
		_, _, _, err := CodexDailySessionPolicy(&incomplete)
		require.ErrorContains(t, err, "complete ChatGPT account and user IDs")
	}
	setup := *base
	setup.Type = AccountTypeSetupToken
	setup.Credentials = map[string]any{"access_token": "stable-setup-bearer"}
	_, _, _, err = CodexDailySessionPolicy(&setup)
	require.NoError(t, err)
	otherUser := *base
	otherUser.Credentials = maps.Clone(base.Credentials)
	otherUser.Credentials["chatgpt_user_id"] = "another-user"
	require.NotEqual(t, CodexDailySessionScope(base), CodexDailySessionScope(&otherUser))
}

func TestCodexDailyPoolInvalidCacheEntryUsesDurableBinding(t *testing.T) {
	for _, invalid := range []any{42, ""} {
		repo := &dailyPoolFixtureRepo{}
		pool := fixtureDailyPool(t, repo)
		account := dailyPoolAccount()
		key := codexPoolDigest("codex-daily-account:v1:credential") + ":" + codexPoolDigest("codex-daily-root:v1:77:root")
		repo.bindings = map[string]string{key: "durable-session"}
		require.True(t, pool.cache.Set(key, invalid, 1))
		pool.cache.Wait()
		session, err := pool.resolve(context.Background(), "credential", 77, "root", account)
		require.NoError(t, err)
		require.Equal(t, "durable-session", session)
		require.Equal(t, 1, repo.reads)
		require.Zero(t, repo.allocations)
		pool.cache.Wait()
		require.True(t, pool.cache.Set(key, invalid, 1))
		pool.cache.Wait()
		repo.err = errors.New("storage unavailable")
		session, err = pool.resolve(context.Background(), "credential", 77, "root", account)
		require.ErrorIs(t, err, repo.err)
		require.Empty(t, session)
		require.Zero(t, repo.allocations)
	}
}

func TestCodexDailyPoolSetupTokenIdentityShapes(t *testing.T) {
	for _, shape := range []struct {
		name, accountID, userID string
		valid                   bool
	}{
		{"absent", "", "", true}, {"account-only", "account", "", false},
		{"user-only", "", "user", false}, {"complete", "account", "user", true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			account := dailyPoolAccount()
			account.Type = AccountTypeSetupToken
			account.Credentials = map[string]any{"access_token": "stable-bearer", "chatgpt_account_id": shape.accountID, "chatgpt_user_id": shape.userID}
			_, _, _, err := CodexDailySessionPolicy(account)
			if shape.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "complete ChatGPT account and user IDs")
			}
		})
	}
}

func TestCodexDailyPoolProjectionBindingAndModeChanges(t *testing.T) {
	repo := &dailyPoolFixtureRepo{}
	pool := fixtureDailyPool(t, repo)
	pool.now = func() time.Time { return time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC) }
	svc := &OpenAIGatewayService{codexDailySessionPool: pool}
	account := dailyPoolAccount()
	project := func(a *Account, key int64, root, thread string) (map[string]any, map[string]any) {
		c := identityContext(t, nil)
		c.Set("api_key", &APIKey{ID: key})
		_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, a)
		require.NoError(t, err)
		body := identityFixture(t, root, thread, "turn-"+thread, 1, nil)
		_, err = projectCodexRequestBodyWithDailySession(c, a, body)
		require.NoError(t, err)
		h := http.Header{}
		applyCodexRequestHeaders(c, a, h)
		m := projectedMetadata(t, body)
		require.Equal(t, m["session_id"], h.Get("session-id"))
		return m, body
	}
	root, rootBody := project(account, 77, "root-A", "root-A")
	child, childBody := project(account, 77, "root-A", "child-A")
	require.Equal(t, root["session_id"], child["session_id"])
	require.NotEqual(t, root["thread_id"], child["thread_id"])
	require.Equal(t, rootBody["prompt_cache_key"], childBody["prompt_cache_key"])
	require.Equal(t, []string{"2026-09-06"}, repo.days, "server timezone owns the allocation date")
	pool.cache.Wait()
	reads := repo.reads
	project(account, 77, "root-A", "child-B")
	require.Equal(t, reads, repo.reads, "positive hit does not read/write storage")
	alias := *account
	alias.ID = 999
	repeated, _ := project(&alias, 77, "root-A", "root-A")
	require.Equal(t, root["session_id"], repeated["session_id"])
	otherKey, otherKeyBody := project(account, 78, "root-A", "root-A")
	require.NotEqual(t, rootBody["prompt_cache_key"], otherKeyBody["prompt_cache_key"])
	require.NotEqual(t, root["thread_id"], otherKey["thread_id"])
	require.Equal(t, 2, repo.allocations, "downstream API key is part of the binding key")
	pool.cache.Clear()
	pool.now = func() time.Time { return time.Date(2026, 9, 7, 1, 0, 0, 0, time.UTC) }
	account.Extra[CodexDailySessionEnabledKey] = false
	resumed, _ := project(account, 77, "root-A", "root-A")
	require.Equal(t, root["session_id"], resumed["session_id"])
	pool.cache.Clear()
	account.Extra[CodexDailySessionMinKey] = 0
	resumed, _ = project(account, 77, "root-A", "root-A")
	require.Equal(t, root["session_id"], resumed["session_id"], "durable lookup precedes even malformed later policy")
	account.Extra[CodexDailySessionMinKey] = 5
	project(account, 77, "new-disabled", "new-disabled")
	require.Equal(t, 2, repo.allocations)
	account.Extra[codexFingerprintModeExtraKey] = "off"
	repo.err = errors.New("storage down")
	off, _ := project(account, 77, "root-A", "root-A")
	require.NotEqual(t, root["session_id"], off["session_id"], "mode switch restores its own projection policy")
}

func TestCodexDailyPoolActualHTTPAndCompatibilityRoutes(t *testing.T) {
	repo := &dailyPoolFixtureRepo{}
	pool := fixtureDailyPool(t, repo)
	account := dailyPoolAccount()
	var session, thread, cache string
	for _, route := range []string{"http", "raw", "compact", "compact-raw", "messages", "chat", "alpha"} {
		t.Run(route, func(t *testing.T) {
			account.Extra["openai_oauth_passthrough"] = strings.Contains(route, "raw")
			c := identityContext(t, nil)
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
			c.Request.Header.Set("session-id", "root-A")
			c.Request.Header.Set("thread-id", "child-A")
			c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"root-A","thread_id":"child-A","turn_id":"turn-A"}`)
			body := identityFixture(t, "root-A", "child-A", "turn-A", 1, nil)
			body["model"], body["input"], body["instructions"] = "gpt-5.4", []any{}, "fixture"
			response := openAICompatSSECompletedResponse("resp_fixture", "gpt-5.4")
			if route == "alpha" {
				body["id"] = "standalone-search-not-root"
				response = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":"fixture"}`))}
			}
			if strings.HasPrefix(route, "compact") {
				c.Request.URL.Path = "/v1/responses/compact"
				delete(body, "client_metadata")
				response = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(compactProbeSSESuccessBody))}
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{resp: response}
			svc := &OpenAIGatewayService{codexDailySessionPool: pool, httpUpstream: upstream, cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
			switch route {
			case "messages", "chat":
				raw = []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"fixture"}],"stream":false}`)
				switch route {
				case "messages":
					_, err = svc.ForwardAsAnthropic(context.Background(), c, account, raw, "root-A", "gpt-5.4")
				case "chat":
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, raw, "root-A", "gpt-5.4")
				}
			case "alpha":
				_, err = svc.ForwardAlphaSearch(context.Background(), c, account, raw)
			default:
				_, err = svc.Forward(context.Background(), c, account, raw)
			}
			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)
			h := upstream.lastReq.Header
			if session == "" {
				session = h.Get("session-id")
				thread = h.Get("thread-id")
				cache = gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
			}
			require.NotEmpty(t, session)
			require.Equal(t, session, h.Get("session-id"))
			switch route {
			case "alpha":
				require.Empty(t, h.Get("session_id"), "standalone SearchClient protocol strips Responses-only headers")
				require.Equal(t, session, codexMetadataObject(h.Get(openAIWSTurnMetadataHeader))["session_id"])
			case "http", "raw":
				require.Empty(t, h.Get("session_id"), "native body metadata replaces the direct session_id alias")
			default:
				require.Equal(t, session, h.Get("session_id"))
			}
			require.Equal(t, thread, h.Get("thread-id"))
			if strings.HasPrefix(route, "compact") {
				require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
			}
			if route == "http" || route == "raw" {
				require.Equal(t, session, gjson.GetBytes(upstream.lastBody, "client_metadata.session_id").String())
				require.Equal(t, cache, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
			}
		})
	}
	require.Equal(t, 1, repo.allocations)
}

func TestCodexDailyPoolStorageFailureAndMissingRoot(t *testing.T) {
	repo := &dailyPoolFixtureRepo{err: errors.New("storage unavailable")}
	svc := &OpenAIGatewayService{codexDailySessionPool: fixtureDailyPool(t, repo)}
	account := dailyPoolAccount()
	for _, mode := range []string{"off", "device", "full", "session"} {
		a := *account
		a.Extra = maps.Clone(account.Extra)
		a.Extra[codexFingerprintModeExtraKey] = mode
		if mode != "session" {
			a.Extra[CodexDailySessionEnabledKey] = false
		}
		c := identityContext(t, nil)
		_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, &a)
		require.NoError(t, err)
		_, err = projectCodexRequestBodyWithDailySession(c, &a, identityFixture(t, "root", "child", "turn", 1, nil))
		if mode == "session" {
			require.ErrorContains(t, err, "storage unavailable")
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, repo.reads)
	c := identityContext(t, nil)
	_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, account)
	require.NoError(t, err)
	_, err = projectCodexRequestBodyWithDailySession(c, account, map[string]any{"input": []any{"not identity"}, "prompt_cache_key": "cache-is-not-root"})
	require.ErrorContains(t, err, "original root session_id")
	require.Zero(t, repo.allocations)
	for _, passthrough := range []bool{false, true} {
		upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_fixture", "gpt-5.4")}
		svc.httpUpstream, svc.cfg, svc.toolCorrector = upstream, &config.Config{}, NewCodexToolCorrector()
		account.Extra["openai_oauth_passthrough"] = passthrough
		c := identityContext(t, http.Header{"Session-Id": {"root"}})
		_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","instructions":"fixture","input":[]}`))
		require.ErrorContains(t, err, "storage unavailable")
		require.Nil(t, upstream.lastReq, "storage failure must abort before any upstream request or random fallback")
	}
}

func TestCodexDailyPoolShadowAndAccountABA(t *testing.T) {
	repo := &dailyPoolFixtureRepo{}
	parent := dailyPoolAccount()
	shadow := dailyPoolAccount()
	shadow.ID = 202
	shadow.ParentAccountID = &parent.ID
	shadow.Credentials = nil
	delete(shadow.Extra, CodexDailySessionEnabledKey)
	delete(shadow.Extra, CodexDailySessionMinKey)
	delete(shadow.Extra, CodexDailySessionMaxKey)
	other := dailyPoolAccount()
	other.ID = 303
	other.Credentials = map[string]any{"chatgpt_account_id": "credential-B", "chatgpt_user_id": "user-A"}
	svc := &OpenAIGatewayService{codexDailySessionPool: fixtureDailyPool(t, repo), accountRepo: &codexAccountIdentityRepoStub{account: parent}}
	c := identityContext(t, nil)
	project := func(account *Account) string {
		_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, account)
		require.NoError(t, err)
		body := identityFixture(t, "same-root", "child", "turn", 1, nil)
		_, err = projectCodexRequestBodyWithDailySession(c, account, body)
		require.NoError(t, err)
		session, ok := projectedMetadata(t, body)["session_id"].(string)
		require.True(t, ok)
		return session
	}
	a := project(parent)
	require.Equal(t, a, project(shadow), "shadow uses parent pool settings and actual credential scope")
	b := project(other)
	require.NotEqual(t, a, b)
	svc.codexDailySessionPool.cache.Clear()
	require.Equal(t, a, project(shadow), "A-B-A on one gin context restores durable A binding")
	require.Equal(t, 2, repo.allocations)
	shadow.Extra[codexFingerprintModeExtraKey] = "off"
	require.NotEqual(t, a, project(shadow), "parent pool does not opt shadow into session mode")
}
