package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheAntigravitySafetyStreak(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rawCache := NewGatewayCache(client)
	streakCache, ok := rawCache.(service.AntigravitySafetyStreakCache)
	require.True(t, ok, "NewGatewayCache must implement service.AntigravitySafetyStreakCache")

	ctx := context.Background()
	_, err := streakCache.GetAntigravitySafetyStreak(ctx, "missing")
	require.ErrorIs(t, err, service.ErrAntigravitySafetyStreakNotFound)

	payload := []byte(`{"soft_streak":2,"hard_streak":0}`)
	require.NoError(t, streakCache.SetAntigravitySafetyStreak(ctx, "hash123", payload, 15*time.Minute))
	require.True(t, mr.Exists("agy:safety_streak:hash123"))
	require.Equal(t, 15*time.Minute, mr.TTL("agy:safety_streak:hash123"))

	got, err := streakCache.GetAntigravitySafetyStreak(ctx, "hash123")
	require.NoError(t, err)
	require.Equal(t, payload, got)

	require.NoError(t, streakCache.DeleteAntigravitySafetyStreak(ctx, "hash123"))
	require.False(t, mr.Exists("agy:safety_streak:hash123"))
}
