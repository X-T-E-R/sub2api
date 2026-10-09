package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type captureChunkReader struct {
	data []byte
	off  int
	size int
}

func (r *captureChunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := r.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	copy(p[:n], r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

func (r *captureChunkReader) Close() error { return nil }

func writeCaptureLease(t *testing.T, path, outputDir string, enabled bool, expires time.Time, userID string) {
	t.Helper()
	body, err := json.Marshal(GeminiCaptureLease{
		Enabled:         enabled,
		MetadataUserIDs: []string{userID},
		Model:           GeminiCaptureTargetModel,
		ExpiresAt:       expires.UTC().Format(time.RFC3339Nano),
		OutputDir:       outputDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func newCaptureTestContext() context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "gateway-request-id")
	return context.WithValue(ctx, ctxkey.ClientRequestID, "client-request-id")
}

func TestGeminiCapture_DefaultOffAndExactLeaseMatching(t *testing.T) {
	t.Parallel()
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	future := time.Now().Add(time.Hour)
	writeCaptureLease(t, leasePath, outputDir, true, future, "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})

	body := []byte("{\r\n  \"model\": \"gemini-3.8-flash\",\r\n  \"metadata\": {\"user_id\": \"match-session\"}\r\n}\r\n")
	if got := controller.Begin(newCaptureTestContext(), "/v1/messages", body, "other-session", GeminiCaptureTargetModel, true, 1); got != nil {
		t.Fatal("unrelated metadata user must not arm capture")
	}
	if got := controller.Begin(newCaptureTestContext(), "/v1/messages?x=1", body, "match-session", GeminiCaptureTargetModel, true, 1); got != nil {
		t.Fatal("non-exact request path must not arm capture")
	}
	if got := controller.Begin(newCaptureTestContext(), "/v1/messages", body, "match-session", "gemini-3.8-flash-preview", true, 1); got != nil {
		t.Fatal("non-exact request model must not arm capture")
	}
	if got := NewGeminiCapture(&config.Config{}).Begin(newCaptureTestContext(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, true, 1); got != nil {
		t.Fatal("missing lease path must be default-off")
	}
}

func TestGeminiCapture_PreservesRawBytesAndRecordsConvertedWire(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	inbound := []byte("{\r\n  \"model\": \"gemini-3.8-flash\",\r\n  \"metadata\": {\"user_id\": \"match-session\"}\r\n}\r\n")
	capture := controller.Begin(newCaptureTestContext(), "/v1/messages", inbound, "match-session", GeminiCaptureTargetModel, true, 42)
	if capture == nil {
		t.Fatal("expected exact lease match")
	}
	account := &Account{ID: 77, Platform: PlatformAntigravity, Type: "oauth"}
	if !capture.Activate(account, GeminiCaptureTargetModel, []byte(`{"contents":[{"role":"user"}]}`)) {
		t.Fatal("expected AG activation")
	}
	attempt := capture.BeginUpstreamAttempt(account.ID, 99, []byte(`{"request":"wrapped"}`))
	if attempt == nil {
		t.Fatal("expected upstream attempt")
	}
	raw := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"thoughtSignature\":\"opaque\"},{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3}}\r\n:comment\r\n\r\n")
	response := attempt.AttachResponse(&http.Response{
		StatusCode: 200,
		Header:     http.Header{"X-Request-Id": []string{"provider-response-id"}},
		Body:       &captureChunkReader{data: raw, size: 5},
	})
	if got, err := io.ReadAll(response.Body); err != nil || !strings.EqualFold(string(got), string(raw)) {
		t.Fatalf("raw upstream bytes changed: err=%v got=%q", err, got)
	}
	_ = response.Body.Close()
	converted := []byte("event: message_start\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n")
	capture.RecordConvertedBody(converted)
	capture.Finish(200)

	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one artifact directory, err=%v entries=%d", err, len(entries))
	}
	artifactDir := filepath.Join(outputDir, entries[0].Name())
	manifestBody, err := os.ReadFile(filepath.Join(artifactDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest geminiCaptureManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.UpstreamAttempts != 1 || manifest.Outcome != "upstream_eof" {
		t.Fatalf("unexpected manifest attempts/outcome: %+v", manifest)
	}
	if len(manifest.UpstreamFinishReasons) != 1 || manifest.UpstreamFinishReasons[0] != "STOP" {
		t.Fatalf("finishReason not preserved: %#v", manifest.UpstreamFinishReasons)
	}
	if manifest.Usage["promptTokenCount"] != float64(3) {
		t.Fatalf("usage not preserved: %#v", manifest.Usage)
	}
	if manifest.Attempts[0].UpstreamRequestID != "provider-response-id" || manifest.Attempts[0].AccountID != account.ID || manifest.Attempts[0].GroupID != 99 {
		t.Fatalf("attempt identity mismatch: %#v", manifest.Attempts)
	}
	gotInbound, err := os.ReadFile(filepath.Join(artifactDir, "inbound.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotInbound) != string(inbound) {
		t.Fatalf("inbound bytes changed: got=%q want=%q", gotInbound, inbound)
	}
	gotRaw, err := os.ReadFile(filepath.Join(artifactDir, "attempts", "001", "upstream.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRaw) != string(raw) {
		t.Fatalf("upstream bytes changed: got=%q want=%q", gotRaw, raw)
	}
	gotConverted, err := os.ReadFile(filepath.Join(artifactDir, "converted.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotConverted) != string(converted) {
		t.Fatalf("converted wire changed: got=%q want=%q", gotConverted, converted)
	}
	if manifest.HeadersRecorded {
		t.Fatal("HTTP headers must never be recorded")
	}
	if runtime.GOOS != "windows" {
		if info, err := entries[0].Info(); err == nil && info.Mode().Perm()&0077 != 0 {
			t.Fatalf("artifact directory is not private: %o", info.Mode().Perm())
		}
	}
}

func TestGeminiCapture_RedactsCredentialFieldsButRetainsThoughtSignature(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	body := []byte(`{"model":"gemini-3.8-flash","metadata":{"user_id":"match-session"},"api_key":"do-not-store","thoughtSignature":"keep-me"}`)
	capture := controller.Begin(context.Background(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, false, 1)
	if capture == nil {
		t.Fatal("expected capture")
	}
	if !capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{"thoughtSignature":"keep-me"}`)) {
		t.Fatal("expected activation")
	}
	capture.Finish(503)
	entries, err := os.ReadDir(outputDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected artifact: err=%v entries=%d", err, len(entries))
	}
	artifactDir := filepath.Join(outputDir, entries[0].Name())
	stored, err := os.ReadFile(filepath.Join(artifactDir, "inbound.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "do-not-store") || !strings.Contains(string(stored), "keep-me") || !strings.Contains(string(stored), "[REDACTED]") {
		t.Fatalf("credential redaction/signature retention failed: %s", stored)
	}
}

func TestGeminiCapture_LeaseExpiryAndDisarmDisableNewRequests(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, false, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	body := []byte(`{"model":"gemini-3.8-flash","metadata":{"user_id":"match-session"}}`)
	if capture := controller.Begin(context.Background(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, true, 1); capture != nil {
		t.Fatal("disabled lease must not capture")
	}
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(-time.Minute), "match-session")
	if capture := controller.Begin(context.Background(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, true, 1); capture != nil {
		t.Fatal("expired lease must not capture")
	}
}
