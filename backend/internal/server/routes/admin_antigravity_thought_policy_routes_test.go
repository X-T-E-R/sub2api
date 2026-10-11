package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The thought policy pair must be reachable only through the admin group, and both
// verbs must exist on the dedicated settings path.
func TestAntigravityThoughtPolicySettingsRoutesUseAdminAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	authCalls := 0
	admin := router.Group("/api/v1/admin")
	admin.Use(func(c *gin.Context) {
		authCalls++
		c.AbortWithStatus(http.StatusUnauthorized)
	})
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Setting: adminhandler.NewSettingHandler(nil, nil, nil, nil, nil, nil, nil),
	}}

	registerSettingsRoutes(admin, handlers)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/v1/admin/settings/antigravity-thought-policy", nil)
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusUnauthorized, recorder.Code, "%s", method)
	}
	require.Equal(t, 2, authCalls)
}
