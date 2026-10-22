//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGatewayRecordUsageForceCachePreservesMeasuredUsage(t *testing.T) {
	tests := []struct {
		name                      string
		usage                     ClaudeUsage
		force                     bool
		billingInput, billingRead int
		billingErr                error
		duplicate, longContext    bool
	}{
		{name: "production_118792_uncached", usage: ClaudeUsage{InputTokens: 118792, OutputTokens: 121}, force: true, billingRead: 118792},
		{name: "force_false", usage: ClaudeUsage{InputTokens: 118792, OutputTokens: 121}, billingInput: 118792},
		{name: "partial_read_and_write", usage: ClaudeUsage{InputTokens: 4000, OutputTokens: 121, CacheReadInputTokens: 9000, CacheCreationInputTokens: 500, CacheCreation5mTokens: 500}, force: true, billingRead: 13000},
		{name: "input_zero", usage: ClaudeUsage{OutputTokens: 121, CacheReadInputTokens: 9000}, force: true, billingRead: 9000},
		{name: "all_zero", force: true},
		{name: "billing_error", usage: ClaudeUsage{InputTokens: 118792, OutputTokens: 121}, force: true, billingRead: 118792, billingErr: errors.New("billing failed")},
		{name: "duplicate_ledger_event", usage: ClaudeUsage{InputTokens: 118792, OutputTokens: 121}, force: true, billingRead: 118792, duplicate: true},
		{name: "long_context", usage: ClaudeUsage{InputTokens: 250000, OutputTokens: 121, CacheReadInputTokens: 9000}, force: true, billingRead: 259000, longContext: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{}
			billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: !tt.duplicate}, err: tt.billingErr}
			svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
			// Use the known fallback price card: this is the same shared RecordUsage path
			// as Argon, with the exact production token counts and a deterministic quote.
			result := &ForwardResult{RequestID: "force-cache-" + tt.name, Model: "claude-sonnet-4", Usage: tt.usage, Duration: time.Second}
			original := *result
			apiKey := &APIKey{ID: 501, Quota: 100}
			user := &User{ID: 601}
			account := &Account{ID: 73, Platform: PlatformAntigravity, Type: AccountTypeOAuth}
			opts := &recordUsageOpts{}
			if tt.longContext {
				opts.LongContextThreshold = 200000
				opts.LongContextMultiplier = 2
			}
			expectedResult := *result
			expectedResult.Usage.InputTokens = tt.billingInput
			expectedResult.Usage.CacheReadInputTokens = tt.billingRead
			expectedCost := svc.calculateRecordUsageCost(context.Background(), &expectedResult, apiKey, result.Model, 1.1, 1, time.Now(), opts)
			require.NotNil(t, expectedCost)
			var err error
			if tt.longContext {
				err = svc.RecordUsageWithLongContext(context.Background(), &RecordUsageLongContextInput{Result: result, APIKey: apiKey, User: user, Account: account, ForceCacheBilling: tt.force, LongContextThreshold: 200000, LongContextMultiplier: 2})
			} else {
				err = svc.RecordUsage(context.Background(), &RecordUsageInput{Result: result, APIKey: apiKey, User: user, Account: account, ForceCacheBilling: tt.force})
			}
			if tt.billingErr != nil {
				require.ErrorIs(t, err, tt.billingErr)
			} else {
				require.NoError(t, err)
			}
			// ForwardResult is also consumed outside billing; no monetary adjustment may
			// rewrite the measured usage shared with those callers.
			require.Equal(t, original.Usage, result.Usage, "billing mutated forwarded usage")
			log := usageRepo.lastLog
			require.NotNil(t, log)
			require.Equal(t, tt.usage.InputTokens, log.InputTokens)
			require.Equal(t, tt.usage.CacheReadInputTokens, log.CacheReadTokens)
			require.Equal(t, tt.usage.CacheCreationInputTokens, log.CacheCreationTokens)
			require.Equal(t, tt.usage.OutputTokens, log.OutputTokens)
			require.InDelta(t, expectedCost.TotalCost, log.TotalCost, 1e-12)
			require.InDelta(t, expectedCost.InputCost, log.InputCost, 1e-12)
			require.InDelta(t, expectedCost.CacheReadCost, log.CacheReadCost, 1e-12)
			if tt.billingErr != nil {
				require.Zero(t, log.ActualCost)
			} else {
				require.InDelta(t, expectedCost.ActualCost, log.ActualCost, 1e-12)
			}
			require.Equal(t, 1, billingRepo.calls)
			cmd := billingRepo.lastCmd
			require.NotNil(t, cmd)
			require.Equal(t, tt.billingInput, cmd.InputTokens, "ledger retains the existing discounted token projection")
			require.Equal(t, tt.billingRead, cmd.CacheReadTokens)
			// Reconstruct the pre-fix ledger command, including its token-based request
			// fingerprint: retries spanning the release must keep the same identity.
			legacyLog := *log
			legacyLog.InputTokens = tt.billingInput
			legacyLog.CacheReadTokens = tt.billingRead
			expectedCmd := buildUsageBillingCommand(log.RequestID, &legacyLog, &postUsageBillingParams{Cost: expectedCost, APIKey: apiKey, User: user, Account: account, AccountRateMultiplier: account.BillingRateMultiplier(), Platform: PlatformAntigravity})
			require.Equal(t, expectedCmd, cmd)
		})
	}
}

func TestGatewayRecordUsageForceCachePreservesTTLAndAccountPricing(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "force"}[force], func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{}
			billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
			svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
			groupID := int64(901)
			// TTL overrides are an existing Anthropic OAuth-only contract.
			account := &Account{ID: 73, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{"cache_ttl_override_enabled": true, "cache_ttl_override_target": "1h"}}
			// Custom account prices intentionally distinguish input from read. A raw-token
			// accounting fix must not accidentally make this existing account cost higher.
			inputPrice, readPrice := 0.004, 0.0001
			channel := &Channel{ID: 1, Status: StatusActive, AccountStatsPricingRules: []AccountStatsPricingRule{{AccountIDs: []int64{73}, Pricing: []ChannelModelPricing{{Models: []string{"claude-sonnet-4"}, InputPrice: &inputPrice, CacheReadPrice: &readPrice}}}}}
			svc.channelService = newTestChannelServiceForStats(t, channel, groupID, PlatformAnthropic)
			apiKey := &APIKey{ID: 501, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformAnthropic, RateMultiplier: 1.1}}
			originalUsage := ClaudeUsage{InputTokens: 4000, OutputTokens: 121, CacheReadInputTokens: 9000, CacheCreationInputTokens: 500, CacheCreation5mTokens: 500}
			result := &ForwardResult{RequestID: "ttl-and-account-prices", Model: "claude-sonnet-4", Usage: originalUsage, Duration: time.Second}
			adjusted := *result
			applyCacheTTLOverride(&adjusted.Usage, "1h")
			if force {
				adjusted.Usage.CacheReadInputTokens += adjusted.Usage.InputTokens
				adjusted.Usage.InputTokens = 0
			}
			expectedCost := svc.calculateRecordUsageCost(context.Background(), &adjusted, apiKey, result.Model, 1.1, 1, time.Now(), &recordUsageOpts{})
			expectedStats := tryCustomRules(channel, account.ID, groupID, PlatformAnthropic, result.Model, UsageTokens{InputTokens: adjusted.Usage.InputTokens, OutputTokens: 121, CacheReadTokens: adjusted.Usage.CacheReadInputTokens, CacheCreationTokens: 500}, 1)
			require.NotNil(t, expectedStats)
			err := svc.RecordUsage(context.Background(), &RecordUsageInput{Result: result, APIKey: apiKey, User: &User{ID: 601}, Account: account, ForceCacheBilling: force})
			require.NoError(t, err)
			require.Equal(t, originalUsage, result.Usage)
			log := usageRepo.lastLog
			require.NotNil(t, log)
			require.Equal(t, originalUsage.InputTokens, log.InputTokens)
			require.Equal(t, originalUsage.CacheReadInputTokens, log.CacheReadTokens)
			require.Equal(t, 500, log.CacheCreationTokens)
			// Existing TTL billing detail and its override marker remain normalized;
			// aggregate measured write tokens and the forwarded usage stay untouched.
			require.True(t, log.CacheTTLOverridden)
			require.Zero(t, log.CacheCreation5mTokens)
			require.Equal(t, 500, log.CacheCreation1hTokens)
			require.InDelta(t, expectedCost.TotalCost, log.TotalCost, 1e-12)
			require.InDelta(t, expectedCost.CacheCreationCost, log.CacheCreationCost, 1e-12)
			require.NotNil(t, log.AccountStatsCost)
			require.InDelta(t, *expectedStats, *log.AccountStatsCost, 1e-12)
		})
	}
}
