package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type grokHTTP5xxHandlerSettingRepo struct {
	value    string
	hasValue bool
	getCalls int
	getErr   error
	setCalls int
	setErr   error
	setKey   string
	setValue string
}

func (r *grokHTTP5xxHandlerSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	if !r.hasValue || key != service.SettingKeyGrokOAuthHTTP5xxCooldownSettings {
		return nil, service.ErrSettingNotFound
	}
	return &service.Setting{Key: key, Value: r.value}, nil
}

func (r *grokHTTP5xxHandlerSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	setting, err := r.Get(ctx, key)
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

func (r *grokHTTP5xxHandlerSettingRepo) Set(_ context.Context, key, value string) error {
	r.setCalls++
	if r.setErr != nil {
		return r.setErr
	}
	r.setKey = key
	r.setValue = value
	r.value = value
	r.hasValue = true
	return nil
}

func (r *grokHTTP5xxHandlerSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *grokHTTP5xxHandlerSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *grokHTTP5xxHandlerSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *grokHTTP5xxHandlerSettingRepo) Delete(context.Context, string) error { return nil }

func newGrokHTTP5xxSettingHandler(repo *grokHTTP5xxHandlerSettingRepo) *SettingHandler {
	cfg := &config.Config{}
	cfg.Gateway.Grok.OAuthHTTP5xxCooldownSeconds = 41
	return NewSettingHandler(service.NewSettingService(repo, cfg), nil, nil, nil, nil, nil, nil)
}

func invokeGrokHTTP5xxSettingHandler(t *testing.T, method, body string, invoke func(*SettingHandler, *gin.Context), repo *grokHTTP5xxHandlerSettingRepo) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, "/api/v1/admin/settings/grok-oauth-http-5xx-cooldown", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	invoke(newGrokHTTP5xxSettingHandler(repo), c)
	return recorder
}

func decodeGrokHTTP5xxHandlerData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope.Data
}

func TestGrokOAuthHTTP5xxCooldownSettingsHandlerGetDoesNotMaterializeRow(t *testing.T) {
	repo := &grokHTTP5xxHandlerSettingRepo{}
	recorder := invokeGrokHTTP5xxSettingHandler(t, http.MethodGet, "", func(h *SettingHandler, c *gin.Context) {
		h.GetGrokOAuthHTTP5xxCooldownSettings(c)
	}, repo)

	require.Equal(t, http.StatusOK, recorder.Code)
	data := decodeGrokHTTP5xxHandlerData(t, recorder)
	require.Equal(t, true, data["enabled"])
	require.Equal(t, float64(41), data["cooldown_seconds"])
	require.Equal(t, service.GrokOAuthHTTP5xxCooldownSourceStartupConfig, data["source"])
	require.Zero(t, repo.setCalls)
}

func TestGrokOAuthHTTP5xxCooldownSettingsHandlerPutPersistsCompleteOverride(t *testing.T) {
	repo := &grokHTTP5xxHandlerSettingRepo{getErr: errors.New("hypothetical read failure")}
	recorder := invokeGrokHTTP5xxSettingHandler(t, http.MethodPut, `{"enabled":false,"cooldown_seconds":33}`, func(h *SettingHandler, c *gin.Context) {
		h.UpdateGrokOAuthHTTP5xxCooldownSettings(c)
	}, repo)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, repo.setCalls)
	require.Zero(t, repo.getCalls)
	require.Equal(t, service.SettingKeyGrokOAuthHTTP5xxCooldownSettings, repo.setKey)
	require.Equal(t, `{"enabled":false,"cooldown_seconds":33}`, repo.setValue)
	data := decodeGrokHTTP5xxHandlerData(t, recorder)
	require.Equal(t, false, data["enabled"])
	require.Equal(t, float64(33), data["cooldown_seconds"])
	require.Equal(t, service.GrokOAuthHTTP5xxCooldownSourceRuntimeSetting, data["source"])
}

func TestGrokOAuthHTTP5xxCooldownSettingsHandlerPutPersistenceFailureIsInternal(t *testing.T) {
	repo := &grokHTTP5xxHandlerSettingRepo{setErr: errors.New("sensitive database failure detail")}
	recorder := invokeGrokHTTP5xxSettingHandler(t, http.MethodPut, `{"enabled":true,"cooldown_seconds":33}`, func(h *SettingHandler, c *gin.Context) {
		h.UpdateGrokOAuthHTTP5xxCooldownSettings(c)
	}, repo)

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, 1, repo.setCalls)
	require.Zero(t, repo.getCalls)
	require.NotContains(t, recorder.Body.String(), "sensitive database failure detail")
	require.Contains(t, recorder.Body.String(), "internal error")
}

func TestGrokOAuthHTTP5xxCooldownSettingsHandlerPutRejectsInvalidPayloads(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "missing enabled", body: `{"cooldown_seconds":33}`},
		{name: "missing seconds", body: `{"enabled":true}`},
		{name: "wrong enabled type", body: `{"enabled":"true","cooldown_seconds":33}`},
		{name: "wrong seconds type", body: `{"enabled":true,"cooldown_seconds":"33"}`},
		{name: "below range while disabled", body: `{"enabled":false,"cooldown_seconds":0}`},
		{name: "above range", body: `{"enabled":true,"cooldown_seconds":7201}`},
		{name: "unknown field", body: `{"enabled":true,"cooldown_seconds":33,"source":"startup_config"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &grokHTTP5xxHandlerSettingRepo{}
			recorder := invokeGrokHTTP5xxSettingHandler(t, http.MethodPut, tt.body, func(h *SettingHandler, c *gin.Context) {
				h.UpdateGrokOAuthHTTP5xxCooldownSettings(c)
			}, repo)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, repo.setCalls)
		})
	}
}
