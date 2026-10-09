package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiCapture_NativeGeminiForwardActivatesOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	cfg := &config.Config{Gateway: config.GatewayConfig{
		GeminiCaptureLeaseFile: leasePath,
		MaxLineSize:            defaultMaxLineSize,
	}}
	controller := NewGeminiCapture(cfg)

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	capture := controller.Begin(c.Request.Context(), c.Request.URL.Path, body, "match-session", GeminiCaptureTargetModel, false, 1)
	if capture == nil {
		t.Fatal("expected exact capture candidate")
	}
	requestCtx := WithGeminiCapture(c.Request.Context(), capture)
	c.Request = c.Request.WithContext(requestCtx)

	upstreamBody := []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1}}}\n\n")
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"native-response-id"}},
		Body:       io.NopCloser(bytes.NewReader(upstreamBody)),
	}}}
	svc := &AntigravityGatewayService{
		settingService: NewSettingService(&antigravitySettingRepoStub{}, cfg),
		tokenProvider:  &AntigravityTokenProvider{},
		httpUpstream:   upstream,
	}
	account := &Account{
		ID:          69,
		Name:        "native-oauth",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "test-access-token",
			"project_id":   "native-project",
			"model_mapping": map[string]any{
				GeminiCaptureTargetModel: GeminiCaptureTargetModel,
			},
		},
	}

	// This is the causal assertion: the pre-hook implementation reaches the
	// same upstream fake but leaves this request candidate inactive.
	result, err := svc.ForwardGemini(requestCtx, c, account, GeminiCaptureTargetModel, "generateContent", false, body, false)
	require.NoError(t, err)
	capture.mu.Lock()
	activated := capture.activated
	capture.mu.Unlock()
	require.True(t, activated, "native ForwardGemini must call Activate after wrapping; the old implementation leaves it false")
	require.NotNil(t, result)
	require.Equal(t, GeminiCaptureTargetModel, result.Model)
	require.Equal(t, GeminiCaptureTargetModel, result.UpstreamModel)
	require.Len(t, upstream.requestBodies, 1)
	capture.Finish(http.StatusOK)

	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the service hook, not a direct Activate call, must create the artifact")
	artifactDir := filepath.Join(outputDir, entries[0].Name())
	for _, rel := range []string{
		"inbound.bin",
		"gemini_request.bin",
		"attempts/001/request.bin",
		"attempts/001/upstream.bin",
		"converted.bin",
		"manifest.json",
	} {
		_, err := os.Stat(filepath.Join(artifactDir, filepath.FromSlash(rel)))
		require.NoError(t, err, rel)
	}

	geminiRequest, err := os.ReadFile(filepath.Join(artifactDir, "gemini_request.bin"))
	require.NoError(t, err)
	require.Contains(t, string(geminiRequest), `"project":"native-project"`)
	require.Contains(t, string(geminiRequest), `"model":"gemini-3.8-flash"`)
	require.NotContains(t, string(geminiRequest), "test-access-token")

	var wrapped map[string]any
	require.NoError(t, json.Unmarshal(upstream.requestBodies[0], &wrapped))
	require.Equal(t, "native-project", wrapped["project"])
	require.Equal(t, GeminiCaptureTargetModel, wrapped["model"])
	require.NotNil(t, wrapped["request"])

	upstreamCaptured, err := os.ReadFile(filepath.Join(artifactDir, "attempts/001/upstream.bin"))
	require.NoError(t, err)
	require.Equal(t, upstreamBody, upstreamCaptured, "normal credential-free SSE remains byte-for-byte identical")
	converted, err := os.ReadFile(filepath.Join(artifactDir, "converted.bin"))
	require.NoError(t, err)
	require.Contains(t, string(converted), `"candidates"`)

	manifestBytes, err := os.ReadFile(filepath.Join(artifactDir, "manifest.json"))
	require.NoError(t, err)
	var manifest geminiCaptureManifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, GeminiCaptureTargetModel, manifest.RequestModel)
	require.Equal(t, GeminiCaptureTargetModel, manifest.FinalModel)
	require.Equal(t, "activated", manifest.CaptureStage)
	require.Equal(t, AccountTypeOAuth, manifest.SelectedAccountType)
	require.Equal(t, 1, manifest.UpstreamAttempts)
	require.Equal(t, "upstream_eof", manifest.Outcome)
	require.False(t, manifest.HeadersRecorded)
}

func TestGeminiCapture_ActivationDiagnosticsDistinguishNotCalledAndRejectedType(t *testing.T) {
	newCandidate := func(t *testing.T) *GeminiCaptureRequest {
		t.Helper()
		outputDir := t.TempDir()
		leasePath := filepath.Join(t.TempDir(), "lease.json")
		writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
		controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
		capture := controller.Begin(newCaptureTestContext(), "/v1/messages", []byte(`{"contents":[]}`), "match-session", GeminiCaptureTargetModel, false, 1)
		if capture == nil {
			t.Fatal("expected exact capture candidate")
		}
		return capture
	}

	t.Run("not called", func(t *testing.T) {
		capture := newCandidate(t)
		capture.Finish(http.StatusOK)
		capture.mu.Lock()
		defer capture.mu.Unlock()
		require.Equal(t, "activate_not_called", capture.activationStage)
		require.Empty(t, capture.activationReason)
	})

	t.Run("rejected account type", func(t *testing.T) {
		capture := newCandidate(t)
		require.False(t, capture.Activate(&Account{ID: 70, Platform: PlatformAntigravity, Type: AccountTypeUpstream}, GeminiCaptureTargetModel, []byte(`{}`)))
		capture.Finish(http.StatusOK)
		capture.mu.Lock()
		defer capture.mu.Unlock()
		require.Equal(t, "activate_rejected", capture.activationStage)
		require.Equal(t, "account_type_upstream", capture.activationReason)
		require.Equal(t, AccountTypeUpstream, capture.selectedAccountType)
	})
}

func TestGeminiCapture_ExcludedSubtypeDoesNotCreateLocalFailureArtifact(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		status      int
	}{
		{name: "api key 400", accountType: AccountTypeAPIKey, status: http.StatusBadRequest},
		{name: "upstream 503", accountType: AccountTypeUpstream, status: http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outputDir := t.TempDir()
			leasePath := filepath.Join(t.TempDir(), "lease.json")
			writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
			controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
			capture := controller.Begin(newCaptureTestContext(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1)
			require.NotNil(t, capture)
			capture.MarkAntigravitySelected(101, tc.accountType)
			capture.Finish(tc.status)

			entries, err := os.ReadDir(outputDir)
			require.NoError(t, err)
			require.Empty(t, entries, "known excluded account subtype must not create a local-failure artifact")
			capture.mu.Lock()
			require.Equal(t, tc.accountType, capture.selectedAccountType)
			require.Equal(t, "activate_not_called", capture.activationStage)
			capture.mu.Unlock()
		})
	}
}

func TestGeminiCapture_NoAccount503StillCreatesLocalFailureArtifact(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(newCaptureTestContext(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1)
	require.NotNil(t, capture)
	capture.Finish(http.StatusServiceUnavailable)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(outputDir)
		if len(entries) == 1 {
			_, err := os.Stat(filepath.Join(outputDir, entries[0].Name(), "manifest.json"))
			if err == nil {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no-account 503 did not produce the existing local-failure artifact")
}

func TestGeminiCapture_ActivationDiagnosticsClearRejectedReasonOnSuccess(t *testing.T) {
	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeCaptureLease(t, leasePath, outputDir, true, time.Now().Add(time.Hour), "match-session")
	controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: leasePath}})
	capture := controller.Begin(newCaptureTestContext(), "/v1/messages", []byte(`{"model":"gemini-3.8-flash"}`), "match-session", GeminiCaptureTargetModel, false, 1)
	require.NotNil(t, capture)
	account := &Account{ID: 102, Platform: PlatformAntigravity, Type: AccountTypeOAuth}

	require.False(t, capture.Activate(account, "gemini-2.5-flash", []byte(`{"rejected":true}`)))
	capture.mu.Lock()
	require.Equal(t, "activate_rejected", capture.activationStage)
	require.Equal(t, "final_model_mismatch", capture.activationReason)
	capture.mu.Unlock()

	require.True(t, capture.Activate(account, GeminiCaptureTargetModel, []byte(`{"accepted":true}`)))
	capture.Finish(http.StatusOK)
	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	manifestBody, err := os.ReadFile(filepath.Join(outputDir, entries[0].Name(), "manifest.json"))
	require.NoError(t, err)
	var manifest geminiCaptureManifest
	require.NoError(t, json.Unmarshal(manifestBody, &manifest))
	require.Equal(t, "activated", manifest.CaptureStage)
	require.Empty(t, manifest.ActivationReason, "successful activation must clear a prior rejection reason")
}
