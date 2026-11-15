package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// RegisterCommonRoutes 注册通用路由（健康检查、状态等）
func RegisterCommonRoutes(r *gin.Engine, drain *middleware.DrainState) {
	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		draining, inFlight := drain.Snapshot()
		c.JSON(http.StatusOK, gin.H{"status": "ok", "draining": draining, "in_flight": inFlight})
	})

	r.GET("/readyz", func(c *gin.Context) {
		draining, inFlight := drain.Snapshot()
		status := http.StatusOK
		if draining {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"draining": draining, "in_flight": inFlight})
	})

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
