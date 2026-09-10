package admin

import (
	"context"
	"encoding/json"
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

type codexSessionAffinityHandlerRepo struct {
	settingHandlerRepoStub
	getErr   error
	writeErr error
}

func (r *codexSessionAffinityHandlerRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *codexSessionAffinityHandlerRepo) Set(_ context.Context, key, value string) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func invokeCodexSessionAffinitySettings(t *testing.T, repo *codexSessionAffinityHandlerRepo, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/settings", h.GetCodexSessionAffinity)
	router.PUT("/settings", h.UpdateCodexSessionAffinity)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestCodexSessionAffinitySettingsHandlerDefaultAndUpdate(t *testing.T) {
	repo := &codexSessionAffinityHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}}}
	recorder := invokeCodexSessionAffinitySettings(t, repo, http.MethodGet, "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "[]", gjson.Get(recorder.Body.String(), "data.group_ids").Raw)

	recorder = invokeCodexSessionAffinitySettings(t, repo, http.MethodPut, `{"group_ids":[12,34]}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "[12,34]", gjson.Get(recorder.Body.String(), "data.group_ids").Raw)
	require.JSONEq(t, `{"group_ids":[12,34]}`, repo.values[service.SettingKeyCodexSessionAffinity])
}

func TestCodexSessionAffinitySettingsHandlerRejectsInvalidInputWithoutWriting(t *testing.T) {
	repo := &codexSessionAffinityHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}}}
	valid := `{"group_ids":[12]}`
	require.Equal(t, http.StatusOK, invokeCodexSessionAffinitySettings(t, repo, http.MethodPut, valid).Code)
	stored := repo.values[service.SettingKeyCodexSessionAffinity]

	tooMany := make([]int64, 1001)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	tooManyJSON, err := json.Marshal(map[string]any{"group_ids": tooMany})
	require.NoError(t, err)

	for _, body := range []string{
		`{}`,
		`{"group_ids":null}`,
		`{"group_ids":[0]}`,
		`{"group_ids":[12,12]}`,
		`{"group_ids":[12],"unknown":true}`,
		valid + `{}`,
		string(tooManyJSON),
	} {
		recorder := invokeCodexSessionAffinitySettings(t, repo, http.MethodPut, body)
		require.Equal(t, http.StatusBadRequest, recorder.Code, body)
		require.Equal(t, stored, repo.values[service.SettingKeyCodexSessionAffinity], body)
	}
}

func TestCodexSessionAffinitySettingsHandlerPropagatesReadAndWriteErrors(t *testing.T) {
	readErr := errors.New("database unavailable")
	repo := &codexSessionAffinityHandlerRepo{
		settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}},
		getErr:                 readErr,
	}
	recorder := invokeCodexSessionAffinitySettings(t, repo, http.MethodGet, "")
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	repo.getErr = nil
	repo.writeErr = errors.New("database unavailable")
	recorder = invokeCodexSessionAffinitySettings(t, repo, http.MethodPut, `{"group_ids":[12]}`)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Empty(t, repo.values)
}
