package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAntigravityEffortNativeCaptureFinalModel(t *testing.T) {
	effortTestSettings(t)
	output := t.TempDir()
	lease := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, lease, output, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: lease}})
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	capture := controller.Begin(newCaptureTestContext(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, false, 1)
	require.NotNil(t, capture)
	runEffortForward(t, GeminiCaptureTargetModel, "gemini", "", WithGeminiCapture(context.Background(), capture))
	capture.Finish(http.StatusOK)
	entries, err := os.ReadDir(output)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	artifact := filepath.Join(output, entries[0].Name())
	wrapped, err := os.ReadFile(filepath.Join(artifact, "gemini_request.bin"))
	require.NoError(t, err)
	require.Contains(t, string(wrapped), `"model":"gemini-3.8-flash-medium"`)
	data, err := os.ReadFile(filepath.Join(artifact, "manifest.json"))
	require.NoError(t, err)
	var manifest geminiCaptureManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	require.Equal(t, GeminiCaptureTargetModel, manifest.RequestModel)
	require.Equal(t, GeminiCaptureTargetModel+"-medium", manifest.FinalModel)
	require.Equal(t, "activated", manifest.CaptureStage)
	require.Equal(t, 1, manifest.UpstreamAttempts)
}

func TestAntigravityEffortIndependentRateScope(t *testing.T) {
	svc, _ := effortTestSettings(t, "ultra")
	account := effortTestAccount(map[string]string{"custom": "custom-{effort}", "custom-image": "custom-image"})
	inFlight := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"ultra"}`))
	require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), defaultAntigravityModelEffortSettings()))
	require.Equal(t, []string{"custom-ultra", "custom"}, antigravityModelRateLimitKeysForAccount(inFlight, account, "custom-ultra"))
	require.Equal(t, []string{"custom-image"}, antigravityModelRateLimitKeysForAccount(inFlight, account, "custom-image"))
	now := time.Now()
	setAccountModelRateLimitSnapshot(account, "custom", now.Add(time.Minute), "fixture", now)
	require.False(t, account.isModelRateLimitedWithContext(context.Background(), "custom-image"))
	require.True(t, account.isModelRateLimitedWithContext(inFlight, "custom"))
}
