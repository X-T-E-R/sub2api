package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Pin before routing/account selection so failover and continuation share the
// global decision made at ingress. Non-Antigravity paths never consume it.
func withAntigravityThoughtPolicySnapshot(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithAntigravityThoughtPolicySnapshot(c.Request.Context()))
		next(c)
	}
}
