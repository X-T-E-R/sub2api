package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadAntigravityGeminiMessagesConfig(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		disabled string
		models   string
		want     GatewayAntigravityGeminiMessagesConfig
	}{
		{name: "default enabled for Gemini"},
		{
			name: "YAML opt out",
			yaml: "gateway:\n  antigravity_gemini_messages:\n    disabled: true\n    models: [gemini-3.8-flash]\n",
			want: GatewayAntigravityGeminiMessagesConfig{Disabled: true, Models: []string{"gemini-3.8-flash"}},
		},
		{
			name:     "environment opt out",
			disabled: "true",
			models:   "gemini-3.8-flash,gemini-2.5-flash",
			want: GatewayAntigravityGeminiMessagesConfig{
				Disabled: true, Models: []string{"gemini-3.8-flash", "gemini-2.5-flash"},
			},
		},
		{
			name:     "environment can reenable YAML disabled policy",
			yaml:     "gateway:\n  antigravity_gemini_messages:\n    disabled: true\n",
			disabled: "false",
			models:   "gemini-3.8-flash",
			want:     GatewayAntigravityGeminiMessagesConfig{Models: []string{"gemini-3.8-flash"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_DISABLED", tt.disabled)
			t.Setenv("GATEWAY_ANTIGRAVITY_GEMINI_MESSAGES_MODELS", tt.models)
			// Use an explicit synthetic file instead of searching the host's config paths.
			configFile := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(configFile, []byte(tt.yaml), 0o600))
			t.Setenv("CONFIG_FILE", configFile)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tt.want.Disabled, cfg.Gateway.AntigravityGeminiMessages.Disabled)
			require.Equal(t, len(tt.want.Models), len(cfg.Gateway.AntigravityGeminiMessages.Models))
			if len(tt.want.Models) > 0 {
				require.Equal(t, tt.want.Models, cfg.Gateway.AntigravityGeminiMessages.Models)
			}
		})
	}
}
