package service

import (
	"context"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

func effortOwnershipGateway() (*GatewayService, *modelsListAccountRepoStub, *CompositeRouteResolver) {
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{8: {{
		ID: 69, Platform: PlatformAntigravity,
		Credentials: map[string]any{"model_mapping": map[string]any{"custom": "custom-{effort}"}},
	}}}}
	gateway := &GatewayService{accountRepo: repo, modelsListCache: gocache.New(time.Minute, time.Minute), modelsListCacheTTL: time.Minute}
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(gateway.resolveCompositeModelOwnership)
	return gateway, repo, resolver
}

// B1: real composite routing must use the in-flight policy, including when a
// new-policy negative cache already exists for the same group and model.
func TestAntigravityEffortOwnershipRetainsRequestSnapshot(t *testing.T) {
	settings, _ := effortTestSettings(t, "ultra")
	_, repo, resolver := effortOwnershipGateway()
	oldRequest := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"ultra"}`))
	require.NoError(t, settings.SetAntigravityModelEffortSettings(context.Background(), defaultAntigravityModelEffortSettings()))
	newRequest := WithAntigravityRequestEffort(context.Background(), []byte(`{}`))
	miss, err := resolver.Resolve(newRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.False(t, miss.Matched)
	oldRequest = WithAntigravityEffortPolicySnapshot(oldRequest)
	owned, err := resolver.Resolve(oldRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, owned.Matched)
	require.Equal(t, PlatformAntigravity, owned.TargetPlatform)
	require.Equal(t, CompositeRouteSourceAccount, owned.Source)
	cached, err := resolver.Resolve(oldRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.Equal(t, owned, cached)
	require.Equal(t, int64(2), repo.listByGroupCalls.Load(), "same snapshot should still use ownership cache")
	stillMiss, err := resolver.Resolve(newRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.Equal(t, miss, stillMiss, "old-policy cache hit cannot leak into new policy")
}

// B2: normal global Settings publication must not wait for ownership-miss TTL.
func TestAntigravityEffortOwnershipRefreshesAfterGlobalAddition(t *testing.T) {
	settings, _ := effortTestSettings(t)
	gateway, repo, resolver := effortOwnershipGateway()
	oldRequest := WithAntigravityRequestEffort(context.Background(), nil)
	miss, err := resolver.Resolve(oldRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.False(t, miss.Matched)
	value := defaultAntigravityModelEffortSettings()
	value.Levels = append(value.Levels, "ultra")
	require.NoError(t, settings.SetAntigravityModelEffortSettings(context.Background(), value))
	newRequest := WithAntigravityRequestEffort(context.Background(), nil)
	owned, err := resolver.Resolve(newRequest, 8, "custom-ultra", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, owned.Matched, "normal Settings update must bypass prior negative cache without waiting for TTL")
	require.Equal(t, PlatformAntigravity, owned.TargetPlatform)
	require.Equal(t, int64(2), repo.listByGroupCalls.Load())
	group := int64(8)
	gateway.InvalidateAvailableModelsCache(&group, PlatformAntigravity)
	require.Empty(t, gateway.modelsListCache.Items(), "account invalidation must clear all policy variants for the group")
}
