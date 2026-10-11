package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// settingDataKeys returns the JSON keys the API returned inside data, so a test
// can assert the payload exposes nothing beyond the prompt contract.
func settingDataKeys(t *testing.T, body string) []string {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	keys := make([]string, 0, len(envelope.Data))
	for key := range envelope.Data {
		keys = append(keys, key)
	}
	return keys
}

func settingDataString(t *testing.T, body, path string) string {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	value, ok := envelope.Data[path].(string)
	require.True(t, ok, "data.%s must be a string, got %#v", path, envelope.Data[path])
	return value
}

func TestModelTestPromptSettingsAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &reasoningFloorSettingRepo{}
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings/model-test", h.GetModelTestPrompt)
	router.PUT("/settings/model-test", h.UpdateModelTestPrompt)
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings/model-test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}

	// Unconfigured: the stored prompt stays empty and the built-in default is
	// reported separately, which is what lets the admin UI reset to it.
	get := call(http.MethodGet, "")
	require.Equal(t, http.StatusOK, get.Code)
	require.ElementsMatch(t, []string{"prompt", "default_prompt"}, settingDataKeys(t, get.Body.String()))
	require.Equal(t, "", settingDataString(t, get.Body.String(), "prompt"))
	require.Equal(t, service.DefaultModelTestPrompt, settingDataString(t, get.Body.String(), "default_prompt"))

	// A saved prompt is trimmed and read back by both PUT and GET.
	put := call(http.MethodPut, `{"prompt":"  Reply with the single word: pong.  "}`)
	require.Equal(t, http.StatusOK, put.Code)
	require.Equal(t, "Reply with the single word: pong.", settingDataString(t, put.Body.String(), "prompt"))
	require.Equal(t, service.DefaultModelTestPrompt, settingDataString(t, put.Body.String(), "default_prompt"))
	require.Equal(t, "Reply with the single word: pong.", repo.values[service.SettingKeyModelTestPrompt])
	require.Equal(t, "Reply with the single word: pong.", settingDataString(t, call(http.MethodGet, "").Body.String(), "prompt"))

	// An empty prompt clears the stored value: the next GET reports the default
	// through default_prompt, not through prompt.
	cleared := call(http.MethodPut, `{"prompt":""}`)
	require.Equal(t, http.StatusOK, cleared.Code)
	require.Equal(t, "", settingDataString(t, cleared.Body.String(), "prompt"))
	require.Equal(t, "", repo.values[service.SettingKeyModelTestPrompt])

	// 8192 astral characters are 32768 bytes and must still be accepted, so the
	// request-body cap never rejects a prompt the service accepts.
	atLimit := strings.Repeat("😀", service.MaxModelTestPromptRunes)
	require.Equal(t, http.StatusOK, call(http.MethodPut, `{"prompt":"`+atLimit+`"}`).Code)
	require.Equal(t, atLimit, repo.values[service.SettingKeyModelTestPrompt])

	stored := repo.values[service.SettingKeyModelTestPrompt]
	rejected := []struct {
		name string
		body string
	}{
		{name: "missing prompt", body: `{}`},
		{name: "null prompt", body: `{"prompt":null}`},
		{name: "numeric prompt", body: `{"prompt":123}`},
		{name: "array prompt", body: `{"prompt":["hi"]}`},
		{name: "object prompt", body: `{"prompt":{"text":"hi"}}`},
		{name: "unknown key", body: `{"prompt":"ok","api_key":"sk-secret"}`},
		{name: "two objects", body: `{"prompt":"first"}{"prompt":"second"}`},
		{name: "trailing garbage", body: `{"prompt":"first"} trailing`},
		{name: "malformed json", body: `{"prompt":`},
		{name: "not an object", body: `"hi"`},
		{name: "oversize body", body: `{"prompt":"` + strings.Repeat("a", 70000) + `"}`},
		{name: "too many runes", body: `{"prompt":"` + strings.Repeat("😀", service.MaxModelTestPromptRunes+1) + `"}`},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			r := call(http.MethodPut, tc.body)
			require.Equal(t, http.StatusBadRequest, r.Code, r.Body.String())
			require.Equal(t, stored, repo.values[service.SettingKeyModelTestPrompt], "a rejected request must not change the stored prompt")
		})
	}

	repo.writeErr = errors.New("write unavailable")
	require.Equal(t, http.StatusInternalServerError, call(http.MethodPut, `{"prompt":"replacement"}`).Code)
	require.Equal(t, stored, repo.values[service.SettingKeyModelTestPrompt])
	require.Equal(t, http.StatusOK, call(http.MethodGet, "").Code, "a failed write must not break reads")
}
