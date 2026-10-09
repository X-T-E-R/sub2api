package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_NativeGeminiForwardAllowsAPIKeyAccount(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil {
		t.Fatal("expected exact capture candidate")
	}
	if !capture.Activate(&Account{ID: 69, Platform: PlatformAntigravity, Type: AccountTypeAPIKey}, GeminiCaptureTargetModel, []byte(`{"wrapped":true}`)) {
		t.Fatal("native Gemini Forward path must activate capture for API-key-backed Antigravity accounts")
	}
	capture.Finish(200)
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected artifact directory: err=%v entries=%d", err, len(entries))
	}
}
