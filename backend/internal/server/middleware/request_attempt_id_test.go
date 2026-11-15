package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRequestLoggerReusedCorrelationHeadersHaveDistinctAttemptIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID(), RequestLogger())
	var attempts []string
	router.GET("/t", func(c *gin.Context) {
		require.Equal(t, "same-request-id", c.Request.Context().Value(ctxkey.RequestID))
		require.Equal(t, "same-client-id", c.Request.Context().Value(ctxkey.ClientRequestID))
		attempt, ok := c.Request.Context().Value(ctxkey.GatewayAttemptID).(string)
		require.True(t, ok)
		require.NotEmpty(t, attempt)
		attempts = append(attempts, attempt)
		c.Status(http.StatusOK)
	})
	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/t", nil)
		request.Header.Set("X-Request-ID", "same-request-id")
		request.Header.Set("X-Client-Request-Id", "same-client-id")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "same-request-id", response.Header().Get("X-Request-ID"))
	}
	require.Len(t, attempts, 2)
	require.NotEqual(t, attempts[0], attempts[1])
}
