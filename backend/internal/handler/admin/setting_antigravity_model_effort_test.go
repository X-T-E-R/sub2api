package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityModelEffortSettingsAPIHotUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &reasoningFloorSettingRepo{}
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings", h.GetAntigravityModelEffort)
	router.PUT("/settings", h.UpdateAntigravityModelEffort)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}
	account := &service.Account{Platform: service.PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"custom": "custom-{effort}"}}}
	require.Equal(t, http.StatusOK, call(http.MethodGet, "").Code)
	require.False(t, account.IsModelSupported("custom-ultra"))
	valid := `{"levels":["low","medium","high","ultra"],"default_effort":"medium"}`
	r := call(http.MethodPut, valid)
	require.Equal(t, http.StatusOK, r.Code)
	require.Equal(t, "medium", gjson.Get(r.Body.String(), "data.default_effort").String())
	require.True(t, account.IsModelSupported("custom-ultra"), "normal admin update must publish immediately, including an already cached account")
	require.Equal(t, "custom-medium", account.GetMappedModel("custom"))
	stored := repo.values[service.SettingKeyAntigravityModelEffort]
	for _, body := range []string{`{}`, `{"levels":[],"default_effort":"medium"}`, `{"levels":["low"],"default_effort":"medium"}`, `{"levels":["medium","bad name"],"default_effort":"medium"}`, valid + `{}`, `{"levels":["medium"],"default_effort":"medium","unknown":1}`} {
		require.Equal(t, http.StatusBadRequest, call(http.MethodPut, body).Code)
		require.Equal(t, stored, repo.values[service.SettingKeyAntigravityModelEffort])
	}
	repo.writeErr = errors.New("write unavailable")
	require.Equal(t, http.StatusInternalServerError, call(http.MethodPut, `{"levels":["medium"],"default_effort":"medium"}`).Code)
	require.True(t, account.IsModelSupported("custom-ultra"), "failed persistence must not publish")
	repo.writeErr = nil
	require.Equal(t, http.StatusOK, call(http.MethodPut, `{"levels":["none","minimal","low","medium","high","xhigh","max"],"default_effort":"medium"}`).Code)
	require.False(t, account.IsModelSupported("custom-ultra"), "removal applies immediately")
	require.False(t, account.IsModelSupported("custom-image"), "independent IDs must not be mistaken for levels")
}
