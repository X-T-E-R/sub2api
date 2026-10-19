package admin

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityUserAgentSettingsAPI(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	antigravity.SetUserAgentResolver(h.settingService.GetAntigravityUserAgentOverride)
	antigravity.SetUserAgentVersionResolver(h.settingService.GetAntigravityUserAgentVersion)
	t.Cleanup(func() {
		antigravity.SetUserAgentResolver(nil)
		antigravity.SetUserAgentVersionResolver(nil)
	})
	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
		h.GetSettings(c)
		return r
	}
	r := get()
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.antigravity_user_agent").Exists())
	require.Empty(t, gjson.Get(r.Body.String(), "data.antigravity_user_agent").String())

	const ua = "independent-client/4 (custom profile)"
	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent": "  " + ua + "  ", "antigravity_user_agent_version": "1.23.2"}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, ua, gjson.Get(r.Body.String(), "data.antigravity_user_agent").String())
	require.Equal(t, ua, repo.values[service.SettingKeyAntigravityUserAgent])
	require.Equal(t, ua, antigravity.GetUserAgent())
	require.Equal(t, ua, gjson.Get(get().Body.String(), "data.antigravity_user_agent").String())

	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent_version": "2.9.1"}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.NotContains(t, repo.lastUpdates, service.SettingKeyAntigravityUserAgent, "omitted field must not be rewritten")
	require.Equal(t, ua, gjson.Get(r.Body.String(), "data.antigravity_user_agent").String())
	require.Equal(t, ua, antigravity.GetUserAgent())

	before := maps.Clone(repo.values)
	for _, invalid := range []string{"\r\nclient", "client\r\nInjected: yes", "client\tua", "client\x00", "client\x7f", "客户端", strings.Repeat("a", 513)} {
		r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent": invalid, "antigravity_user_agent_version": "7.8.9"}, nil)
		require.Equal(t, http.StatusBadRequest, r.Code, r.Body.String())
		require.Contains(t, r.Body.String(), "antigravity_user_agent")
		require.Equal(t, before, repo.values, "invalid input must not persist any settings")
		require.Equal(t, ua, antigravity.GetUserAgent())
		require.Equal(t, "2.9.1", h.settingService.GetAntigravityUserAgentVersion(context.Background()))
	}
	// Boundary length is valid; no provider or product-name allowlist applies.
	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent": strings.Repeat("x", 512)}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, strings.Repeat("x", 512), antigravity.GetUserAgent())
	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent": "   "}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Empty(t, repo.values[service.SettingKeyAntigravityUserAgent])
	require.Empty(t, gjson.Get(r.Body.String(), "data.antigravity_user_agent").String())
	require.Equal(t, "antigravity/hub/2.9.1 darwin/arm64", antigravity.GetUserAgent())
}

func TestAntigravityUserAgentSettingsAuditChange(t *testing.T) {
	before := &service.SystemSettings{AntigravityUserAgent: "old-client/1"}
	after := &service.SystemSettings{AntigravityUserAgent: "new-client/2"}
	require.Contains(t, diffSettings(before, after, nil, nil, UpdateSettingsRequest{}), "antigravity_user_agent")
	require.NotContains(t, diffSettings(after, after, nil, nil, UpdateSettingsRequest{}), "antigravity_user_agent")
}
