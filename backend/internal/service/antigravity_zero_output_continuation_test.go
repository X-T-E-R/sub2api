package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func forwardZeroFixture(t *testing.T, svc *AntigravityGatewayService, protocol string, stream bool) (*ForwardResult, error, string) {
	t.Helper()
	body := []byte(`{"model":"client-alias","stream":` + fmt.Sprint(stream) + `,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"question"}],"input":"question","contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
	account := antigravityThoughtAccount(map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: false, AntigravityContinueThoughtOnlyExtraKey: false})
	var result *ForwardResult
	var err error
	switch protocol {
	case "messages":
		result, err = svc.Forward(context.Background(), c, account, body, false)
	case "chat":
		result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
	case "responses":
		result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
	case "gemini":
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		result, err = svc.ForwardGemini(context.Background(), c, account, "gemini-3.1-pro-high", action, stream, body, false)
	}
	return result, err, recorder.Body.String()
}

func TestAntigravityZeroOutputContinueProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, on := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/on=%t", protocol, stream, on), func(t *testing.T) {
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityEmptyStopLine), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
					svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: on}, upstream)
					result, err, wire := forwardZeroFixture(t, svc, protocol, stream)
					if !on && stream && (protocol == "chat" || protocol == "responses") {
						// Legacy A/B-off compat streams keep their existing empty-stream
						// recovery signal; the new disabled switch must not alter it.
						var failover *UpstreamFailoverError
						require.ErrorAs(t, err, &failover)
						require.True(t, failover.RetryableOnSameAccount)
						require.Nil(t, result)
						require.Equal(t, 1, upstream.callCount)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					if !on {
						require.Equal(t, 1, upstream.callCount)
						require.Equal(t, 8, result.Usage.InputTokens)
						require.Zero(t, result.Usage.OutputTokens)
						return
					}
					require.Equal(t, 2, upstream.callCount)
					require.Equal(t, 28, result.Usage.InputTokens)
					require.Equal(t, 4, result.Usage.OutputTokens)
					require.Contains(t, wire, "answer")
					require.NotContains(t, wire, `"error"`)
					// Removing precisely the appended user turn reconstructs the original
					// prepared envelope byte-for-byte in meaning, including model/config/tools.
					first, second := upstream.requestBodies[0], upstream.requestBodies[1]
					contents := gjson.GetBytes(second, "request.contents").Array()
					require.Len(t, contents, len(gjson.GetBytes(first, "request.contents").Array())+1)
					require.JSONEq(t, `{"role":"user","parts":[{"text":"Continue."}]}`, contents[len(contents)-1].Raw)
					restored, deleteErr := sjson.DeleteBytes(second, fmt.Sprintf("request.contents.%d", len(contents)-1))
					require.NoError(t, deleteErr)
					require.JSONEq(t, string(first), string(restored))
				})
			}
		}
	}
}

func TestAntigravityZeroOutputContinueEmptyPlaceholders(t *testing.T) {
	for _, part := range []string{`{}`, `{"text":""}`, `{"thought":true,"text":""}`, `{"thought":true,"text":"","thoughtSignature":"opaque-signature"}`, `{"thoughtSignature":"signature-only"}`, `{"functionCall":null}`} {
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
			antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[` + part + `]},"finishReason":"STOP"}]}}`),
			antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
		}}
		svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
		_, err, wire := forwardZeroFixture(t, svc, "messages", true)
		require.NoError(t, err)
		require.Equal(t, 2, upstream.callCount)
		require.Contains(t, wire, "answer")
	}
}

func TestAntigravityZeroOutputContinueMissingTerminalAndPositiveUsage(t *testing.T) {
	line := `data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"","thoughtSignature":"signature-only"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
				result, err, wire := forwardZeroFixture(t, svc, protocol, stream)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, upstream.callCount)
				require.Contains(t, wire, "answer")
				require.Equal(t, 28, result.Usage.InputTokens)
				require.Equal(t, 9, result.Usage.OutputTokens)
				require.NotContains(t, string(upstream.requestBodies[1]), "signature-only", "no fabricated/replayed assistant tail")
				require.Equal(t, "user", gjson.GetBytes(upstream.requestBodies[1], "request.contents.1.role").String())
			})
		}
	}
	// UNSPECIFIED has the same clean-EOF eligibility, and trailing positive usage
	// does not turn empty parts into actual output.
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"FINISH_REASON_UNSPECIFIED","groundingMetadata":{"webSearchQueries":[],"groundingChunks":[]}}]}}`, `data: {"response":{"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
	result, err, _ := forwardZeroFixture(t, svc, "messages", true)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount)
	require.Equal(t, 28, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
}

func TestAntigravityZeroOutputContinueExcludesAnyOutputAndNonSTOP(t *testing.T) {
	cases := map[string]string{
		"thinking":            `{"text":"plan","thought":true}`,
		"whitespace thinking": `{"text":" ","thought":true}`,
		"text":                `{"text":"answer"}`,
		"whitespace text":     `{"text":" "}`,
		"tool":                `{"functionCall":{"name":"read","args":{}}}`,
		"image":               `{"inlineData":{"data":"AA==","mimeType":"image/png"}}`,
		"file":                `{"fileData":{"fileUri":"gs://fixture"}}`,
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			line := `data: {"response":{"candidates":[{"content":{"parts":[` + part + `]},"finishReason":"STOP"}]}}`
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
			_, _, _ = forwardZeroFixture(t, svc, "messages", true)
			require.Equal(t, 1, upstream.callCount)
		})
	}
	for _, reason := range []string{"MAX_TOKENS", "SAFETY", "MALFORMED_FUNCTION_CALL", "OTHER"} {
		t.Run("terminal/"+reason, func(t *testing.T) {
			line := `data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"` + reason + `"}]}}`
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
			_, _, _ = forwardZeroFixture(t, svc, "messages", true)
			require.Equal(t, 1, upstream.callCount)
		})
	}
	for name, line := range map[string]string{
		"safety rating":  `data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP","safetyRatings":[{"blocked":true}]}]}}`,
		"prompt refusal": `data: {"response":{"promptFeedback":{"blockReason":"SAFETY"}}}`,
		"grounding":      `data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["query"]}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
			_, _, _ = forwardZeroFixture(t, svc, "messages", true)
			require.Equal(t, 1, upstream.callCount)
		})
	}
}

func TestAntigravityZeroOutputContinueSecondFailureStops(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, second := range []string{"empty", "missing", "no objects", "status", "read", "cancel"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, second), func(t *testing.T) {
					next := antigravityThoughtStreamResponse(antigravityEmptyStopLine)
					switch second {
					case "missing":
						next = antigravityThoughtStreamResponse(`data: {"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":8}}}`)
					case "no objects":
						next = antigravityThoughtStreamResponse()
					case "status":
						next = &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"status":"INVALID_ARGUMENT","message":"fixture rejection"}}`))}
					case "read":
						next.Body = &antigravityCompatErrorReader{data: []byte(antigravityEmptyStopLine), err: errors.New("connection reset by peer")}
					case "cancel":
						next.Body = &antigravityCompatErrorReader{data: []byte(antigravityEmptyStopLine), err: context.Canceled}
					}
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityEmptyStopLine), next}}
					svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true, ContinueThoughtOnly: true}, upstream)
					result, err, wire := forwardZeroFixture(t, svc, protocol, stream)
					require.Error(t, err)
					require.NotNil(t, result)
					require.Equal(t, 2, upstream.callCount)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
					wantInput := 16
					if second == "status" || second == "no objects" {
						wantInput = 8
					}
					require.Equal(t, wantInput, result.Usage.InputTokens)
					require.Zero(t, result.Usage.OutputTokens)
					for _, success := range []string{`"stop_reason":"end_turn"`, `"type":"message_stop"`, `"finish_reason":"stop"`, "data: [DONE]", "response.completed", `"finishReason":"STOP"`} {
						require.NotContains(t, wire, success)
					}
				})
			}
		}
	}
}

func TestAntigravityZeroOutputContinueReadFailureNeverAppends(t *testing.T) {
	for _, readErr := range []error{context.Canceled, errors.New("connection reset by peer")} {
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 200, Header: http.Header{}, Body: &antigravityCompatErrorReader{data: []byte(antigravityEmptyStopLine), err: readErr}}}}
		svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
		_, _, _ = forwardZeroFixture(t, svc, "messages", true)
		require.Equal(t, 1, upstream.callCount)
	}
}
