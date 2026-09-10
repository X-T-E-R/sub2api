//go:build unit

package service

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexSessionAffinityTokenCacheDoesNotChangeIdentity(t *testing.T) {
	a := sessionAffinityTestAccount(11, "A")
	a.Credentials["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	ctx, _ := sessionAffinityTestContext(t, 7, "root")
	state, ok := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	require.True(t, ok)
	state.strict.Store(true)
	state.owner.Store(&CodexSessionAccountOwner{AccountScope: CodexDailySessionScope(&a), AccountID: a.ID})
	cache := newOpenAITokenCacheStub()
	cache.tokens[OpenAITokenCacheKey(&a)] = "different-credential-cached-token"
	provider := NewOpenAITokenProvider(nil, cache, nil)
	token, err := provider.GetAccessToken(ctx, &a)
	require.NoError(t, err)
	require.Equal(t, a.GetOpenAIAccessToken(), token)
	require.Zero(t, cache.getCalled, "strict warm path uses its credential snapshot, not row-keyed token cache")
	token, err = provider.GetAccessToken(context.Background(), &a)
	require.NoError(t, err)
	require.Equal(t, "different-credential-cached-token", token, "legacy cache behavior is unchanged")
}

func TestCodexSessionAffinityTokenVersionReplacement(t *testing.T) {
	a := sessionAffinityTestAccount(11, "A")
	a.Credentials["expires_at"] = time.Now().Add(time.Minute).Format(time.RFC3339)
	a.Credentials["refresh_token"] = "synthetic-refresh"
	a.Credentials["_token_version"] = int64(1)
	ctx, _ := sessionAffinityTestContext(t, 7, "root")
	state, ok := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	require.True(t, ok)
	state.strict.Store(true)
	state.owner.Store(&CodexSessionAccountOwner{AccountScope: CodexDailySessionScope(&a), AccountID: a.ID})
	for _, same := range []bool{false, true} {
		latest := a
		latest.Credentials = maps.Clone(a.Credentials)
		latest.Credentials["_token_version"] = int64(2)
		latest.Credentials["access_token"] = "new-token"
		if !same {
			latest.Credentials["chatgpt_account_id"] = "B"
		}
		repo := sessionOwnerAccountRepo{schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{latest}}}}
		provider := NewOpenAITokenProvider(repo, newOpenAITokenCacheStub(), nil)
		token, err := provider.GetAccessToken(ctx, &a)
		if same {
			require.NoError(t, err)
			require.Equal(t, "new-token", token)
		} else {
			require.ErrorIs(t, err, ErrCodexSessionAffinity)
			require.Empty(t, token)
		}
	}
}
