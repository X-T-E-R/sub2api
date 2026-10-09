package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_EOFWithFinalDataChunkParsesTailMetadata(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil || !capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{}`)) {
		t.Fatal("expected active capture")
	}
	attempt := capture.BeginUpstreamAttempt(1, 1, []byte(`{}`))
	tail := []byte(`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7}}`)
	resp := attempt.AttachResponse(&http.Response{StatusCode: http.StatusOK, Body: &captureEOFReader{data: tail}})
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	capture.Finish(http.StatusOK)

	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected artifact: err=%v entries=%d", err, len(entries))
	}
	manifestBody, err := os.ReadFile(filepath.Join(outputDir, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest geminiCaptureManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.UpstreamFinishReasons) != 1 || manifest.UpstreamFinishReasons[0] != "STOP" {
		t.Fatalf("final EOF chunk finishReason lost: %#v", manifest.UpstreamFinishReasons)
	}
	if manifest.Usage["promptTokenCount"] != float64(7) {
		t.Fatalf("final EOF chunk usage lost: %#v", manifest.Usage)
	}
}
