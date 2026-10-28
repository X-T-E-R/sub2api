package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Set the scope before deriving any seed so every generic ingress shares the
// same API-key/group/target-platform boundary, including composite/forced routes.
func setGatewaySessionContext(c *gin.Context, apiKey *service.APIKey, parsed *service.ParsedRequest) {
	if parsed == nil {
		return
	}
	platform := effectiveAPIKeyPlatform(c, apiKey)
	if forced, ok := middleware.GetForcePlatformFromContext(c); ok {
		platform = forced
	}
	parsed.GroupID = apiKey.GroupID
	parsed.SessionContext = &service.SessionContext{
		ClientIP:        ip.GetClientIP(c),
		UserAgent:       c.GetHeader("User-Agent"),
		APIKeyID:        apiKey.ID,
		Platform:        platform,
		ClientSessionID: service.GatewaySessionIDFromHeaders(c),
	}
}
