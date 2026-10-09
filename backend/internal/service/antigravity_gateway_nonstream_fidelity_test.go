package service

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHandleGeminiStreamToNonStreamingPreservesEarlyThinkingToolAndSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", nil)
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"sig-after-thought"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_signature_chain","name":"lookup","args":{"query":"Tokyo"}},"thoughtSignature":"sig-tool"}]}}]}`,
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"thoughtsTokenCount":3}}`,
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	streamResult, err := svc.handleGeminiStreamToNonStreaming(c, resp, time.Now())
	require.NoError(t, err)
	require.NotNil(t, streamResult)
	require.Equal(t, 11, streamResult.usage.InputTokens)
	require.Equal(t, 10, streamResult.usage.OutputTokens)

	var response struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Candidates, 1)
	parts := response.Candidates[0].Content.Parts
	require.Len(t, parts, 3, "early parts must survive a finish/usage-only tail")
	require.Equal(t, true, parts[0]["thought"])
	require.Equal(t, "plan", parts[0]["text"])
	require.Equal(t, "sig-after-thought", parts[1]["thoughtSignature"])
	require.Empty(t, parts[1]["text"])
	require.Equal(t, "sig-tool", parts[2]["thoughtSignature"])
	require.Equal(t, "lookup", parts[2]["functionCall"].(map[string]any)["name"])
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestMergeCollectedPartsKeepsThoughtSeparateFromOrdinaryText(t *testing.T) {
	response := map[string]any{"candidates": []any{map[string]any{}}}
	merged := mergeCollectedPartsToResponse(response, []map[string]any{
		{"text": "plan", "thought": true},
		{"text": "visible answer"},
		{"text": "", "thoughtSignature": "sig-carrier"},
		{"functionCall": map[string]any{"name": "lookup"}, "thoughtSignature": "sig-tool"},
	})

	candidates := merged["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	content := candidate["content"].(map[string]any)
	parts := content["parts"].([]any)
	require.Len(t, parts, 4)
	require.Equal(t, true, parts[0].(map[string]any)["thought"])
	require.Equal(t, "visible answer", parts[1].(map[string]any)["text"])
	require.Equal(t, "sig-carrier", parts[2].(map[string]any)["thoughtSignature"])
	require.Equal(t, "sig-tool", parts[3].(map[string]any)["thoughtSignature"])
}
