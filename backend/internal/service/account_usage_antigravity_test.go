//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

type antigravityUsageAccountRepoStub struct {
	AccountRepository
	account *Account
}

func (s *antigravityUsageAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if s.account == nil || s.account.ID != id {
		return nil, ErrAccountNotFound
	}
	return s.account, nil
}

func (s *antigravityUsageAccountRepoStub) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	if s.account == nil {
		return nil, nil
	}
	for _, id := range ids {
		if id == s.account.ID {
			return []*Account{s.account}, nil
		}
	}
	return nil, nil
}

func withAntigravityUsageBaseURL(t *testing.T, baseURL string) {
	t.Helper()
	originalBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	originalBaseURL := antigravity.BaseURL
	originalAvailability := antigravity.DefaultURLAvailability
	antigravity.BaseURLs = []string{baseURL}
	antigravity.BaseURL = baseURL
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
	t.Cleanup(func() {
		antigravity.BaseURLs = originalBaseURLs
		antigravity.BaseURL = originalBaseURL
		antigravity.DefaultURLAvailability = originalAvailability
	})
}

func TestAccountUsageServiceAntigravityForceBypassesCache(t *testing.T) {
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer token-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1internal:fetchAvailableModels"):
			call := modelCalls.Add(1)
			remaining := 0.8
			if call > 1 {
				remaining = 0.2
			}
			_, _ = fmt.Fprintf(w, `{"models":{"gemini-3.8-flash":{"displayName":"Gemini 3.8 Flash","quotaInfo":{"remainingFraction":%v,"resetTime":"2026-09-06T00:00:00Z"}}}}`, remaining)
		case strings.HasSuffix(r.URL.Path, "/v1internal:loadCodeAssist"):
			_, _ = w.Write([]byte(`{"paidTier":{"id":"g1-pro-tier","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"0","minimumCreditAmountForUsage":"5"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAntigravityUsageBaseURL(t, server.URL)

	account := &Account{
		ID:       71,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token": "token-secret",
			"project_id":   "project-secret",
		},
	}
	service := &AccountUsageService{
		accountRepo:             &antigravityUsageAccountRepoStub{account: account},
		antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil),
		cache:                   NewUsageCache(),
	}

	missing, err := service.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "passive", missing.Source)
	require.Equal(t, antigravityObservationUnavailable, missing.AntigravityQuotaState)
	require.Nil(t, missing.UpdatedAt)
	require.Zero(t, modelCalls.Load())

	first, err := service.GetUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "active", first.Source)
	require.Equal(t, antigravityObservationAvailable, first.AntigravityQuotaState)
	require.Equal(t, antigravityObservationAvailable, first.AntigravitySubscriptionState)
	require.Equal(t, "PRO", first.SubscriptionTier)
	require.Equal(t, 20, first.AntigravityQuota["gemini-3.8-flash"].Utilization)
	require.Len(t, first.AICredits, 1)
	require.NotNil(t, first.AICredits[0].Amount)
	require.Zero(t, *first.AICredits[0].Amount)

	passive, err := service.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "passive", passive.Source)
	require.False(t, passive.AntigravityQuotaStale)
	require.Equal(t, 20, passive.AntigravityQuota["gemini-3.8-flash"].Utilization)
	require.Equal(t, first.UpdatedAt, passive.UpdatedAt)
	require.EqualValues(t, 1, modelCalls.Load())

	cached, err := service.GetUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 20, cached.AntigravityQuota["gemini-3.8-flash"].Utilization)
	require.EqualValues(t, 1, modelCalls.Load())

	refreshed, err := service.GetUsage(context.Background(), account.ID, true)
	require.NoError(t, err)
	require.Equal(t, 80, refreshed.AntigravityQuota["gemini-3.8-flash"].Utilization)
	require.EqualValues(t, 2, modelCalls.Load())

	service.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{
		usageInfo: refreshed,
		timestamp: time.Now().Add(-apiCacheTTL - time.Second),
	})
	stale, err := service.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "passive", stale.Source)
	require.True(t, stale.AntigravityQuotaStale)
	require.Equal(t, 80, stale.AntigravityQuota["gemini-3.8-flash"].Utilization)
	require.EqualValues(t, 2, modelCalls.Load())

	encoded, err := json.Marshal(refreshed)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "token-secret")
	require.NotContains(t, string(encoded), "project-secret")
}

func TestBuildUsageInfoMissingRemainingFractionIsNotFullUtilization(t *testing.T) {
	var response antigravity.FetchAvailableModelsResponse
	require.NoError(t, json.Unmarshal([]byte(`{"models":{"gemini-3.8-flash":{"quotaInfo":{"resetTime":"2026-09-06T00:00:00Z"}}}}`), &response))

	info := (&AntigravityQuotaFetcher{}).buildUsageInfo(&response, "", "", nil)
	require.Empty(t, info.AntigravityQuota)
}

func TestAccountUsageBatchUsesPassiveAntigravitySnapshot(t *testing.T) {
	account := &Account{
		ID:       72,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token": "token-secret",
		},
	}
	service := &AccountUsageService{
		accountRepo: &antigravityUsageAccountRepoStub{account: account},
		cache:       NewUsageCache(),
	}

	usage, usageErrors, err := service.GetUsageBatch(context.Background(), []int64{account.ID}, true)
	require.NoError(t, err)
	require.Empty(t, usageErrors)
	require.Equal(t, "passive", usage[account.ID].Source)
	require.Equal(t, antigravityObservationUnavailable, usage[account.ID].AntigravityQuotaState)
}

func TestBuildAntigravityDegradedUsageIsExplicitlyUnavailable(t *testing.T) {
	usage := buildAntigravityDegradedUsage(fmt.Errorf("fetchAvailableModels failed (HTTP 429)"))

	require.Equal(t, "active", usage.Source)
	require.Equal(t, antigravityObservationUnavailable, usage.AntigravityQuotaState)
	require.Equal(t, antigravityObservationUnavailable, usage.AntigravitySubscriptionState)
	require.Equal(t, errorCodeRateLimited, usage.ErrorCode)
}
