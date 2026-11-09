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
	"github.com/stretchr/testify/require"
)

func writeGeneralizedCaptureLease(t *testing.T, path string, lease GeminiCaptureLease) {
	t.Helper()
	if lease.OutputDir != "" {
		if info, err := os.Stat(lease.OutputDir); err == nil && info.IsDir() {
			require.NoError(t, os.Chmod(lease.OutputDir, 0700))
		}
	}
	body, err := json.Marshal(lease)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(body, '\n'), 0600))
}

func TestGeminiCapture_GeneralizedLeaseAndWebSearchModelWiring(t *testing.T) {
	gin.SetMode(gin.TestMode)

	outputDir := t.TempDir()
	leasePath := filepath.Join(t.TempDir(), "lease.json")
	writeGeneralizedCaptureLease(t, leasePath, GeminiCaptureLease{
		Enabled:   true,
		Models:    []string{"gemini-4-argon", "gemini-3.1-flash-lite"},
		APIKeyIDs: []int64{501},
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		OutputDir: outputDir,
	})

	cfg := &config.Config{Gateway: config.GatewayConfig{
		GeminiCaptureLeaseFile: leasePath,
		MaxLineSize:            defaultMaxLineSize,
	}}
	controller := NewGeminiCapture(cfg)

	settingRepo := &antigravitySettingRepoStub{values: map[string]string{
		SettingKeyAntigravityWebSearchModel: "gemini-3.1-flash-lite",
	}}
	settingSvc := NewSettingService(settingRepo, cfg)
	settingSvc.LoadAntigravityWebSearchModelSettings(context.Background())
	require.Equal(t, "gemini-3.1-flash-lite", settingSvc.GetAntigravityWebSearchModel(context.Background()))

	account := &Account{
		ID:          88,
		Name:        "argon-oauth",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 2,
		Credentials: map[string]any{
			"access_token": "oauth-secret-access-token",
			"project_id":   "argon-proj-1",
			"model_mapping": map[string]any{
				"gemini-4-argon-high": "gemini-4-argon-high",
			},
		},
	}

	// 1. Wrong API key ID is ignored even when model matches.
	unrelatedInbound := []byte(`{"model":"gemini-4-argon-high","messages":[{"role":"user","content":"hi"}]}`)
	require.Nil(t, controller.Begin(newCaptureTestContext(), "/v1/messages", unrelatedInbound, "", "gemini-4-argon-high", false, 7, 999))

	// 2. Normal Argon request with business fields named "token"/"api_key"/"secret" inside messages/tools:
	//    - Must NOT switch model to gemini-3.1-flash-lite (stays gemini-4-argon-high, requestType="agent").
	//    - Must preserve business content byte-for-byte without false-positive credential redaction.
	normalInbound := []byte("{\n  \"model\": \"gemini-4-argon-high\",\n  \"max_tokens\": 1024,\n  \"system\": [{\"type\": \"text\", \"text\": \"Keep business token/secret/api_key intact\"}],\n  \"tools\": [\n    {\n      \"name\": \"check_auth_config\",\n      \"description\": \"Inspect config\",\n      \"input_schema\": {\n        \"type\": \"object\",\n        \"properties\": {\n          \"token\": {\"type\": \"string\"},\n          \"api_key\": {\"type\": \"string\"},\n          \"secret\": {\"type\": \"string\"}\n        }\n      }\n    }\n  ],\n  \"messages\": [\n    {\n      \"role\": \"user\",\n      \"content\": [\n        {\"type\": \"text\", \"text\": \"Check auth config\"}\n      ]\n    },\n    {\n      \"role\": \"assistant\",\n      \"content\": [\n        {\"type\": \"thinking\", \"thinking\": \"step 1\", \"signature\": \"sig-argon-1\"},\n        {\"type\": \"tool_use\", \"id\": \"toolu_1\", \"name\": \"check_auth_config\", \"input\": {\"token\": \"biz-token-value\", \"api_key\": \"biz-key-value\"}}\n      ]\n    },\n    {\n      \"role\": \"user\",\n      \"content\": [\n        {\"type\": \"tool_result\", \"tool_use_id\": \"toolu_1\", \"content\": \"{\\\"password\\\":\\\"biz-pass\\\"}\"}\n      ]\n    }\n  ]\n}")

	normalUpstreamSSE := []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thought\":true,\"text\":\"thinking...\",\"thoughtSignature\":\"sig-upstream-1\"},{\"text\":\"Done checking token.\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":6,\"totalTokenCount\":18}}}\n\n")
	// 3. Explicit web_search control request (empty candidate / no usable text return):
	//    - Must switch wire model to configured "gemini-3.1-flash-lite" and requestType="web_search".
	//    - Manifest must record request_model="claude-sonnet-4-6", mapped_model="gemini-4-argon-high", final_model="gemini-3.1-flash-lite", wire_request_type="web_search".
	//    - Empty upstream candidate return is preserved byte-for-byte in upstream.bin and converted.bin.
	searchInbound := []byte("{\n  \"model\": \"claude-sonnet-4-6\",\n  \"max_tokens\": 1024,\n  \"tools\": [\n    {\"type\": \"web_search_20250305\", \"name\": \"web_search\"}\n  ],\n  \"messages\": [\n    {\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"Search sub2api release notes\"}]}\n  ]\n}")
	emptySearchUpstreamSSE := []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"groundingMetadata\":{\"webSearchQueries\":[\"sub2api release notes\"]}}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":0,\"totalTokenCount\":9}}}\n\n")

	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"req-argon-normal"}},
			Body:       io.NopCloser(bytes.NewReader(normalUpstreamSSE)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"req-search-empty"}},
			Body:       io.NopCloser(bytes.NewReader(emptySearchUpstreamSSE)),
		},
	}}

	svc := &AntigravityGatewayService{
		settingService: settingSvc,
		tokenProvider:  &AntigravityTokenProvider{},
		httpUpstream:   upstream,
	}

	// Execute Request 1: Normal Argon (no metadata.user_id, matched by model "gemini-4-argon" + apiKeyID 501)
	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	c1.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(normalInbound))
	c1.Request.Header.Set("Authorization", "Bearer sk-ant-client-secret-should-never-be-recorded")
	cap1 := controller.Begin(newCaptureTestContext(), "/v1/messages", normalInbound, "", "gemini-4-argon-high", false, 7, 501)
	require.NotNil(t, cap1, "Argon -high variant without metadata.user_id must match lease")
	ctx1 := WithGeminiCapture(c1.Request.Context(), cap1)
	c1.Request = c1.Request.WithContext(ctx1)

	res1, err := svc.Forward(ctx1, c1, account, normalInbound, false)
	require.NoError(t, err)
	require.NotNil(t, res1)
	cap1.Finish(w1.Code)

	// Execute Request 2: Explicit web_search (requestModel="claude-sonnet-4-6" deferred at Begin, mapped to "gemini-4-argon-high", transformed to wire "gemini-3.1-flash-lite")
	account.Credentials["model_mapping"] = map[string]any{
		"claude-sonnet-4-6": "gemini-4-argon-high",
	}
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(searchInbound))
	cap2 := controller.Begin(newCaptureTestContext(), "/v1/messages", searchInbound, "", "claude-sonnet-4-6", false, 7, 501)
	require.NotNil(t, cap2, "web_search request must arm deferred candidate even when requestModel is claude-sonnet-4-6")
	ctx2 := WithGeminiCapture(c2.Request.Context(), cap2)
	c2.Request = c2.Request.WithContext(ctx2)

	res2, err := svc.Forward(ctx2, c2, account, searchInbound, false)
	require.NoError(t, err)
	require.NotNil(t, res2)
	cap2.Finish(w2.Code)

	entries, err := os.ReadDir(outputDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	var normalManifest, searchManifest geminiCaptureManifest
	var normalDir, searchDir string
	for _, entry := range entries {
		dir := filepath.Join(outputDir, entry.Name())
		mBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		require.NoError(t, err)
		var m geminiCaptureManifest
		require.NoError(t, json.Unmarshal(mBytes, &m))
		if m.RequestModel == "gemini-4-argon-high" {
			normalManifest = m
			normalDir = dir
		} else if m.RequestModel == "claude-sonnet-4-6" {
			searchManifest = m
			searchDir = dir
		}
	}
	require.NotEmpty(t, normalDir)
	require.NotEmpty(t, searchDir)

	// Verify Artifact 1 (Normal Argon):
	require.Equal(t, "gemini-4-argon-high", normalManifest.RequestModel)
	require.Equal(t, "gemini-4-argon-high", normalManifest.MappedModel)
	require.Equal(t, "gemini-4-argon-high", normalManifest.FinalModel)
	require.Equal(t, "agent", normalManifest.WireRequestType)
	require.Equal(t, int64(501), normalManifest.APIKeyID)
	require.False(t, normalManifest.Incomplete, "business fields named token/api_key/secret must not trigger credential_redaction_applied")
	require.Empty(t, normalManifest.RedactedFields)
	require.False(t, normalManifest.HeadersRecorded)

	gotNormalInbound, err := os.ReadFile(filepath.Join(normalDir, "inbound.bin"))
	require.NoError(t, err)
	require.Equal(t, string(normalInbound), string(gotNormalInbound), "inbound.bin must be preserved byte-for-byte")

	gotNormalGeminiReq, err := os.ReadFile(filepath.Join(normalDir, "gemini_request.bin"))
	require.NoError(t, err)
	require.Contains(t, string(gotNormalGeminiReq), `"model":"gemini-4-argon-high"`)
	require.Contains(t, string(gotNormalGeminiReq), `"requestType":"agent"`)
	require.Contains(t, string(gotNormalGeminiReq), `"biz-token-value"`)
	require.Contains(t, string(gotNormalGeminiReq), `"biz-key-value"`)
	require.NotContains(t, string(gotNormalGeminiReq), "[REDACTED]")
	require.NotContains(t, string(gotNormalGeminiReq), "oauth-secret-access-token")
	require.NotContains(t, string(gotNormalGeminiReq), "sk-ant-client-secret")

	gotNormalUpstream, err := os.ReadFile(filepath.Join(normalDir, "attempts/001/upstream.bin"))
	require.NoError(t, err)
	require.Equal(t, string(normalUpstreamSSE), string(gotNormalUpstream))
	require.Contains(t, string(gotNormalUpstream), `"thoughtSignature":"sig-upstream-1"`)

	gotNormalConverted, err := os.ReadFile(filepath.Join(normalDir, "converted.bin"))
	require.NoError(t, err)
	require.Equal(t, w1.Body.String(), string(gotNormalConverted))
	require.Contains(t, string(gotNormalConverted), "Done checking token.")

	// Verify Artifact 2 (Web Search switched to gemini-3.1-flash-lite with empty candidate return):
	require.Equal(t, "claude-sonnet-4-6", searchManifest.RequestModel)
	require.Equal(t, "gemini-4-argon-high", searchManifest.MappedModel)
	require.Equal(t, "gemini-3.1-flash-lite", searchManifest.FinalModel)
	require.Equal(t, "web_search", searchManifest.WireRequestType)
	require.Equal(t, int64(501), searchManifest.APIKeyID)
	require.False(t, searchManifest.Incomplete)
	require.Equal(t, []string{"STOP"}, searchManifest.UpstreamFinishReasons)

	gotSearchGeminiReq, err := os.ReadFile(filepath.Join(searchDir, "gemini_request.bin"))
	require.NoError(t, err)
	require.Contains(t, string(gotSearchGeminiReq), `"model":"gemini-3.1-flash-lite"`)
	require.Contains(t, string(gotSearchGeminiReq), `"requestType":"web_search"`)
	require.Contains(t, string(gotSearchGeminiReq), `"googleSearch":`)

	gotSearchAttemptReq, err := os.ReadFile(filepath.Join(searchDir, "attempts/001/request.bin"))
	require.NoError(t, err)
	require.Equal(t, string(gotSearchGeminiReq), string(gotSearchAttemptReq))

	gotSearchUpstream, err := os.ReadFile(filepath.Join(searchDir, "attempts/001/upstream.bin"))
	require.NoError(t, err)
	require.Equal(t, string(emptySearchUpstreamSSE), string(gotSearchUpstream))
	require.True(t, strings.Contains(string(gotSearchUpstream), `"webSearchQueries":["sub2api release notes"]`))
}

func TestGeminiCapture_CaptureOffDoesNotAffectSearchOrNormalForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{Gateway: config.GatewayConfig{
		GeminiCaptureLeaseFile: "",
		MaxLineSize:            defaultMaxLineSize,
	}}
	controller := NewGeminiCapture(cfg)

	settingRepo := &antigravitySettingRepoStub{values: map[string]string{
		SettingKeyAntigravityWebSearchModel: "gemini-3.1-flash-lite",
	}}
	settingSvc := NewSettingService(settingRepo, cfg)
	settingSvc.LoadAntigravityWebSearchModelSettings(context.Background())

	upstreamSSE := []byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Search result OK\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3,\"totalTokenCount\":8}}}\n\n")
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(upstreamSSE)),
	}}}
	svc := &AntigravityGatewayService{
		settingService: settingSvc,
		tokenProvider:  &AntigravityTokenProvider{},
		httpUpstream:   upstream,
	}
	account := &Account{
		ID:          89,
		Name:        "search-oauth",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "oauth-token",
			"project_id":   "proj-search",
			"model_mapping": map[string]any{
				"gemini-4-argon-high": "gemini-4-argon-high",
			},
		},
	}

	inbound := []byte(`{"model":"gemini-4-argon-high","max_tokens":512,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Search Go 1.27"}]}`)
	require.Nil(t, controller.Begin(newCaptureTestContext(), "/v1/messages", inbound, "", "gemini-4-argon-high", false, 1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(inbound))
	res, err := svc.Forward(c.Request.Context(), c, account, inbound, false)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "Search result OK")
	require.Len(t, upstream.requestBodies, 1)
	require.Contains(t, string(upstream.requestBodies[0]), `"model":"gemini-3.1-flash-lite"`)
	require.Contains(t, string(upstream.requestBodies[0]), `"requestType":"web_search"`)
}

