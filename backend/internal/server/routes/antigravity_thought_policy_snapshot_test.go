package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityThoughtPolicySnapshotPrecedesRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, prebound := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "bound"}[prebound], func(t *testing.T) {
			router := gin.New()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if prebound {
				request = request.WithContext(service.WithAntigravityThoughtPolicySnapshot(request.Context()))
			}
			router.Use(withAntigravityThoughtPolicySnapshot(func(c *gin.Context) {
				require.Same(t, c.Request.Context(), service.WithAntigravityThoughtPolicySnapshot(c.Request.Context()))
				if prebound {
					require.Same(t, request.Context(), c.Request.Context())
				}
				c.Next()
			}))
			router.POST("/v1/messages", func(c *gin.Context) {
				require.Same(t, c.Request.Context(), service.WithAntigravityThoughtPolicySnapshot(c.Request.Context()))
				c.Status(http.StatusNoContent)
			})
			writer := httptest.NewRecorder()
			router.ServeHTTP(writer, request)
			require.Equal(t, http.StatusNoContent, writer.Code)
		})
	}
}
