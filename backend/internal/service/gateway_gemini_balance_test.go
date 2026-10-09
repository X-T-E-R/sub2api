//go:build unit

package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// Keep the load snapshot fixed while enforcing real slot capacity. This models
// concurrent selectors sharing the load-batch TTL, not an always-successful slot.
type geminiBalanceConcurrencyCache struct {
	mockConcurrencyCache
	mu    sync.Mutex
	slots map[int64]map[string]struct{}
	peak  map[int64]int
}

func (m *geminiBalanceConcurrencyCache) AcquireAccountSlot(_ context.Context, id int64, limit int, requestID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slots == nil {
		m.slots = make(map[int64]map[string]struct{})
		m.peak = make(map[int64]int)
	}
	if len(m.slots[id]) >= limit {
		return false, nil
	}
	if m.slots[id] == nil {
		m.slots[id] = make(map[string]struct{})
	}
	m.slots[id][requestID] = struct{}{}
	if len(m.slots[id]) > m.peak[id] {
		m.peak[id] = len(m.slots[id])
	}
	return true, nil
}

func (m *geminiBalanceConcurrencyCache) ReleaseAccountSlot(_ context.Context, id int64, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.slots[id], requestID)
	return nil
}

// The embedded cache supplies the unused gateway methods; sticky operations are
// synchronized because different session keys are written by concurrent requests.
type geminiBalanceStickyCache struct {
	mockGatewayCacheForPlatform
	mu sync.Mutex
}

func (m *geminiBalanceStickyCache) GetSessionAccountID(ctx context.Context, groupID int64, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockGatewayCacheForPlatform.GetSessionAccountID(ctx, groupID, key)
}
func (m *geminiBalanceStickyCache) SetSessionAccountID(ctx context.Context, groupID int64, key string, id int64, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockGatewayCacheForPlatform.SetSessionAccountID(ctx, groupID, key, id, ttl)
}
func (m *geminiBalanceStickyCache) DeleteSessionAccountID(ctx context.Context, groupID int64, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mockGatewayCacheForPlatform.DeleteSessionAccountID(ctx, groupID, key)
}

func newGeminiBalanceFixture(platform string, concurrency int) (*GatewayService, *geminiBalanceConcurrencyCache, context.Context) {
	old := time.Now().Add(-time.Hour)
	recent := old.Add(time.Minute)
	repo := &mockAccountRepoForPlatform{accounts: []Account{
		{ID: 1, Platform: platform, Type: AccountTypeOAuth, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: concurrency, LastUsedAt: &old},
		{ID: 2, Platform: platform, Type: AccountTypeOAuth, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: concurrency, LastUsedAt: &recent},
	}}
	cache := &geminiBalanceConcurrencyCache{}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	svc := &GatewayService{accountRepo: repo, cache: &geminiBalanceStickyCache{}, cfg: cfg, concurrencyService: NewConcurrencyService(cache)}
	// Deterministically retain the same snapshot for the entire burst.
	svc.concurrencyService.SetAccountLoadBatchCacheTTL(time.Hour)
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, platform)
	return svc, cache, ctx
}

func TestGeminiNewSessionsDisperseWithStaleSnapshot(t *testing.T) {
	for _, platform := range []string{PlatformGemini, PlatformAntigravity} {
		t.Run(platform, func(t *testing.T) {
			for _, unevenLoad := range []bool{false, true} {
				t.Run(fmt.Sprintf("uneven_load_%t", unevenLoad), func(t *testing.T) {
					svc, slots, ctx := newGeminiBalanceFixture(platform, 256)
					if unevenLoad {
						slots.loadMap = map[int64]*AccountLoadInfo{1: {AccountID: 1, LoadRate: 10}, 2: {AccountID: 2, LoadRate: 20}}
					}
					const n = 128
					results := make([]*AccountSelectionResult, n)
					errs := make([]error, n)
					var wg sync.WaitGroup
					for i := range results {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							results[i], errs[i] = svc.SelectAccountWithLoadAwareness(ctx, nil, fmt.Sprintf("session-%d", i), "gemini-2.5-pro", nil, "", 0)
						}(i)
					}
					wg.Wait()
					counts := map[int64]int{}
					for i, result := range results {
						require.NoError(t, errs[i])
						require.True(t, result.Acquired)
						counts[result.Account.ID]++
						result.ReleaseFunc()
					}
					// This is a herd regression, not a claim of exact round-robin fairness.
					// The old deterministic min-load/LRU selector sends all 128 to ID 1.
					require.Len(t, counts, 2, "new sessions must not all select the same stale-snapshot winner")
					require.Equal(t, 1, slots.loadBatchCalls)
					for _, active := range slots.slots {
						require.Empty(t, active)
					}
					t.Logf("new session distribution: %v", counts)
				})
			}
		})
	}
}

func TestGeminiBalancePreservesStickyAndSlotCapacity(t *testing.T) {
	svc, slots, ctx := newGeminiBalanceFixture(PlatformGemini, 2)
	first, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "same-session", "gemini-2.5-pro", nil, "", 0)
	require.NoError(t, err)
	second, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "same-session", "gemini-2.5-pro", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, first.Account.ID, second.Account.ID)
	require.Equal(t, 1, slots.loadBatchCalls, "sticky must return before the load-balancing layer")
	third, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "same-session", "gemini-2.5-pro", nil, "", 0)
	require.NoError(t, err)
	require.False(t, third.Acquired)
	require.Equal(t, first.Account.ID, third.WaitPlan.AccountID, "full sticky account uses existing sticky wait plan")

	// New sessions must escape a full account even when the snapshot says zero.
	newSession, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "another-session", "gemini-2.5-pro", nil, "", 0)
	require.NoError(t, err)
	require.True(t, newSession.Acquired)
	require.NotEqual(t, first.Account.ID, newSession.Account.ID)
	first.ReleaseFunc()
	second.ReleaseFunc()
	newSession.ReleaseFunc()
	for id, active := range slots.slots {
		require.Empty(t, active)
		require.LessOrEqual(t, slots.peak[id], 2)
	}
}

func TestGeminiCapacityHeadroomWeights(t *testing.T) {
	factor := 40
	pool := []accountWithLoad{
		{account: &Account{ID: 1, Concurrency: 10}, loadInfo: &AccountLoadInfo{LoadRate: 0}},
		{account: &Account{ID: 2, Concurrency: 10, LoadFactor: &factor}, loadInfo: &AccountLoadInfo{LoadRate: 50}},
		{account: &Account{ID: 3, Concurrency: 100}, loadInfo: &AccountLoadInfo{LoadRate: 100}},
	}
	// Headroom 10:20:0 respects the existing scheduling LoadFactor override,
	// not the hard concurrency ceiling. Exact draws check the weight boundary.
	require.Equal(t, int64(1), selectByCapacityHeadroom(pool, false, 0.3).account.ID)
	require.Equal(t, int64(2), selectByCapacityHeadroom(pool, false, 0.4).account.ID)
	require.Equal(t, int64(2), selectByCapacityHeadroom(pool, false, 0.99).account.ID)
	require.Nil(t, selectByCapacityHeadroom(pool[2:], false, 0.5))
	require.Nil(t, selectByCapacityHeadroom(nil, false, 0.5))

	pool[0].loadInfo.LoadRate = 90
	pool[1].account.LoadFactor = nil
	pool[1].loadInfo.LoadRate = 0
	// Equal capacity, 90% vs 0% load: 1:10, so even a high stale load
	// reduces weight instead of deterministically sending every request to ID 2.
	require.Equal(t, int64(1), selectByCapacityHeadroom(pool, false, 0.05).account.ID)
	require.Equal(t, int64(2), selectByCapacityHeadroom(pool, false, 0.1).account.ID)
}

func TestGeminiBalanceRespectsPriorityResetAndFallback(t *testing.T) {
	for _, loadError := range []bool{false, true} {
		t.Run(fmt.Sprintf("load_error_%t", loadError), func(t *testing.T) {
			svc, slots, ctx := newGeminiBalanceFixture(PlatformGemini, 128)
			repo := svc.accountRepo.(*mockAccountRepoForPlatform)
			repo.accounts[1].Priority = 2
			if loadError {
				slots.loadBatchErr = fmt.Errorf("load backend unavailable")
			}
			for i := 0; i < 12; i++ {
				result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, fmt.Sprintf("priority-%d", i), "gemini-2.5-pro", nil, "", 0)
				require.NoError(t, err)
				require.Equal(t, int64(1), result.Account.ID, "explicit account priority must remain strict")
				result.ReleaseFunc()
			}
			// The load-error fallback must also avoid the fixed oldest winner
			// once both accounts have the same configured priority.
			repo.accounts[1].Priority = 1
			seen := map[int64]bool{}
			for i := 0; i < 128; i++ {
				result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, fmt.Sprintf("fallback-%d", i), "gemini-2.5-pro", nil, "", 0)
				require.NoError(t, err)
				seen[result.Account.ID] = true
				result.ReleaseFunc()
			}
			require.Len(t, seen, 2)
		})
	}
	svc, _, ctx := newGeminiBalanceFixture(PlatformGemini, 128)
	repo := svc.accountRepo.(*mockAccountRepoForPlatform)
	later := time.Now().Add(2 * time.Hour)
	sooner := later.Add(-time.Hour)
	repo.accounts[0].SessionWindowEnd = &later
	repo.accounts[1].SessionWindowEnd = &sooner
	svc.cfg.Gateway.Scheduling.PreferSoonestReset = true
	for i := 0; i < 12; i++ {
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, fmt.Sprintf("reset-%d", i), "gemini-2.5-pro", nil, "", 0)
		require.NoError(t, err)
		require.Equal(t, int64(2), result.Account.ID)
		result.ReleaseFunc()
	}
}

func TestGeminiBalanceUnavailableBindingsAndExclusions(t *testing.T) {
	for _, gate := range []string{"disabled", "cooldown", "model", "quota", "excluded"} {
		t.Run(gate, func(t *testing.T) {
			svc, slots, ctx := newGeminiBalanceFixture(PlatformAntigravity, 2)
			repo := svc.accountRepo.(*mockAccountRepoForPlatform)
			sticky := svc.cache.(*geminiBalanceStickyCache)
			require.NoError(t, sticky.SetSessionAccountID(ctx, 0, "bound", 1, time.Hour))
			var excluded map[int64]struct{}
			switch gate {
			case "disabled":
				repo.accounts[0].Schedulable = false
			case "cooldown":
				until := time.Now().Add(time.Hour)
				repo.accounts[0].RateLimitResetAt = &until
			case "model":
				repo.accounts[0].Extra = map[string]any{modelRateLimitsKey: map[string]any{
					antigravityGeminiModelRateLimitKey: map[string]any{"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
				}}
			case "quota":
				repo.accounts[0].Type = AccountTypeAPIKey
				repo.accounts[0].Extra = map[string]any{"quota_limit": 10.0, "quota_used": 10.0}
			case "excluded":
				excluded = map[int64]struct{}{1: {}}
			}
			result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "bound", "gemini-2.5-pro", excluded, "", 0)
			require.NoError(t, err)
			require.Equal(t, int64(2), result.Account.ID)
			binding, err := sticky.GetSessionAccountID(ctx, 0, "bound")
			require.NoError(t, err)
			require.Equal(t, int64(2), binding)
			require.Empty(t, slots.slots[1], "unavailable/model-limited/quota-limited accounts must never acquire a slot")
			result.ReleaseFunc()
			require.Empty(t, slots.slots[2])
		})
	}
}

func TestGeminiBalanceAllSlotsFullWaitsAndReleases(t *testing.T) {
	svc, slots, ctx := newGeminiBalanceFixture(PlatformGemini, 2)
	results := make([]*AccountSelectionResult, 12)
	for i := range results {
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, fmt.Sprintf("full-%d", i), "gemini-2.5-pro", nil, "", 0)
		require.NoError(t, err)
		results[i] = result
	}
	acquired := 0
	for _, result := range results {
		if result.Acquired {
			acquired++
			result.ReleaseFunc()
		} else {
			require.NotNil(t, result.WaitPlan)
			require.Nil(t, result.ReleaseFunc)
			require.Equal(t, 2, result.WaitPlan.MaxConcurrency)
		}
	}
	require.Equal(t, 4, acquired, "atomic admission must enforce capacity even with a stale zero-load snapshot")
	for id, active := range slots.slots {
		require.Empty(t, active)
		require.LessOrEqual(t, slots.peak[id], 2)
	}
}

func TestGatewaySelectionHydrationFailureReleasesSlot(t *testing.T) {
	svc := &GatewayService{schedulerSnapshot: NewSchedulerSnapshotService(&snapshotHydrationCache{}, nil, &mockAccountRepoForPlatform{}, nil, nil)}
	released := 0
	selection, err := svc.newSelectionResult(context.Background(), &Account{ID: 99}, true, func() { released++ }, nil)
	require.Error(t, err)
	require.Nil(t, selection)
	require.Equal(t, 1, released)
}
