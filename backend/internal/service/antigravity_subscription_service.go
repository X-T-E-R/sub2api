package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// AntigravitySubscriptionResult 表示订阅检测后的规范化结果。
type AntigravitySubscriptionResult struct {
	PlanType           string
	SubscriptionStatus string
	SubscriptionError  string
}

// NormalizeAntigravitySubscription maps the current subscription independently
// of tier-specific eligibility restrictions. Those are not account denial signals.
func NormalizeAntigravitySubscription(resp *antigravity.LoadCodeAssistResponse) AntigravitySubscriptionResult {
	if resp == nil {
		return AntigravitySubscriptionResult{PlanType: "Free"}
	}
	return AntigravitySubscriptionResult{
		PlanType: antigravity.TierIDToPlanType(resp.GetTier()),
	}
}

// AntigravityIneligibleTier preserves the scope and reason of a tier restriction.
// Even INELIGIBLE_ACCOUNT here describes eligibility for a tier, not API access.
type AntigravityIneligibleTier struct {
	TierID        string `json:"tier_id,omitempty"`
	ReasonCode    string `json:"reason_code,omitempty"`
	ReasonMessage string `json:"reason_message,omitempty"`
}

func normalizeAntigravityIneligibleTiers(resp *antigravity.LoadCodeAssistResponse) []AntigravityIneligibleTier {
	if resp == nil {
		return nil
	}
	var result []AntigravityIneligibleTier
	for _, tier := range resp.IneligibleTiers {
		if tier == nil {
			continue
		}
		entry := AntigravityIneligibleTier{
			ReasonCode:    strings.TrimSpace(tier.ReasonCode),
			ReasonMessage: strings.TrimSpace(tier.ReasonMessage),
		}
		if tier.Tier != nil {
			entry.TierID = strings.TrimSpace(tier.Tier.ID)
		}
		if entry.TierID != "" || entry.ReasonCode != "" || entry.ReasonMessage != "" {
			result = append(result, entry)
		}
	}
	return result
}
