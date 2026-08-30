//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type grokHTTP5xxSettingRepo struct {
	mockSettingRepo
	value    string
	readErr  error
	setKey   string
	setValue string
}

func newGrokHTTP5xxSettingRepo() *grokHTTP5xxSettingRepo {
	return &grokHTTP5xxSettingRepo{mockSettingRepo: *newMockSettingRepo()}
}

func (r *grokHTTP5xxSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.getValueCalls++
	if r.readErr != nil {
		return "", r.readErr
	}
	return r.value, nil
}

func (r *grokHTTP5xxSettingRepo) Set(_ context.Context, key, value string) error {
	r.setKey = key
	r.setValue = value
	r.value = value
	return nil
}

func TestGetGrokOAuthHTTP5xxCooldownSettingsEffectiveSourceAndFallbacks(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Grok.OAuthHTTP5xxCooldownDisabled = true
	cfg.Gateway.Grok.OAuthHTTP5xxCooldownSeconds = 47

	tests := []struct {
		name       string
		value      string
		readErr    error
		wantSource string
		wantEnable bool
		wantSecs   int
		wantReason string
	}{
		{name: "valid runtime", value: `{"enabled":true,"cooldown_seconds":19}`, wantSource: GrokOAuthHTTP5xxCooldownSourceRuntimeSetting, wantEnable: true, wantSecs: 19},
		{name: "missing", readErr: ErrSettingNotFound, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47},
		{name: "empty", value: "", wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "malformed", value: `{`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "wrong type", value: `{"enabled":"true","cooldown_seconds":19}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "missing enabled", value: `{"cooldown_seconds":19}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "missing seconds", value: `{"enabled":true}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "below range even disabled", value: `{"enabled":false,"cooldown_seconds":0}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "above range", value: `{"enabled":true,"cooldown_seconds":7201}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "unknown field", value: `{"enabled":true,"cooldown_seconds":19,"source":"runtime_setting"}`, wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "invalid_setting"},
		{name: "read error", readErr: errors.New("database unavailable"), wantSource: GrokOAuthHTTP5xxCooldownSourceStartupConfig, wantEnable: false, wantSecs: 47, wantReason: "db_read_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			repo := newGrokHTTP5xxSettingRepo()
			repo.value = tt.value
			repo.readErr = tt.readErr
			svc := NewSettingService(repo, cfg)

			settings, err := svc.GetGrokOAuthHTTP5xxCooldownSettings(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.wantSource, settings.Source)
			require.Equal(t, tt.wantEnable, settings.Enabled)
			require.Equal(t, tt.wantSecs, settings.CooldownSeconds)
			if tt.wantReason == "" {
				require.NotContains(t, logs.String(), "grok_oauth_http_5xx_cooldown_settings_fallback")
			} else {
				require.Contains(t, logs.String(), `"msg":"grok_oauth_http_5xx_cooldown_settings_fallback"`)
				require.Contains(t, logs.String(), `"reason":"`+tt.wantReason+`"`)
				require.NotContains(t, logs.String(), "database unavailable")
			}
		})
	}
}

func TestSetGrokOAuthHTTP5xxCooldownSettingsStrictPersistence(t *testing.T) {
	repo := newGrokHTTP5xxSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	updated, err := svc.SetGrokOAuthHTTP5xxCooldownSettings(context.Background(), &GrokOAuthHTTP5xxCooldownSettings{
		Enabled:         false,
		CooldownSeconds: 1,
		Source:          "must_not_persist",
	})
	require.NoError(t, err)
	require.Equal(t, &GrokOAuthHTTP5xxCooldownSettings{
		Enabled:         false,
		CooldownSeconds: 1,
		Source:          GrokOAuthHTTP5xxCooldownSourceRuntimeSetting,
	}, updated)
	require.Equal(t, SettingKeyGrokOAuthHTTP5xxCooldownSettings, repo.setKey)
	require.Equal(t, `{"enabled":false,"cooldown_seconds":1}`, repo.setValue)

	settings, err := svc.GetGrokOAuthHTTP5xxCooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 1, settings.CooldownSeconds)
	require.Equal(t, GrokOAuthHTTP5xxCooldownSourceRuntimeSetting, settings.Source)

	for _, seconds := range []int{0, -1, 7201} {
		_, err := svc.SetGrokOAuthHTTP5xxCooldownSettings(context.Background(), &GrokOAuthHTTP5xxCooldownSettings{
			Enabled:         false,
			CooldownSeconds: seconds,
		})
		require.ErrorContains(t, err, "cooldown_seconds must be between 1-7200")
	}
	_, err = svc.SetGrokOAuthHTTP5xxCooldownSettings(context.Background(), nil)
	require.Error(t, err)
}
