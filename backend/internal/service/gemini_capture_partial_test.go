package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_ConvertedPartialWriteIsIncomplete(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1)
	if capture == nil || !capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{}`)) {
		t.Fatal("expected active capture")
	}
	capture.RecordConvertedWrite([]byte("converted"), 3, errors.New("short write"))
	capture.Finish(200)
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected artifact: err=%v entries=%d", err, len(entries))
	}
	artifact := filepath.Join(outputDir, entries[0].Name())
	manifestBody, err := os.ReadFile(filepath.Join(artifact, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest geminiCaptureManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Incomplete || !strings.Contains(strings.Join(manifest.IncompleteReasons, ","), "client_write_partial_or_failed") {
		t.Fatalf("partial write was not marked incomplete: %+v", manifest)
	}
	converted, err := os.ReadFile(filepath.Join(artifact, "converted.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(converted) != "con" {
		t.Fatalf("partial prefix mismatch: %q", converted)
	}
}
