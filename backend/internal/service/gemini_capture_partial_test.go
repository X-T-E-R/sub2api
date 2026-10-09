package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

type partialNonStreamingWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *partialNonStreamingWriter) Header() http.Header { return w.header }

func (w *partialNonStreamingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *partialNonStreamingWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	n := len(p) / 2
	if n == 0 && len(p) > 0 {
		n = 1
	}
	_, _ = w.body.Write(p[:n])
	return n, io.ErrShortWrite
}

func TestGeminiCapture_ConvertedNonStreamingTapMarksPartialWrite(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.Background(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1)
	if capture == nil || !capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{}`)) {
		t.Fatal("expected active capture")
	}
	partialWriter := &partialNonStreamingWriter{header: make(http.Header)}
	c, _ := gin.CreateTestContext(partialWriter)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = c.Request.WithContext(WithGeminiCapture(c.Request.Context(), capture))
	convertedBody := []byte(`{"ok":true}`)
	writeClaudeNonStreamingResponse(c, convertedBody)
	if c.Writer.Status() != http.StatusOK || partialWriter.header.Get("Content-Type") != "application/json" {
		t.Fatalf("normal Gin status/header semantics changed: status=%d content_type=%q", c.Writer.Status(), partialWriter.header.Get("Content-Type"))
	}
	capture.Finish(c.Writer.Status())
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
	wantPrefix := string(convertedBody[:len(convertedBody)/2])
	if string(converted) != wantPrefix {
		t.Fatalf("partial prefix mismatch: got=%q want=%q", converted, wantPrefix)
	}
}
