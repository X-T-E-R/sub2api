//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAISelectAccountForModelWithExclusions_ChannelMappedRestrictionRejectsEarly(t *testing.T) {
	t.Parallel()

	channelSvc := newTestChannelService(makeStandardRepo(Channel{
		ID:                 1,
		Status:             StatusActive,
		GroupIDs:           []int64{10},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceChannelMapped,
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformOpenAI, Models: []string{"gpt-4o"}},
		},
		ModelMapping: map[string]map[string]string{
			PlatformOpenAI: {"gpt-4.1": "o3-mini"},
		},
	}, map[int64]string{10: PlatformOpenAI}))

	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true},
		}},
		channelService: channelSvc,
	}

	groupID := int64(10)
	_, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "gpt-4.1", nil)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Contains(t, err.Error(), "channel pricing restriction")
}

func TestOpenAISelectAccountForModelWithExclusions_UpstreamRestrictionSkipsDisallowedAccount(t *testing.T) {
	t.Parallel()

	channelSvc := newTestChannelService(makeStandardRepo(Channel{
		ID:                 1,
		Status:             StatusActive,
		GroupIDs:           []int64{10},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformOpenAI, Models: []string{"o3-mini"}},
		},
	}, map[int64]string{10: PlatformOpenAI}))

	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Priority:    10,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-4.1": "gpt-4o"},
				},
			},
			{
				ID:          2,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Priority:    20,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-4.1": "o3-mini"},
				},
			},
		}},
		channelService: channelSvc,
	}

	groupID := int64(10)
	account, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "gpt-4.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(2), account.ID)
}

func TestIsUpstreamModelRestrictedByChannel_CompactMappingMatchesForwardPath(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"model_mapping":         map[string]any{"gpt-5.4-channel": "gpt-5.4-account"},
			"compact_model_mapping": map[string]any{"gpt-5.4-account": "gpt-5.4-compact"},
		},
	}
	tests := []struct {
		name                   string
		allowedUpstreamModel   string
		useCompactModelMapping bool
	}{
		{
			name:                   "legacy compact applies compact mapping after channel and account mapping",
			allowedUpstreamModel:   "gpt-5.4-compact",
			useCompactModelMapping: true,
		},
		{
			name:                   "native v2 stops after channel and account mapping",
			allowedUpstreamModel:   "gpt-5.4-account",
			useCompactModelMapping: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			channelSvc := newTestChannelService(makeStandardRepo(Channel{
				ID:                 1,
				Status:             StatusActive,
				GroupIDs:           []int64{10},
				RestrictModels:     true,
				BillingModelSource: BillingModelSourceUpstream,
				ModelPricing: []ChannelModelPricing{
					{Platform: PlatformOpenAI, Models: []string{tt.allowedUpstreamModel}},
				},
				ModelMapping: map[string]map[string]string{
					PlatformOpenAI: {"gpt-5.4": "gpt-5.4-channel"},
				},
			}, map[int64]string{10: PlatformOpenAI}))
			svc := &OpenAIGatewayService{channelService: channelSvc}
			mapping := channelSvc.ResolveChannelMapping(context.Background(), 10, "gpt-5.4")
			require.True(t, mapping.Mapped)
			require.Equal(t, "gpt-5.4-channel", mapping.MappedModel)

			ctx := WithOpenAIForwardModel(
				context.Background(),
				mapping.MappedModel,
				tt.useCompactModelMapping,
			)
			require.False(t, svc.isUpstreamModelRestrictedByChannel(
				ctx, 10, account, "gpt-5.4", true,
			))
			require.True(t, svc.isUpstreamModelRestrictedByChannel(
				context.Background(), 10, account, "gpt-5.4", true,
			), "without the forward-model context the restriction check follows a different chain")
		})
	}
}

func TestIsUpstreamModelRestrictedByChannel_PassthroughMatchesForwardPath(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4-channel": "gpt-5.4-account"},
			"compact_model_mapping": map[string]any{
				"gpt-5.4-channel": "gpt-5.4-compact",
			},
		},
		Extra: map[string]any{"openai_passthrough": true},
	}
	tests := []struct {
		name                   string
		allowedUpstreamModel   string
		useCompactModelMapping bool
	}{
		{
			name:                   "native v2 keeps channel-mapped model and ignores normal account mapping",
			allowedUpstreamModel:   "gpt-5.4-channel",
			useCompactModelMapping: false,
		},
		{
			name:                   "legacy compact applies compact mapping to channel-mapped model",
			allowedUpstreamModel:   "gpt-5.4-compact",
			useCompactModelMapping: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			channelSvc := newTestChannelService(makeStandardRepo(Channel{
				ID:                 1,
				Status:             StatusActive,
				GroupIDs:           []int64{10},
				RestrictModels:     true,
				BillingModelSource: BillingModelSourceUpstream,
				ModelPricing: []ChannelModelPricing{
					{Platform: PlatformOpenAI, Models: []string{tt.allowedUpstreamModel}},
				},
				ModelMapping: map[string]map[string]string{
					PlatformOpenAI: {"gpt-5.4": "gpt-5.4-channel"},
				},
			}, map[int64]string{10: PlatformOpenAI}))
			svc := &OpenAIGatewayService{channelService: channelSvc}
			mapping := channelSvc.ResolveChannelMapping(context.Background(), 10, "gpt-5.4")
			require.True(t, mapping.Mapped)
			require.Equal(t, "gpt-5.4-channel", mapping.MappedModel)

			ctx := WithOpenAIForwardModel(
				context.Background(),
				mapping.MappedModel,
				tt.useCompactModelMapping,
			)
			require.False(t, svc.isUpstreamModelRestrictedByChannel(
				ctx, 10, account, "gpt-5.4", true,
			))
		})
	}
}

func TestIsUpstreamModelRestrictedByChannel_PassthroughFlagWithRawChatFallbackMatchesForwardPath(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4-channel": "gpt-5.4-account"},
			"compact_model_mapping": map[string]any{
				"gpt-5.4-account": "gpt-5.4-compact",
			},
		},
		Extra: map[string]any{
			"openai_passthrough":         true,
			"openai_responses_supported": false,
		},
	}

	for _, useCompactModelMapping := range []bool{false, true} {
		useCompactModelMapping := useCompactModelMapping
		name := "native v2"
		if useCompactModelMapping {
			name = "legacy compact"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			channelSvc := newTestChannelService(makeStandardRepo(Channel{
				ID:                 1,
				Status:             StatusActive,
				GroupIDs:           []int64{10},
				RestrictModels:     true,
				BillingModelSource: BillingModelSourceUpstream,
				ModelPricing: []ChannelModelPricing{
					{Platform: PlatformOpenAI, Models: []string{"gpt-5.4-account"}},
				},
				ModelMapping: map[string]map[string]string{
					PlatformOpenAI: {"gpt-5.4": "gpt-5.4-channel"},
				},
			}, map[int64]string{10: PlatformOpenAI}))
			svc := &OpenAIGatewayService{channelService: channelSvc}
			ctx := WithOpenAIForwardModel(
				context.Background(),
				"gpt-5.4-channel",
				useCompactModelMapping,
			)

			require.False(t, svc.isUpstreamModelRestrictedByChannel(
				ctx, 10, account, "gpt-5.4", true,
			))
		})
	}
}

func TestIsUpstreamModelRestrictedByChannel_ForwardModelContextMatchesNormalForwardPath(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4-channel": "gpt-5.4-account"},
		},
		Extra: map[string]any{
			"openai_passthrough":         true,
			"openai_responses_supported": false,
		},
	}
	channelSvc := newTestChannelService(makeStandardRepo(Channel{
		ID:                 1,
		Status:             StatusActive,
		GroupIDs:           []int64{10},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformOpenAI, Models: []string{"gpt-5.4-account"}},
		},
		ModelMapping: map[string]map[string]string{
			PlatformOpenAI: {"gpt-5.4": "gpt-5.4-channel"},
		},
	}, map[int64]string{10: PlatformOpenAI}))
	svc := &OpenAIGatewayService{channelService: channelSvc}
	ctx := WithOpenAIForwardModel(context.Background(), "gpt-5.4-channel", false)

	require.False(t, svc.isUpstreamModelRestrictedByChannel(
		ctx, 10, account, "gpt-5.4", false,
	))
}

func TestOpenAISelectAccountForModelWithExclusions_StickyRestrictedUpstreamFallsBack(t *testing.T) {
	t.Parallel()

	channelSvc := newTestChannelService(makeStandardRepo(Channel{
		ID:                 1,
		Status:             StatusActive,
		GroupIDs:           []int64{10},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformOpenAI, Models: []string{"o3-mini"}},
		},
	}, map[int64]string{10: PlatformOpenAI}))

	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:sticky-session": 1},
	}
	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Priority:    10,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-4.1": "gpt-4o"},
				},
			},
			{
				ID:          2,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Priority:    20,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-4.1": "o3-mini"},
				},
			},
		}},
		channelService: channelSvc,
		cache:          cache,
	}

	groupID := int64(10)
	account, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "sticky-session", "gpt-4.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(2), account.ID)
	require.Equal(t, 1, cache.deletedSessions["openai:sticky-session"])
	require.Equal(t, int64(2), cache.sessionBindings["openai:sticky-session"])
}

func TestOpenAISelectAccount_SupportsChannelMappedForwardModelWhitelist(t *testing.T) {
	t.Parallel()

	channelSvc := newTestChannelService(makeStandardRepo(Channel{
		ID:       1,
		Status:   StatusActive,
		GroupIDs: []int64{10},
		ModelMapping: map[string]map[string]string{
			PlatformOpenAI: {"public-gpt": "channel-gpt-5"},
		},
	}, map[int64]string{10: PlatformOpenAI}))

	account := Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Priority:    1,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"channel-gpt-5": "gpt-5.4"},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:    stubOpenAIAccountRepo{accounts: []Account{account}},
		channelService: channelSvc,
	}

	groupID := int64(10)
	// Without WithOpenAIForwardModel, the raw public alias does not match the
	// account's channel-mapped whitelist key.
	_, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, "", "public-gpt", nil)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)

	// With WithOpenAIForwardModel recorded by the handler after channel mapping,
	// scheduling admits the account whose whitelist matches the forwarded model.
	ctx := WithOpenAIForwardModel(context.Background(), "channel-gpt-5", false)
	selected, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "public-gpt", nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, int64(11), selected.ID)

	// Unrelated forward model still gets rejected.
	badCtx := WithOpenAIForwardModel(context.Background(), "other-model", false)
	_, err = svc.SelectAccountForModelWithExclusions(badCtx, &groupID, "", "public-gpt", nil)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
}
