package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type accountSwitchUsageRepo struct {
	UsageLogRepository
	log *UsageLog
}

func (r *accountSwitchUsageRepo) CreateBestEffort(_ context.Context, log *UsageLog) error {
	r.log = log
	return nil
}

func TestGatewayRecordUsagePreservesAccountSwitchCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count *int
	}{
		{name: "no metadata remains unknown"},
		{name: "explicit zero", count: intPtr(0)},
		{name: "failover winner", count: intPtr(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &accountSwitchUsageRepo{}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			svc := &GatewayService{cfg: cfg, billingService: NewBillingService(cfg, nil), usageLogRepo: repo, deferredService: &DeferredService{}}
			ctx := context.Background()
			if tc.count != nil {
				ctx = WithAccountSwitchCount(ctx, *tc.count, false)
			}
			err := svc.RecordUsage(ctx, &RecordUsageInput{
				Result: &ForwardResult{RequestID: "switched", Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 1}},
				APIKey: &APIKey{ID: 11}, User: &User{ID: 1}, Account: &Account{ID: 73},
			})
			require.NoError(t, err)
			require.NotNil(t, repo.log)
			require.Equal(t, tc.count, repo.log.AccountSwitchCount)
		})
	}
}
