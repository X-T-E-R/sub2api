package service

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func antigravityCachedZeroOutputResponse() *http.Response {
	// Both frames report the same per-generation snapshot, not two increments.
	return antigravityThoughtStreamResponse(
		`data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":380743,"cachedContentTokenCount":378694,"candidatesTokenCount":0}}}`,
		`data: {"response":{"usageMetadata":{"promptTokenCount":380743,"cachedContentTokenCount":378694,"candidatesTokenCount":0}}}`,
	)
}

func TestAntigravityContinuedUsageAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name                  string
		second                func() *http.Response
		wantInput, wantOutput int
		wantCache             int
		wantAnswer            bool
	}{
		{
			name: "second_response_without_usage",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}}`)
			},
			wantInput: 2049, wantCache: 378694,
		},
		{
			name: "second_empty_response_object_without_usage",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(`data: {"response":{}}`)
			},
			wantInput: 2049, wantCache: 378694,
		},
		{
			name: "second_missing_terminal_without_usage",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[]}}]}}`)
			},
			wantInput: 2049, wantCache: 378694,
		},
		{
			name: "second_answer_without_usage",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`)
			},
			wantInput: 2049, wantCache: 378694, wantAnswer: true,
		},
		{
			name: "second_response_empty_usage",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{}}}`)
			},
			wantInput: 2049, wantCache: 378694,
		},
		{
			name:      "second_missing_response_falls_back_once",
			second:    func() *http.Response { return antigravityThoughtStreamResponse() },
			wantInput: 2049, wantCache: 378694,
		},
		{
			name: "second_usage_snapshot_is_added_once",
			second: func() *http.Response {
				return antigravityThoughtStreamResponse(
					`data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":380744,"cachedContentTokenCount":378695,"candidatesTokenCount":4}}}`,
					`data: {"response":{"usageMetadata":{"promptTokenCount":380744,"cachedContentTokenCount":378695,"candidatesTokenCount":4}}}`,
				)
			},
			wantInput: 4098, wantOutput: 4, wantCache: 757389, wantAnswer: true,
		},
	}
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, tc.name), func(t *testing.T) {
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCachedZeroOutputResponse(), tc.second()}}
					svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
					result, wire, err := forwardZeroFixture(t, svc, protocol, stream)
					if tc.wantAnswer {
						require.NoError(t, err)
						require.Contains(t, wire, "answer")
					} else {
						require.True(t, IsAntigravityNoUsableAnswerError(err), "err=%v", err)
					}
					require.NotNil(t, result)
					require.Equal(t, 2, upstream.callCount, "one bounded extra call")
					require.Equal(t, tc.wantInput, result.Usage.InputTokens)
					require.Equal(t, tc.wantOutput, result.Usage.OutputTokens)
					require.Equal(t, tc.wantCache, result.Usage.CacheReadInputTokens)
					assertAntigravityReportedUsage(t, protocol, stream, wire, tc.wantInput, tc.wantOutput, tc.wantCache)
				})
			}
		}
	}
}

func assertAntigravityReportedUsage(t *testing.T, protocol string, stream bool, wire string, input, output, cache int) {
	t.Helper()
	// All adapters expose one final cumulative usage snapshot. For streaming,
	// select its terminal usage rather than message_start's initial estimate.
	var usage gjson.Result
	if !stream {
		usage = gjson.Get(wire, "usage")
		if protocol == "gemini" {
			usage = gjson.Get(wire, "usageMetadata")
		}
	} else {
		for _, line := range strings.Split(wire, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			candidate := gjson.Get(payload, "usage")
			switch protocol {
			case "gemini":
				candidate = gjson.Get(payload, "usageMetadata")
			case "responses":
				candidate = gjson.Get(payload, "response.usage")
			}
			if candidate.IsObject() {
				usage = candidate
			}
		}
	}
	require.True(t, usage.IsObject(), "final usage missing: %s", wire)
	switch protocol {
	case "messages":
		require.EqualValues(t, input, usage.Get("input_tokens").Int())
		require.EqualValues(t, output, usage.Get("output_tokens").Int())
		require.EqualValues(t, cache, usage.Get("cache_read_input_tokens").Int())
	case "chat":
		require.EqualValues(t, input+cache, usage.Get("prompt_tokens").Int())
		require.EqualValues(t, output, usage.Get("completion_tokens").Int())
		require.EqualValues(t, cache, usage.Get("prompt_tokens_details.cached_tokens").Int())
	case "responses":
		require.EqualValues(t, input+cache, usage.Get("input_tokens").Int())
		require.EqualValues(t, output, usage.Get("output_tokens").Int())
		require.EqualValues(t, cache, usage.Get("input_tokens_details.cached_tokens").Int())
	case "gemini":
		require.EqualValues(t, input+cache, usage.Get("promptTokenCount").Int())
		require.EqualValues(t, output, usage.Get("candidatesTokenCount").Int()+usage.Get("thoughtsTokenCount").Int())
		require.EqualValues(t, cache, usage.Get("cachedContentTokenCount").Int())
	}
}
