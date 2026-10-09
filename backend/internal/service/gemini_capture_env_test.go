package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_LeasePathEnvironmentFallback(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	t.Setenv(geminiCaptureLeaseEnv, leasePath)
	controller := NewGeminiCapture(&config.Config{})
	if capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1); capture == nil {
		t.Fatal("expected GATEWAY_GEMINI_CAPTURE_LEASE_FILE fallback to arm a matching request")
	}
}
