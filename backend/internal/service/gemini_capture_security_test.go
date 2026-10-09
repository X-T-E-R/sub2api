package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeminiCapture_RedactionParseFailureIsFailClosed(t *testing.T) {
	redacted, fields, ok := redactGeminiCaptureJSON([]byte(`{"accessToken":"must-not-fallback"`))
	if ok || len(redacted) != 0 || len(fields) != 0 {
		t.Fatalf("parse failure must not return raw fallback: redacted=%q fields=%v ok=%v", redacted, fields, ok)
	}
}

func TestGeminiCapture_CredentialFieldMatcherCoversTokenSpellings(t *testing.T) {
	for _, key := range []string{"accessToken", "refreshToken", "token", "apiKey", "authorization", "clientSecret", "private_key"} {
		if !isCredentialCaptureField(key) {
			t.Fatalf("credential field not covered: %q", key)
		}
	}
	for _, key := range []string{"thoughtSignature", "thought_signature", "opaque", "providerSignature"} {
		if isCredentialCaptureField(key) {
			t.Fatalf("provider carrier must be retained: %q", key)
		}
	}
}

func TestGeminiCapture_CloseHasBoundedWaitOnStalledWorker(t *testing.T) {
	q := &geminiCaptureQueue{items: make(chan geminiCaptureChunk), files: make(map[string]*geminiCaptureFile), incomplete: make(map[string]struct{})}
	q.wg.Add(1)
	done := make(chan struct{})
	go func() {
		q.close()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("close unexpectedly waited for stalled worker")
	case <-time.After(50 * time.Millisecond):
	}
	q.wg.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled worker cleanup did not finish")
	}
}

func TestGeminiCapture_DiskAndParserBudgetsStopCapture(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "body.bin")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	q := newGeminiCaptureQueue()
	q.addFile("body", "body.bin", file)
	q.mu.Lock()
	q.totalBytes = geminiCaptureDiskBytes - 1
	q.mu.Unlock()
	if q.enqueue("body", []byte("xx"), 2) {
		t.Fatal("disk budget must reject an over-budget append")
	}
	q.close()
	q.mu.Lock()
	_, diskMarked := q.incomplete["disk_quota"]
	q.mu.Unlock()
	if !diskMarked {
		t.Fatal("disk quota failure was not marked")
	}

	capture := &GeminiCaptureRequest{
		incompleteReason: make(map[string]struct{}),
		terminals:        make(map[string]struct{}),
	}
	capture.parseUpstreamChunk(make([]byte, geminiCaptureParserMaxBytes+1))
	capture.mu.Lock()
	_, parserMarked := capture.incompleteReason["parser_quota"]
	capture.mu.Unlock()
	if !parserMarked {
		t.Fatal("parser quota failure was not marked")
	}
}
