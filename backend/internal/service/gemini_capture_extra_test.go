package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_DisarmStopsInFlightWritesAndMarksPartial(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil || !capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{"contents":[]}`)) {
		t.Fatal("expected active capture")
	}
	writeCaptureLease(t, leasePath, outputDir, false, time.Now().Add(time.Hour), "match-session")
	capture.lastLeaseCheck = time.Time{}
	capture.RecordConvertedBody([]byte("must-not-be-appended-after-disarm"))
	capture.Finish(200)
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one partial artifact, err=%v entries=%d", err, len(entries))
	}
	manifestBody, err := os.ReadFile(filepath.Join(outputDir, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest geminiCaptureManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Incomplete {
		t.Fatalf("disarmed in-flight capture must be marked incomplete: %+v", manifest)
	}
}

func TestGeminiCapture_InvalidOutputAndQueueQuotaFailOpen(t *testing.T) {
	root := t.TempDir()
	invalidOutput := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(invalidOutput, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(root, "lease.json")
	writeCaptureLease(t, leasePath, invalidOutput, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil {
		t.Fatal("lease itself should still match before output validation")
	}
	if capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{}`)) {
		t.Fatal("invalid output path must fail open without activation")
	}
	capture.Finish(503)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("invalid output path created an artifact: %s", entry.Name())
		}
	}

	q := newGeminiCaptureQueue()
	filePath := filepath.Join(root, "queue.bin")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	q.addFile("file", "queue.bin", file)
	if q.enqueue("file", make([]byte, geminiCaptureQueueBytes+1), geminiCaptureQueueBytes+1) {
		t.Fatal("queue quota must fail open")
	}
	q.close()
}
