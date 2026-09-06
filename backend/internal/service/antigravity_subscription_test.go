package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/assert"
)

func TestNormalizeAntigravitySubscription_PaidTierWithIneligible(t *testing.T) {
	resp := &antigravity.LoadCodeAssistResponse{
		PaidTier: &antigravity.PaidTierInfo{ID: "g1-pro-tier"},
		IneligibleTiers: []*antigravity.IneligibleTier{
			{ReasonMessage: "location validation required"},
		},
	}

	result := NormalizeAntigravitySubscription(resp)

	assert.Equal(t, "Pro", result.PlanType, "paid tier should preserve Pro even with ineligible tiers")
	assert.Empty(t, result.SubscriptionStatus, "tier restrictions do not invalidate the current subscription")
	assert.Empty(t, result.SubscriptionError)
}

func TestNormalizeAntigravitySubscription_FreeTierWithIneligible(t *testing.T) {
	resp := &antigravity.LoadCodeAssistResponse{
		PaidTier: &antigravity.PaidTierInfo{ID: "free-tier"},
		IneligibleTiers: []*antigravity.IneligibleTier{
			{ReasonMessage: "some warning"},
		},
	}

	result := NormalizeAntigravitySubscription(resp)

	assert.Equal(t, "Free", result.PlanType, "tier restrictions must preserve a current free tier")
	assert.Empty(t, result.SubscriptionStatus)
}

func TestNormalizeAntigravitySubscription_NoIneligible(t *testing.T) {
	resp := &antigravity.LoadCodeAssistResponse{
		PaidTier: &antigravity.PaidTierInfo{ID: "g1-ultra-tier"},
	}

	result := NormalizeAntigravitySubscription(resp)

	assert.Equal(t, "Ultra", result.PlanType)
	assert.Empty(t, result.SubscriptionStatus)
}

func TestNormalizeAntigravitySubscription_NilResponse(t *testing.T) {
	result := NormalizeAntigravitySubscription(nil)
	assert.Equal(t, "Free", result.PlanType)
}

func TestNormalizeAntigravitySubscription_NoTierWithIneligible(t *testing.T) {
	resp := &antigravity.LoadCodeAssistResponse{
		IneligibleTiers: []*antigravity.IneligibleTier{
			{ReasonMessage: "unknown issue"},
		},
	}

	result := NormalizeAntigravitySubscription(resp)

	assert.Equal(t, "Free", result.PlanType, "missing tier retains the existing default, not an inferred denial")
	assert.Empty(t, result.SubscriptionStatus)
}

func TestNormalizeAntigravitySubscription_TierPrecedenceAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name          string
		current, paid string
		want          string
	}{
		{"paid tier wins", "free-tier", "g1-pro-tier", "Pro"},
		{"current free tier", "free-tier", "", "Free"},
		{"unknown tier keeps identity", "future-tier", "", "future-tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := NormalizeAntigravitySubscription(&antigravity.LoadCodeAssistResponse{
				CurrentTier:     &antigravity.TierInfo{ID: tc.current},
				PaidTier:        &antigravity.PaidTierInfo{ID: tc.paid},
				IneligibleTiers: []*antigravity.IneligibleTier{nil, {}, {Tier: &antigravity.TierInfo{ID: "other-tier"}, ReasonCode: "INELIGIBLE_ACCOUNT"}},
			})
			assert.Equal(t, tc.want, result.PlanType)
			assert.Empty(t, result.SubscriptionStatus)
			assert.Empty(t, result.SubscriptionError)
		})
	}
}
