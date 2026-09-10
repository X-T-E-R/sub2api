package service

import (
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// resetOpenAIUpstreamPoolOnCapacityShed resets only the exact transport entry
// that produced a pre-output capacity-shed response. The repository token is
// generation-bound, so a late event cannot evict a replacement entry.
func (s *OpenAIGatewayService) resetOpenAIUpstreamPoolOnCapacityShed(c *gin.Context, account *Account, resp *http.Response, payload []byte) {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.ResetOpenAIPoolOnCapacityShed ||
		account == nil || account.Platform != PlatformOpenAI || resp == nil ||
		!isOpenAIUpstreamCapacityShedEvent(payload) {
		return
	}
	token, ok := HTTPUpstreamPoolEntryTokenFromResponse(resp)
	protocol := openAIPoolResetProtocol(token, resp)
	resetter, supported := s.httpUpstream.(HTTPUpstreamPoolReset)
	if !ok || !supported {
		RecordOpenAIPoolResetEvent(OpenAIPoolResetEvent{
			AccountID: account.ID,
			Protocol:  protocol,
			At:        time.Now(),
		})
		return
	}
	cooldown := time.Duration(s.cfg.Gateway.OpenAIPoolResetCooldownSeconds) * time.Second
	reset := resetter.ResetIdleConnectionPool(token, cooldown)
	RecordOpenAIPoolResetEvent(OpenAIPoolResetEvent{
		AccountID: account.ID,
		Protocol:  protocol,
		Triggered: reset,
		At:        time.Now(),
	})
	fields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.Bool("reset", reset),
		zap.String("pool_cache_key", token.CacheKey),
		zap.Uint64("pool_generation", token.Generation),
	}
	if c != nil {
		logger.FromContext(c).Info("gateway.openai_pool_reset_on_capacity_shed", fields...)
	}
}
