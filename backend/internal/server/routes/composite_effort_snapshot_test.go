package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// B1: routing happens before the handler reads effort. Pin at that first
// dependency and pass the same bound request on to the handler.
func TestCompositeEffortSnapshotPrecedesOwnership(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, prebound := range []bool{false, true} {
			t.Run(map[bool]string{false: "json", true: "gemini"}[native]+map[bool]string{false: "/new", true: "/bound"}[prebound], func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				resolver := service.NewCompositeRouteResolver(nil)
				var ownershipContext context.Context
				resolver.SetModelOwnershipResolver(func(ctx context.Context, _ int64, model string) (service.CompositeModelOwnership, error) {
					require.Equal(t, "custom-ultra", model)
					// An unbound context would be replaced, failing this assertion.
					require.Same(t, ctx, service.WithAntigravityEffortPolicySnapshot(ctx))
					ownershipContext = ctx
					return service.CompositeModelOwnership{Matched: true, TargetPlatform: service.PlatformAntigravity}, nil
				})
				router := gin.New()
				router.Use(func(c *gin.Context) {
					group := int64(8)
					c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{GroupID: &group, Group: &service.Group{ID: group, Platform: service.PlatformComposite}})
					c.Next()
				})
				path := "/v1/responses"
				if native {
					path = "/v1beta/models/custom-ultra:generateContent"
					router.Use(compositeGeminiTargetPlatformMiddleware(resolver))
				} else {
					router.Use(compositeTargetPlatformMiddleware(resolver))
				}
				handler := func(c *gin.Context) {
					require.NotNil(t, ownershipContext)
					require.Same(t, c.Request.Context(), service.WithAntigravityEffortPolicySnapshot(c.Request.Context()))
					platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
					require.True(t, ok)
					require.Equal(t, service.PlatformAntigravity, platform)
					body, err := io.ReadAll(c.Request.Body)
					require.NoError(t, err)
					require.JSONEq(t, `{"model":"custom-ultra","reasoning_effort":"ultra"}`, string(body))
					c.Status(http.StatusNoContent)
				}
				if native {
					router.POST("/v1beta/models/*modelAction", handler)
				} else {
					router.POST(path, handler)
				}
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"custom-ultra","reasoning_effort":"ultra"}`))
				if prebound {
					req = req.WithContext(service.WithAntigravityEffortPolicySnapshot(req.Context()))
				}
				req.Header.Set("Content-Type", "application/json")
				writer := httptest.NewRecorder()
				router.ServeHTTP(writer, req)
				require.Equal(t, http.StatusNoContent, writer.Code)
			})
		}
	}
}
