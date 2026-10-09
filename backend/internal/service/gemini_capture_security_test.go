package service

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestGeminiCapture_RedactionParseFailureIsFailClosed(t *testing.T) {
	redacted, fields, ok := redactGeminiCaptureJSON([]byte(`{"accessToken":"must-not-fallback"`))
	if ok || len(redacted) != 0 || len(fields) != 0 {
		t.Fatalf("parse failure must not return raw fallback: redacted=%q fields=%v ok=%v", redacted, fields, ok)
	}
}

func TestGeminiCapture_SSEControlAndMalformedCredentialLinesAreSafe(t *testing.T) {
	for _, raw := range [][]byte{[]byte("data:   \r\n"), []byte("data: [DONE]\r\n"), []byte(":keepalive\r\n"), []byte("\r\n")} {
		got, _, changed, safe := redactGeminiCaptureLine(raw)
		if !safe || changed || string(got) != string(raw) {
			t.Fatalf("SSE control line changed or failed: raw=%q got=%q changed=%v safe=%v", raw, got, changed, safe)
		}
	}
	malformed := []byte("data: {\"access-token\":\"REVIEW_FAKE_AUTH\",\n")
	got, _, _, safe := redactGeminiCaptureLine(malformed)
	if safe && strings.Contains(string(got), "REVIEW_FAKE_AUTH") {
		t.Fatalf("malformed credential escaped fail-closed path: %q", got)
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

func TestGeminiCapture_WriterSlotReleasesAfterWriteSyncClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	q := newGeminiCaptureQueue()
	if !q.writerAdmitted {
		t.Fatal("writer slot unavailable")
	}
	q.addFile("body", "writer.bin", file)
	if !q.enqueue("body", []byte("x"), 1) {
		t.Fatal("enqueue failed")
	}
	q.close()
	next := newGeminiCaptureQueue()
	if !next.writerAdmitted {
		next.close()
		t.Fatal("writer slot was not released after queue close")
	}
	next.close()
}

func TestGeminiCapture_WriterAdmissionIsBounded(t *testing.T) {
	queues := make([]*geminiCaptureQueue, 0, geminiCaptureWriterSlots)
	for i := 0; i < geminiCaptureWriterSlots; i++ {
		queues = append(queues, newGeminiCaptureQueue())
	}
	extra := newGeminiCaptureQueue()
	if extra.writerAdmitted {
		extra.close()
		t.Fatal("writer admission exceeded shared slot bound")
	}
	for _, q := range queues {
		q.close()
	}
}

func TestGeminiCapture_CancelledManifestCannotLateWriteCompleteSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	task := &geminiCaptureManifestTask{path: path, body: []byte(`{"incomplete":false}`), cancelled: make(chan struct{}), done: make(chan error, 1)}
	close(task.cancelled)
	if err := writeGeminiCaptureManifestTask(task); err == nil {
		t.Fatal("cancelled manifest task unexpectedly succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancelled task left a late manifest: err=%v", err)
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

func TestGeminiCapture_ExistingOutputConsumesSharedBudget(t *testing.T) {
	outputDir := t.TempDir()
	existing := filepath.Join(outputDir, "existing-artifact.bin")
	file, err := os.OpenFile(existing, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(geminiCaptureOutputBytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.TODO(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil {
		t.Fatal("expected exact lease candidate")
	}
	if capture.Activate(&Account{ID: 1, Platform: PlatformAntigravity, Type: "oauth"}, GeminiCaptureTargetModel, []byte(`{}`)) {
		t.Fatal("existing output at shared budget must block a new artifact")
	}
}

func TestGeminiCapture_SharedBudgetSerializesConcurrentReservations(t *testing.T) {
	budget := &geminiCaptureOutputBudget{ready: true, used: geminiCaptureOutputBytes - 64}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if budget.reserve(48) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("shared output budget was not atomic across concurrent requests: accepted=%d", accepted)
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

func TestGeminiCapture_LocalFailureFinishUsesCachedLeaseAndReturnsBounded(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(context.TODO(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, true, 1)
	if capture == nil {
		t.Fatal("expected exact lease candidate")
	}
	capture.MarkAntigravitySelected(42)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	capture.Finish(http.StatusServiceUnavailable)
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("local-failure Finish performed blocking storage work: %v", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(outputDir)
		if len(entries) > 0 {
			manifest := filepath.Join(outputDir, entries[0].Name(), "manifest.json")
			if _, err := os.Stat(manifest); err == nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bounded local-failure worker did not finish artifact")
}
