package admin

import (
	"context"
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

type reasoningFloorSettingRepo struct {
	settingHandlerRepoStub
	writeErr error
}

func (r *reasoningFloorSettingRepo) Set(_ context.Context, key, value string) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func TestModelReasoningFloorSettingsAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &reasoningFloorSettingRepo{}
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings", h.GetModelReasoningFloor)
	router.PUT("/settings", h.UpdateModelReasoningFloor)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}
	r := call(http.MethodGet, "")
	require.Equal(t, 200, r.Code)
	require.Equal(t, "[]", gjson.Get(r.Body.String(), "data.rules").Raw)
	valid := `{"enabled":true,"rules":[{"model":"gpt-5.6-luna","min_effort":"xhigh"}]}`
	r = call(http.MethodPut, valid)
	require.Equal(t, 200, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.enabled").Bool())
	stored := repo.values[service.SettingKeyModelReasoningFloor]
	for _, body := range []string{`{}`, `{"enabled":true}`, `{"enabled":true,"rules":null}`, `{"enabled":true,"rules":[],"unknown":true}`, valid + `{}`, `{"enabled":true,"rules":[{"model":"gpt-*","min_effort":"xhigh"}]}`, `{"enabled":true,"rules":[{"model":"same","min_effort":"xhigh"},{"model":"same","min_effort":"high"}]}`} {
		r = call(http.MethodPut, body)
		require.Equal(t, 400, r.Code, body)
		require.Equal(t, stored, repo.values[service.SettingKeyModelReasoningFloor])
	}
	repo.writeErr = errors.New("write unavailable")
	require.Equal(t, 500, call(http.MethodPut, `{"enabled":false,"rules":[]}`).Code)
	require.Equal(t, stored, repo.values[service.SettingKeyModelReasoningFloor])
}
