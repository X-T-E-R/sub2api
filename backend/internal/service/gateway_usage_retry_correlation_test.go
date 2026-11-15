//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestRecordUsageRetriesShareCorrelationNotBillingIdentity(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			billing := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
			userRepo := &openAIRecordUsageUserRepoStub{}
			subRepo := &openAIRecordUsageSubRepoStub{}
			openai := newOpenAIRecordUsageServiceWithBillingRepoForTest(logs, billing, userRepo, subRepo, nil)
			anthropic := newGatewayRecordUsageServiceWithBillingRepoForTest(logs, billing, userRepo, subRepo)
			persisted := make(map[string]*UsageLog)
			commands := make(map[string]*UsageBillingCommand)
			for _, attempt := range []string{"attempt-1", "attempt-2"} {
				ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "shared-retry-correlation")
				ctx = context.WithValue(ctx, ctxkey.RequestID, "shared-incoming-request-id")
				ctx = context.WithValue(ctx, ctxkey.GatewayAttemptID, attempt)
				key, user, account := &APIKey{ID: 1}, &User{ID: 2}, &Account{ID: 3}
				var err error
				if platform == PlatformOpenAI {
					err = openai.RecordUsage(ctx, &OpenAIRecordUsageInput{
						Result: &OpenAIForwardResult{RequestID: "same-upstream-id", Model: "gpt-5.1", Stream: true, Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 10}, Duration: time.Second},
						APIKey: key, User: user, Account: account,
					})
				} else {
					err = anthropic.RecordUsage(ctx, &RecordUsageInput{
						Result: &ForwardResult{RequestID: "same-upstream-id", Model: "claude-sonnet-4", Stream: true, Usage: ClaudeUsage{InputTokens: 100, OutputTokens: 10}, Duration: time.Second},
						APIKey: key, User: user, Account: account,
					})
				}
				require.NoError(t, err)
				require.Equal(t, "local:"+attempt, billing.lastCmd.RequestID)
				require.Equal(t, billing.lastCmd.RequestID, logs.lastLog.RequestID)
				require.Greater(t, billing.lastCmd.BalanceCost, 0.0)
				require.Greater(t, logs.lastLog.ActualCost, 0.0)
				require.Equal(t, "shared-retry-correlation", *logs.lastLog.ClientRequestID)
				require.Equal(t, "shared-incoming-request-id", *logs.lastLog.GatewayRequestID)
				persisted[logs.lastLog.RequestID] = logs.lastLog
				commands[billing.lastCmd.RequestID] = billing.lastCmd
			}
			require.Len(t, persisted, 2, "each retry must have a distinct usage_logs unique key")
			require.Len(t, commands, 2, "each retry must have a distinct billing dedup key")
			require.Equal(t, 2, logs.calls)
			require.Equal(t, 2, billing.calls)
		})
	}
}
