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

func TestAntigravityWebSearchModelSettingsAPI(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
		h.GetSettings(c)
		return r
	}
	r := get()
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.antigravity_web_search_model").Exists())
	require.Empty(t, gjson.Get(r.Body.String(), "data.antigravity_web_search_model").String())
	require.Empty(t, h.settingService.GetAntigravityWebSearchModel(context.Background()))

	const searchModel = "gemini-3.1-flash-lite"
	r = doUpdateSettings(t, h, map[string]any{"antigravity_web_search_model": "  models/" + searchModel + "  "}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, searchModel, gjson.Get(r.Body.String(), "data.antigravity_web_search_model").String())
	require.Equal(t, searchModel, repo.values[service.SettingKeyAntigravityWebSearchModel])
	require.Equal(t, searchModel, h.settingService.GetAntigravityWebSearchModel(context.Background()))
	require.Equal(t, searchModel, antigravity.EffectiveWebSearchModel(h.settingService.GetAntigravityWebSearchModel(context.Background())))

	// Omitted partial PUT preserves existing value without rewriting.
	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent_version": "2.9.1"}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.NotContains(t, repo.lastUpdates, service.SettingKeyAntigravityWebSearchModel)
	require.Equal(t, searchModel, gjson.Get(r.Body.String(), "data.antigravity_web_search_model").String())
	require.Equal(t, searchModel, h.settingService.GetAntigravityWebSearchModel(context.Background()))

	// Invalid values are rejected without mutating stored settings or runtime snapshot.
	before := maps.Clone(repo.values)
	for _, invalid := range []string{"model with space", "model\r\ninjected", "模型", strings.Repeat("m", 129)} {
		r = doUpdateSettings(t, h, map[string]any{"antigravity_web_search_model": invalid}, nil)
		require.Equal(t, http.StatusBadRequest, r.Code, r.Body.String())
		require.Contains(t, r.Body.String(), "antigravity_web_search_model")
		require.Equal(t, before, repo.values)
		require.Equal(t, searchModel, h.settingService.GetAntigravityWebSearchModel(context.Background()))
	}

	// Clearing resets to empty and falls back to DefaultWebSearchFallbackModel ("gemini-2.5-flash").
	r = doUpdateSettings(t, h, map[string]any{"antigravity_web_search_model": "   "}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Empty(t, repo.values[service.SettingKeyAntigravityWebSearchModel])
	require.Empty(t, gjson.Get(r.Body.String(), "data.antigravity_web_search_model").String())
	require.Empty(t, h.settingService.GetAntigravityWebSearchModel(context.Background()))
	require.Equal(t, antigravity.DefaultWebSearchFallbackModel, antigravity.EffectiveWebSearchModel(h.settingService.GetAntigravityWebSearchModel(context.Background())))
}

func TestAntigravityWebSearchModelSettingsAuditChange(t *testing.T) {
	before := &service.SystemSettings{AntigravityWebSearchModel: "gemini-2.5-flash"}
	after := &service.SystemSettings{AntigravityWebSearchModel: "gemini-3.1-flash-lite"}
	require.Contains(t, diffSettings(before, after, nil, nil, UpdateSettingsRequest{}), "antigravity_web_search_model")
	require.NotContains(t, diffSettings(after, after, nil, nil, UpdateSettingsRequest{}), "antigravity_web_search_model")
}
