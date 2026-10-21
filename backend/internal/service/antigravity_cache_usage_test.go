package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityMessagesFinalCacheUsageMatchesBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := "data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":1}}}` + "\n\n" +
		"data: " + `{"response":{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":7,"thoughtsTokenCount":3}}}` + "\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
	result, err := svc.handleClaudeStreamingResponse(c, resp, time.Now(), "configured-model")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 20, result.usage.InputTokens)
	require.Equal(t, 80, result.usage.CacheReadInputTokens)
	require.Equal(t, 10, result.usage.OutputTokens)
	var reported map[string]any
	stopped := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		require.False(t, stopped, "no Anthropic event may follow message_stop")
		if event["type"] == "message_stop" {
			stopped = true
		}
		if event["type"] == "message_delta" {
			reported, _ = event["usage"].(map[string]any)
		}
	}
	require.True(t, stopped)
	require.NotNil(t, reported)
	require.Equal(t, float64(result.usage.InputTokens), reported["input_tokens"])
	require.Equal(t, float64(result.usage.CacheReadInputTokens), reported["cache_read_input_tokens"])
	require.Equal(t, float64(result.usage.OutputTokens), reported["output_tokens"])
}

func TestAntigravityMessagesReadFailureAfterFinishDoesNotSignalSuccess(t *testing.T) {
	for _, readErr := range []error{context.Canceled, errors.New("connection reset by peer")} {
		t.Run(readErr.Error(), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc := newAntigravityTestService(&config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			data := "data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":7}}}` + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &antigravityCompatErrorReader{data: []byte(data), err: readErr}}
			result, err := svc.handleClaudeStreamingResponse(c, resp, time.Now(), "configured-model")
			require.NotContains(t, rec.Body.String(), "event: message_stop")
			require.NotContains(t, rec.Body.String(), `"stop_reason":"end_turn"`)
			if errors.Is(readErr, context.Canceled) {
				require.NoError(t, err)
				require.True(t, result.clientDisconnect)
				require.Equal(t, 80, result.usage.CacheReadInputTokens)
				require.NotContains(t, rec.Body.String(), "event: error")
			} else {
				require.ErrorContains(t, err, "connection reset by peer")
				require.Contains(t, rec.Body.String(), "stream_read_error")
			}
		})
	}
}

// R1: a fully cached final prompt replaces the initial uncached estimate with
// zero. Exercise both real adapters, not just the processor's billing result.
func TestAntigravityCompatFinalCacheUsageZeroInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
			c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/"+protocol, nil)
			body := "data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":1}}}` + "\n\n" +
				"data: " + `{"response":{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":100,"candidatesTokenCount":7,"thoughtsTokenCount":3}}}` + "\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			var result *antigravityStreamResult
			var err error
			if protocol == "responses" {
				result, err = svc.handleResponsesStreamingFromAntigravity(c, resp, time.Now(), "configured-model")
			} else {
				result, err = svc.handleChatCompletionsStreamingFromAntigravity(c, resp, time.Now(), "configured-model", true)
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Zero(t, result.usage.InputTokens)
			require.Equal(t, 100, result.usage.CacheReadInputTokens)
			require.Equal(t, 10, result.usage.OutputTokens)
			var terminalUsage map[string]any
			for _, line := range strings.Split(rec.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") || strings.TrimPrefix(line, "data: ") == "[DONE]" {
					continue
				}
				var event map[string]any
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
				if protocol == "responses" && event["type"] == "response.completed" {
					response := event["response"].(map[string]any)
					terminalUsage = response["usage"].(map[string]any)
				} else if protocol == "chat" {
					if usage, ok := event["usage"].(map[string]any); ok {
						terminalUsage = usage
					}
				}
			}
			require.NotNil(t, terminalUsage)
			inputKey, outputKey, detailsKey := "input_tokens", "output_tokens", "input_tokens_details"
			if protocol == "chat" {
				inputKey, outputKey, detailsKey = "prompt_tokens", "completion_tokens", "prompt_tokens_details"
			}
			require.Equal(t, float64(100), terminalUsage[inputKey], "total input must equal final uncached + cached buckets")
			require.Equal(t, float64(10), terminalUsage[outputKey])
			require.Equal(t, float64(110), terminalUsage["total_tokens"])
			details, ok := terminalUsage[detailsKey].(map[string]any)
			require.True(t, ok)
			require.Equal(t, float64(100), details["cached_tokens"])
		})
	}
}
