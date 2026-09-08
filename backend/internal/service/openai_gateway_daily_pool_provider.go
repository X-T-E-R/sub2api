package service

import "github.com/Wei-Shaw/sub2api/internal/config"

// ProvideOpenAIGatewayService keeps direct test construction independent of storage.
func ProvideOpenAIGatewayService(
	accountRepo AccountRepository, usageLogRepo UsageLogRepository, usageBillingRepo UsageBillingRepository,
	userRepo UserRepository, userSubRepo UserSubscriptionRepository, userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache, cfg *config.Config, schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService, billingService *BillingService, rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService, httpUpstream HTTPUpstream, deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider, grokTokenProvider *GrokTokenProvider, resolver *ModelPricingResolver,
	channelService *ChannelService, balanceNotifyService *BalanceNotifyService, settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository, pool *CodexDailySessionPool,
) *OpenAIGatewayService {
	gateway := NewOpenAIGatewayService(accountRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo,
		userGroupRateRepo, cache, cfg, schedulerSnapshot, concurrencyService, billingService, rateLimitService,
		billingCacheService, httpUpstream, deferredService, openAITokenProvider, grokTokenProvider, resolver,
		channelService, balanceNotifyService, settingService, userPlatformQuotaRepo)
	gateway.codexDailySessionPool = pool
	return gateway
}
