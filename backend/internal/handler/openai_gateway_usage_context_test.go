package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "request-456")
	parent = context.WithValue(parent, ctxkey.GatewayAttemptID, "server-attempt-789")
	parent = service.WithAccountSwitchCount(parent, 2, false)
	parent, cancel := context.WithCancel(parent)
	cancel()

	var gotClientRequestID string
	var gotRequestID string
	var gotAttemptID string
	h := &GatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
		gotAttemptID, _ = ctx.Value(ctxkey.GatewayAttemptID).(string)
		switchCount, ok := service.AccountSwitchCountFromContext(ctx)
		require.True(t, ok)
		require.Equal(t, 2, switchCount)
		require.NoError(t, ctx.Err(), "post-stream usage must survive client cancellation")
	})

	require.Equal(t, "client-request-123", gotClientRequestID)
	require.Equal(t, "request-456", gotRequestID)
	require.Equal(t, "server-attempt-789", gotAttemptID)
}

func TestOpenAISubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "openai-client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "openai-request-456")
	parent = context.WithValue(parent, ctxkey.GatewayAttemptID, "openai-server-attempt-789")

	var gotClientRequestID string
	var gotRequestID string
	var gotAttemptID string
	h := &OpenAIGatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
		gotAttemptID, _ = ctx.Value(ctxkey.GatewayAttemptID).(string)
	})

	require.Equal(t, "openai-client-request-123", gotClientRequestID)
	require.Equal(t, "openai-request-456", gotRequestID)
	require.Equal(t, "openai-server-attempt-789", gotAttemptID)
}

func TestUsageTaskRetriesKeepIngressAttemptIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, gateway := range []string{"openai", "anthropic"} {
		t.Run(gateway, func(t *testing.T) {
			router := gin.New()
			router.Use(middleware.ClientRequestID(), middleware.RequestLogger())
			var attempts []string
			router.POST("/usage", func(c *gin.Context) {
				parent, cancel := context.WithCancel(c.Request.Context())
				cancel()
				task := func(ctx context.Context) {
					require.NoError(t, ctx.Err())
					require.Equal(t, "same-request-id", ctx.Value(ctxkey.RequestID))
					require.Equal(t, "same-client-id", ctx.Value(ctxkey.ClientRequestID))
					attempt, _ := ctx.Value(ctxkey.GatewayAttemptID).(string)
					require.NotEmpty(t, attempt)
					attempts = append(attempts, attempt)
				}
				if gateway == "openai" {
					(&OpenAIGatewayHandler{}).submitUsageRecordTask(parent, task)
				} else {
					(&GatewayHandler{}).submitUsageRecordTask(parent, task)
				}
				c.Status(http.StatusOK)
			})
			for range 2 {
				request := httptest.NewRequest(http.MethodPost, "/usage", nil)
				request.Header.Set("X-Request-ID", "same-request-id")
				request.Header.Set("X-Client-Request-Id", "same-client-id")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code)
			}
			require.Len(t, attempts, 2)
			require.NotEqual(t, attempts[0], attempts[1])
		})
	}
}
