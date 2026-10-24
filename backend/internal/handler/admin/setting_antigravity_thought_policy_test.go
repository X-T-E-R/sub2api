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

func TestAntigravityThoughtPolicySettingsAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &reasoningFloorSettingRepo{}
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings", h.GetAntigravityThoughtPolicy)
	router.PUT("/settings", h.UpdateAntigravityThoughtPolicy)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}

	// Seed: with nothing stored both switches read as false, not as missing fields.
	r := call(http.MethodGet, "")
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.suppress_thought_only_stop").Exists())
	require.True(t, gjson.Get(r.Body.String(), "data.continue_thought_only").Exists())
	require.False(t, gjson.Get(r.Body.String(), "data.suppress_thought_only_stop").Bool())
	require.False(t, gjson.Get(r.Body.String(), "data.continue_thought_only").Bool())

	valid := `{"suppress_thought_only_stop":true,"continue_thought_only":false,"continue_zero_output":false}`
	r = call(http.MethodPut, valid)
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.suppress_thought_only_stop").Bool())
	require.False(t, gjson.Get(r.Body.String(), "data.continue_thought_only").Bool())
	require.Equal(t, valid, repo.values[service.SettingKeyAntigravityThoughtPolicy])

	// Readback after the write returns the persisted pair.
	r = call(http.MethodGet, "")
	require.True(t, gjson.Get(r.Body.String(), "data.suppress_thought_only_stop").Bool())
	require.False(t, gjson.Get(r.Body.String(), "data.continue_thought_only").Bool())

	stored := repo.values[service.SettingKeyAntigravityThoughtPolicy]
	for _, body := range []string{
		``,
		`{}`,
		`null`,
		`{"suppress_thought_only_stop":true}`,
		`{"continue_thought_only":true}`,
		`{"suppress_thought_only_stop":"true","continue_thought_only":true}`,
		`{"suppress_thought_only_stop":1,"continue_thought_only":true}`,
		`{"suppress_thought_only_stop":true,"continue_thought_only":true,"unknown":true}`,
		`{"suppress_thought_only_stop":true,"continue_thought_only":false,"continue_zero_output":null}`,
		`{"suppress_thought_only_stop":true,"continue_thought_only":false,"continue_zero_output":"true"}`,
		valid + `{}`,
	} {
		require.Equal(t, http.StatusBadRequest, call(http.MethodPut, body).Code, body)
		require.Equal(t, stored, repo.values[service.SettingKeyAntigravityThoughtPolicy], body)
	}

	// Explicit false/false round-trips: an off switch is stored, not dropped.
	require.Equal(t, http.StatusOK, call(http.MethodPut, `{"suppress_thought_only_stop":false,"continue_thought_only":false}`).Code)
	require.Equal(t, `{"suppress_thought_only_stop":false,"continue_thought_only":false,"continue_zero_output":false}`, repo.values[service.SettingKeyAntigravityThoughtPolicy])

	// New switch is global, explicit, and survives a legacy A/B-only save.
	r = call(http.MethodPut, `{"suppress_thought_only_stop":true,"continue_thought_only":false,"continue_zero_output":true}`)
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.continue_zero_output").Bool())
	r = call(http.MethodPut, `{"suppress_thought_only_stop":false,"continue_thought_only":false}`)
	require.Equal(t, http.StatusOK, r.Code)
	require.True(t, gjson.Get(r.Body.String(), "data.continue_zero_output").Bool())
	r = call(http.MethodPut, `{"suppress_thought_only_stop":false,"continue_thought_only":false,"continue_zero_output":false}`)
	require.Equal(t, http.StatusOK, r.Code)
	require.False(t, gjson.Get(r.Body.String(), "data.continue_zero_output").Bool())

	// A failed write must not answer with values that were never persisted.
	repo.writeErr = errors.New("write unavailable")
	r = call(http.MethodPut, valid)
	require.Equal(t, http.StatusInternalServerError, r.Code)
	require.Equal(t, `{"suppress_thought_only_stop":false,"continue_thought_only":false,"continue_zero_output":false}`, repo.values[service.SettingKeyAntigravityThoughtPolicy])
	repo.writeErr = nil
	r = call(http.MethodGet, "")
	require.Equal(t, http.StatusOK, r.Code)
	require.False(t, gjson.Get(r.Body.String(), "data.suppress_thought_only_stop").Bool())
}
