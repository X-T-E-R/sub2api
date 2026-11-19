package handler

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWrapReleaseOnDone_NoGoroutineLeak 验证 wrapReleaseOnDone 修复后不会泄露 goroutine
func TestWrapReleaseOnDone_NoGoroutineLeak(t *testing.T) {
	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 正常释放
	release()

	require.Equal(t, int32(1), atomic.LoadInt32(&releaseCount))
	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= initialGoroutines+2
	}, time.Second, 5*time.Millisecond, "goroutine leak detected: initial=%d", initialGoroutines)
}

// TestWrapReleaseOnDone_ContextCancellation 验证 context 取消时也能正确释放
func TestWrapReleaseOnDone_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var releaseCount int32
	released := make(chan struct{}, 1)
	_ = wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
		select {
		case released <- struct{}{}:
		default:
		}
	})

	// 取消 context，应该触发释放
	cancel()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for release after context cancellation")
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&releaseCount))
}

func TestWrapReleaseOnDone_AlreadyCancelledReleasesExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})
	release()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&releaseCount) == 1
	}, time.Second, time.Millisecond)
}

// TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce 验证多次调用 release 只释放一次
func TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 调用多次
	release()
	release()
	release()

	// release 内部同步执行 sync.Once，无需 sleep 等待
	require.Equal(t, int32(1), atomic.LoadInt32(&releaseCount))
}

// TestWrapReleaseOnDone_NilReleaseFunc 验证 nil releaseFunc 不会 panic
func TestWrapReleaseOnDone_NilReleaseFunc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := wrapReleaseOnDone(ctx, nil)

	if release != nil {
		t.Error("expected nil release function when releaseFunc is nil")
	}
}

// TestWrapReleaseOnDone_ConcurrentCalls 验证并发调用的安全性
func TestWrapReleaseOnDone_ConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 并发调用 release，使用 WaitGroup 确定性等待全部完成
	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			release()
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), atomic.LoadInt32(&releaseCount))
}

// BenchmarkWrapReleaseOnDone 性能基准测试
func BenchmarkWrapReleaseOnDone(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release := wrapReleaseOnDone(ctx, func() {})
		release()
	}
}
