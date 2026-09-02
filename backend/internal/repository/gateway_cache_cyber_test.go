package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type cyberRedisCommandHook struct {
	mu            sync.Mutex
	mgetKeyCounts []int
	setBatchSizes []int
}

func (h *cyberRedisCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *cyberRedisCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "mget" {
			h.mu.Lock()
			h.mgetKeyCounts = append(h.mgetKeyCounts, len(cmd.Args())-1)
			h.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}

func (h *cyberRedisCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		setCount := 0
		for _, cmd := range cmds {
			if cmd.Name() == "set" {
				setCount++
			}
		}
		if setCount > 0 {
			h.mu.Lock()
			h.setBatchSizes = append(h.setBatchSizes, setCount)
			h.mu.Unlock()
		}
		return next(ctx, cmds)
	}
}

func TestGatewayCacheCyberBlockWritesScopeAndExactKeysTogether(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)

	ctx := context.Background()
	require.NoError(t, store.SetCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, "scope-1", "block-1", time.Minute))
	active, err := store.IsCyberSessionScopeActive(ctx, "scope-1")
	require.NoError(t, err)
	require.True(t, active)
	matched, ttl, err := store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, []string{"missing", "block-1"})
	require.NoError(t, err)
	require.Equal(t, "block-1", matched)
	require.Greater(t, ttl, time.Duration(0))
	require.Greater(t, server.TTL(cyberSessionScopePrefix+"scope-1"), time.Duration(0))
	require.Greater(t, server.TTL(cyberSessionTranscriptBlockPrefix+"block-1"), time.Duration(0))
	require.False(t, server.Exists("cyber_session_block:block-1"), "v1 prefix must remain untouched")
}

func TestGatewayCacheCyberBlockCommandsAreBoundedAndLookupShortCircuits(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &cyberRedisCommandHook{}
	client.AddHook(hook)
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)

	keys := make([]string, cyberSessionRedisCommandMaxKeys*2)
	for i := range keys {
		keys[i] = fmt.Sprintf("block-%d", i)
	}
	ctx := context.Background()
	require.NoError(t, store.SetCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, "large-scope", keys[cyberSessionRedisCommandMaxKeys+3], time.Minute))

	lookup := make([]string, len(keys))
	for i := range lookup {
		lookup[i] = fmt.Sprintf("missing-%d", i)
	}
	lookup[cyberSessionRedisCommandMaxKeys+3] = keys[cyberSessionRedisCommandMaxKeys+3]
	matched, _, err := store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, lookup)
	require.NoError(t, err)
	require.Equal(t, keys[cyberSessionRedisCommandMaxKeys+3], matched)
	require.Equal(t, []int{cyberSessionRedisCommandMaxKeys, cyberSessionRedisCommandMaxKeys}, hook.mgetKeyCounts)
}

func TestGatewayCacheCyberV2PrefixesAreExclusiveAndExpire(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)
	ctx := context.Background()

	require.NoError(t, client.Set(ctx, "cyber_session_block:same", "1", time.Minute).Err())
	matched, _, err := store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindExplicit, []string{"same"})
	require.NoError(t, err)
	require.Empty(t, matched, "v1 keys must be ignored")
	require.NoError(t, store.SetCyberSessionBlocked(ctx, service.CyberSessionBlockKindExplicit, "ignored", "same", time.Second))
	require.True(t, server.Exists(cyberSessionExplicitBlockPrefix+"same"))
	require.False(t, server.Exists(cyberSessionTranscriptBlockPrefix+"same"))
	require.False(t, server.Exists(cyberSessionScopePrefix+"ignored"), "explicit mode never writes scope")

	server.FastForward(2 * time.Second)
	matched, _, err = store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindExplicit, []string{"same"})
	require.NoError(t, err)
	require.Empty(t, matched)
}

func TestGatewayCacheCyberLookupSkipsExpiredAndNoTTLBeforeLaterMatch(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)
	ctx := context.Background()

	require.NoError(t, client.Set(ctx, cyberSessionTranscriptBlockPrefix+"expired", "1", time.Second).Err())
	require.NoError(t, client.Set(ctx, cyberSessionTranscriptBlockPrefix+"persistent", "1", 0).Err())
	require.NoError(t, client.Set(ctx, cyberSessionTranscriptBlockPrefix+"valid", "1", time.Minute).Err())
	server.FastForward(2 * time.Second)

	matched, ttl, err := store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, []string{"expired", "persistent", "valid"})
	require.NoError(t, err)
	require.Equal(t, "valid", matched)
	require.Greater(t, ttl, time.Duration(0))

	matched, ttl, err = store.FindCyberSessionBlocked(ctx, service.CyberSessionBlockKindTranscript, []string{"expired", "persistent", "missing"})
	require.NoError(t, err)
	require.Empty(t, matched)
	require.Zero(t, ttl)
}
