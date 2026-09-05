//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type antigravityIsolationCache struct {
	tokens map[string]string
	locks  map[string]bool
}

func newAntigravityIsolationCache() *antigravityIsolationCache {
	return &antigravityIsolationCache{tokens: map[string]string{}, locks: map[string]bool{}}
}

func (c *antigravityIsolationCache) GetAccessToken(_ context.Context, key string) (string, error) {
	return c.tokens[key], nil
}
func (c *antigravityIsolationCache) SetAccessToken(_ context.Context, key, token string, _ time.Duration) error {
	c.tokens[key] = token
	return nil
}
func (c *antigravityIsolationCache) DeleteAccessToken(_ context.Context, key string) error {
	delete(c.tokens, key)
	return nil
}
func (c *antigravityIsolationCache) AcquireRefreshLock(_ context.Context, key string, _ time.Duration) (bool, error) {
	if c.locks[key] {
		return false, nil
	}
	c.locks[key] = true
	return true, nil
}
func (c *antigravityIsolationCache) ReleaseRefreshLock(_ context.Context, key string) error {
	delete(c.locks, key)
	return nil
}

func antigravityIsolationAccount(id int64, token string) *Account {
	return &Account{ID: id, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{
		"project_id": "shared-project", "access_token": token, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}}
}

func TestAntigravityTokenProvider_AccountIsolation(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "a_then_b", true: "b_then_a"}[reverse], func(t *testing.T) {
			ctx := context.Background()
			cache := newAntigravityIsolationCache()
			provider := NewAntigravityTokenProvider(nil, cache, nil)
			a := antigravityIsolationAccount(801, "synthetic-a")
			b := antigravityIsolationAccount(802, "synthetic-b")
			a.Credentials["email"] = "a@example.test"
			b.Credentials["email"] = "b@example.test"
			accounts := []*Account{a, b}
			if reverse {
				accounts = []*Account{b, a}
			}
			for _, account := range accounts {
				token, err := provider.GetAccessToken(ctx, account)
				require.NoError(t, err)
				require.Equal(t, account.GetCredential("access_token"), token)
			}
			// Empty snapshots force subsequent reads to prove cache ownership.
			for _, account := range accounts {
				snapshot := *account
				snapshot.Credentials = map[string]any{"project_id": "shared-project"}
				token, err := provider.GetAccessToken(ctx, &snapshot)
				require.NoError(t, err)
				require.Equal(t, account.GetCredential("access_token"), token)
			}
			require.NoError(t, NewCompositeTokenCacheInvalidator(cache).InvalidateToken(ctx, a))
			require.Empty(t, cache.tokens[AntigravityTokenCacheKey(a)])
			require.Equal(t, "synthetic-b", cache.tokens[AntigravityTokenCacheKey(b)])
			a.Credentials["access_token"] = "synthetic-a-refreshed"
			a.Credentials["project_id"] = "backfilled-project"
			token, err := provider.GetAccessToken(ctx, a)
			require.NoError(t, err)
			require.Equal(t, "synthetic-a-refreshed", token)
		})
	}
}

func TestAntigravityTokenProvider_IgnoresLegacyCache(t *testing.T) {
	for _, project := range []string{"shared-project", ""} {
		cache := newAntigravityIsolationCache()
		cache.tokens["ag:shared-project"] = "legacy-other-account"
		cache.tokens["ag:account:801"] = "legacy-fallback"
		account := antigravityIsolationAccount(801, "selected-account")
		account.Credentials["project_id"] = project
		token, err := NewAntigravityTokenProvider(nil, cache, nil).GetAccessToken(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, "selected-account", token)
	}
}

func TestAntigravityTokenProvider_StaleSnapshotNotCached(t *testing.T) {
	old := antigravityIsolationAccount(801, "old-token")
	latest := antigravityIsolationAccount(801, "latest-token")
	latest.Credentials["_token_version"] = int64(2)
	cache := newAntigravityIsolationCache()
	repo := &refreshAPIAccountRepo{account: latest}
	token, err := NewAntigravityTokenProvider(repo, cache, nil).GetAccessToken(context.Background(), old)
	require.NoError(t, err)
	require.Equal(t, "latest-token", token)
	require.Empty(t, cache.tokens)
}

// Keep the real Antigravity key/eligibility/window methods; replace only the
// external OAuth call with deterministic synthetic credentials.
type antigravityIsolationExecutor struct {
	AntigravityTokenRefresher
	refreshCalls int
}

func (e *antigravityIsolationExecutor) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	e.refreshCalls++
	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = "refreshed-selected-account"
	credentials["project_id"] = "discovered-project"
	credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return credentials, nil
}

func TestAntigravityTokenProvider_RefreshLockIsolation(t *testing.T) {
	ctx := context.Background()
	cache := newAntigravityIsolationCache()
	a := antigravityIsolationAccount(801, "synthetic-a")
	b := antigravityIsolationAccount(802, "synthetic-b")
	b.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
	repo := &refreshAPIAccountRepo{account: b}
	executor := &antigravityIsolationExecutor{}
	keyA, keyB := executor.CacheKey(a), executor.CacheKey(b)
	require.NotEqual(t, keyA, keyB)
	acquired, err := cache.AcquireRefreshLock(ctx, keyA, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	api := NewOAuthRefreshAPI(repo, cache)
	// In-process serialization also uses the isolated executor key.
	require.NotSame(t, api.getLocalLock(keyA), api.getLocalLock(keyB))
	provider := NewAntigravityTokenProvider(repo, cache, nil)
	provider.SetRefreshAPI(api, executor)
	token, err := provider.GetAccessToken(ctx, b)
	require.NoError(t, err)
	require.Equal(t, "refreshed-selected-account", token)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, 1, repo.updateCredentialsCalls)
	require.Equal(t, keyB, executor.CacheKey(repo.account))
	require.Equal(t, token, cache.tokens[keyB])
	require.True(t, cache.locks[keyA], "B must not release A's lock")
	require.False(t, cache.locks[keyB], "B's lock must be released after refresh")
	result, err := api.RefreshIfNeeded(ctx, a, executor, time.Minute)
	require.NoError(t, err)
	require.True(t, result.LockHeld, "the same account must still respect its held lock")
	require.Equal(t, 1, executor.refreshCalls)
}

func TestAntigravityTokenProvider_MissingTokenDoesNotBorrowLegacy(t *testing.T) {
	cache := newAntigravityIsolationCache()
	cache.tokens["ag:shared-project"] = "legacy-other-account"
	account := antigravityIsolationAccount(801, "   ")
	token, err := NewAntigravityTokenProvider(nil, cache, nil).GetAccessToken(context.Background(), account)
	require.ErrorContains(t, err, "access_token not found")
	require.Empty(t, token)
	require.Len(t, cache.tokens, 1, "missing credentials must not populate a new token entry")
}

func TestAntigravityTokenProvider_ProjectBackfillKeepsCacheScope(t *testing.T) {
	ctx := context.Background()
	cache := newAntigravityIsolationCache()
	account := antigravityIsolationAccount(801, "before-backfill")
	delete(account.Credentials, "project_id")
	provider := NewAntigravityTokenProvider(nil, cache, nil)
	token, err := provider.GetAccessToken(ctx, account)
	require.NoError(t, err)
	require.Equal(t, "before-backfill", token)
	key := AntigravityTokenCacheKey(account)
	account.Credentials["project_id"] = "newly-discovered-project"
	account.Credentials["access_token"] = "after-rotation"
	account.Credentials["refresh_token"] = "rotated-refresh-token"
	account.Credentials["email"] = "changed@example.test"
	require.Equal(t, key, AntigravityTokenCacheKey(account))
	require.NoError(t, NewCompositeTokenCacheInvalidator(cache).InvalidateToken(ctx, account))
	require.Empty(t, cache.tokens)
	token, err = provider.GetAccessToken(ctx, account)
	require.NoError(t, err)
	require.Equal(t, "after-rotation", token)
	require.Len(t, cache.tokens, 1)
}

func TestAntigravityTokenProvider_GetAccessToken_Upstream(t *testing.T) {
	provider := &AntigravityTokenProvider{}

	t.Run("upstream account with valid api_key", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAntigravity,
			Type:     AccountTypeUpstream,
			Credentials: map[string]any{
				"api_key": "sk-test-key-12345",
			},
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, "sk-test-key-12345", token)
	})

	t.Run("upstream account missing api_key", func(t *testing.T) {
		account := &Account{
			Platform:    PlatformAntigravity,
			Type:        AccountTypeUpstream,
			Credentials: map[string]any{},
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "upstream account missing api_key")
		require.Empty(t, token)
	})

	t.Run("upstream account with empty api_key", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAntigravity,
			Type:     AccountTypeUpstream,
			Credentials: map[string]any{
				"api_key": "",
			},
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "upstream account missing api_key")
		require.Empty(t, token)
	})

	t.Run("upstream account with nil credentials", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAntigravity,
			Type:     AccountTypeUpstream,
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "upstream account missing api_key")
		require.Empty(t, token)
	})
}

func TestAntigravityTokenProvider_GetAccessToken_Guards(t *testing.T) {
	provider := &AntigravityTokenProvider{}

	t.Run("nil account", func(t *testing.T) {
		token, err := provider.GetAccessToken(context.Background(), nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "account is nil")
		require.Empty(t, token)
	})

	t.Run("non-antigravity platform", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not an antigravity account")
		require.Empty(t, token)
	})

	t.Run("unsupported account type", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAntigravity,
			Type:     AccountTypeAPIKey,
		}
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not an antigravity oauth account")
		require.Empty(t, token)
	})
}
