//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyService_TouchLastUsed_InvalidKeyID(t *testing.T) {
	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			return errors.New("should not be called")
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	require.NoError(t, svc.TouchLastUsed(context.Background(), 0))
	require.NoError(t, svc.TouchLastUsed(context.Background(), -1))
	require.Empty(t, repo.touchedIDs)
}

func TestAPIKeyService_TouchLastUsed_FirstTouchSucceeds(t *testing.T) {
	repo := &apiKeyRepoStub{}
	svc := &APIKeyService{apiKeyRepo: repo}

	err := svc.TouchLastUsed(context.Background(), 123)
	require.NoError(t, err)
	require.Equal(t, []int64{123}, repo.touchedIDs)
	require.Len(t, repo.touchedUsedAts, 1)
	require.False(t, repo.touchedUsedAts[0].IsZero())

	cached, ok := svc.lastUsedTouchL1.Load(int64(123))
	require.True(t, ok, "successful touch should update debounce cache")
	_, isTime := cached.(time.Time)
	require.True(t, isTime)
}

func TestAPIKeyService_TouchLastUsed_DebouncedWithinWindow(t *testing.T) {
	repo := &apiKeyRepoStub{}
	svc := &APIKeyService{apiKeyRepo: repo}

	require.NoError(t, svc.TouchLastUsed(context.Background(), 123))
	require.NoError(t, svc.TouchLastUsed(context.Background(), 123))

	require.Equal(t, []int64{123}, repo.touchedIDs, "second touch within debounce window should not hit repository")
}

func TestAPIKeyService_TouchLastUsed_ExpiredDebounceTouchesAgain(t *testing.T) {
	repo := &apiKeyRepoStub{}
	svc := &APIKeyService{apiKeyRepo: repo}

	require.NoError(t, svc.TouchLastUsed(context.Background(), 123))

	// 强制将 debounce 时间回拨到窗口之外，触发第二次写库。
	svc.lastUsedTouchL1.Store(int64(123), time.Now().Add(-apiKeyLastUsedMinTouch-time.Second))

	require.NoError(t, svc.TouchLastUsed(context.Background(), 123))
	require.Len(t, repo.touchedIDs, 2)
	require.Equal(t, int64(123), repo.touchedIDs[0])
	require.Equal(t, int64(123), repo.touchedIDs[1])
}

func TestAPIKeyService_TouchLastUsed_RepoError(t *testing.T) {
	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			return errors.New("db write failed")
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	err := svc.TouchLastUsed(context.Background(), 123)
	require.Error(t, err)
	require.ErrorContains(t, err, "touch api key last used")
	require.Equal(t, []int64{123}, repo.touchedIDs)

	cached, ok := svc.lastUsedTouchL1.Load(int64(123))
	require.True(t, ok, "failed touch should still update retry debounce cache")
	_, isTime := cached.(time.Time)
	require.True(t, isTime)
}

func TestAPIKeyService_TouchLastUsed_RepoErrorDebounced(t *testing.T) {
	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			return errors.New("db write failed")
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	firstErr := svc.TouchLastUsed(context.Background(), 456)
	require.Error(t, firstErr)
	require.ErrorContains(t, firstErr, "touch api key last used")

	secondErr := svc.TouchLastUsed(context.Background(), 456)
	require.NoError(t, secondErr, "failed touch should be debounced and skip immediate retry")
	require.Equal(t, []int64{456}, repo.touchedIDs, "debounced retry should not hit repository again")
}

type touchSingleflightRepo struct {
	*apiKeyRepoStub
	mu      sync.Mutex
	calls   int
	blockCh chan struct{}
}

func (r *touchSingleflightRepo) UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	<-r.blockCh
	return nil
}

func TestAPIKeyService_TouchLastUsed_ConcurrentFirstTouchDeduplicated(t *testing.T) {
	repo := &touchSingleflightRepo{
		apiKeyRepoStub: &apiKeyRepoStub{},
		blockCh:        make(chan struct{}),
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	const workers = 20
	startCh := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			errCh <- svc.TouchLastUsed(context.Background(), 321)
		}()
	}

	close(startCh)

	require.Eventually(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return repo.calls >= 1
	}, time.Second, 10*time.Millisecond)

	close(repo.blockCh)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		require.NoError(t, err)
	}

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Equal(t, 1, repo.calls, "并发首次 touch 只应写库一次")
}

func TestAPIKeyService_TouchLastUsedAsync_NonBlockingAndBounded(t *testing.T) {
	blockCh := make(chan struct{})
	startedCh := make(chan struct{}, 2)
	var calls int
	var mu sync.Mutex

	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			mu.Lock()
			calls++
			mu.Unlock()
			select {
			case startedCh <- struct{}{}:
			default:
			}
			<-blockCh
			return nil
		},
	}
	svc := &APIKeyService{
		apiKeyRepo:         repo,
		lastUsedTouchSlots: make(chan struct{}, 1),
	}

	// 第一次异步 touch 占用唯一槽位并阻塞在慢 DB 上，但调用本身立即返回。
	doneCh := make(chan struct{})
	go func() {
		svc.TouchLastUsedAsync(101)
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TouchLastUsedAsync should not block caller")
	}

	select {
	case <-startedCh:
	case <-time.After(time.Second):
		t.Fatal("expected background touch to start")
	}

	// 槽位已满时，另一个 Key 的异步 touch 应立即丢弃返回，不阻塞也不排队堆积。
	svc.TouchLastUsedAsync(202)

	close(blockCh)
	require.Eventually(t, func() bool {
		return len(svc.lastUsedTouchSlots) == 0
	}, time.Second, 5*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls, "slot-full touch should be dropped without blocking")
}

func TestAPIKeyService_TouchLastUsedAsync_SameKeyDoesNotExhaustSlotsOrBlockOtherKeys(t *testing.T) {
	blockCh := make(chan struct{})
	startedIDs := make(chan int64, 4)

	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			startedIDs <- id
			<-blockCh
			return nil
		},
	}
	svc := &APIKeyService{
		apiKeyRepo:         repo,
		lastUsedTouchSlots: make(chan struct{}, 2),
	}

	// 同一 Key 在首次写库尚未完成前连续触发多次，只应占用 1 个并发槽位。
	for i := 0; i < 10; i++ {
		svc.TouchLastUsedAsync(101)
	}
	require.Eventually(t, func() bool {
		return len(svc.lastUsedTouchSlots) == 1
	}, time.Second, 5*time.Millisecond)

	// 另一个 Key 仍可正常获得剩余的第 2 个槽位，不会被同一 Key 的重复触发挤占。
	svc.TouchLastUsedAsync(202)
	require.Eventually(t, func() bool {
		return len(startedIDs) == 2
	}, time.Second, 5*time.Millisecond)

	close(blockCh)
	require.Eventually(t, func() bool {
		return len(svc.lastUsedTouchSlots) == 0
	}, time.Second, 5*time.Millisecond)
	require.Len(t, repo.touchedIDs, 2)
}

func TestAPIKeyService_TouchLastUsedAsync_StructLiteralInitializesBoundedSlots(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)

	repo := &apiKeyRepoStub{
		updateLastUsed: func(ctx context.Context, id int64, usedAt time.Time) error {
			<-blockCh
			return nil
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	for i := 1; i <= defaultLastUsedTouchConcurrency+8; i++ {
		svc.TouchLastUsedAsync(int64(i))
	}

	require.NotNil(t, svc.lastUsedTouchSlots)
	require.Equal(t, defaultLastUsedTouchConcurrency, cap(svc.lastUsedTouchSlots))
	require.Equal(t, defaultLastUsedTouchConcurrency, len(svc.lastUsedTouchSlots))
}
