package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func missingTerminalFixture(parts, reason string) string {
	finish := ""
	if reason != "" {
		finish = `,"finishReason":"` + reason + `"`
	}
	return `data: {"response":{"candidates":[{"content":{"parts":` + parts + `}` + finish + `}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
}

func newMissingTerminalReplayService(t *testing.T, upstream HTTPUpstream) *AntigravityGatewayService {
	t.Helper()
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	svc.settingService.cfg.Gateway.AntigravityRetryMissingTerminal = true
	return svc
}

func TestAntigravityMissingTerminalReplayProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for name, parts := range map[string]string{
				"empty":              `[]`,
				"thought_signature":  `[{"thought":true,"thoughtSignature":"sig-dead"}]`,
				"trailing_signature": `[{"text":"","thoughtSignature":"sig-dead"}]`,
			} {
				for _, reason := range []string{"", "FINISH_REASON_UNSPECIFIED"} {
					for _, recovered := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/stream=%t/%s/%s/recovered=%t", protocol, stream, name, reason, recovered), func(t *testing.T) {
							first := missingTerminalFixture(parts, reason)
							second := missingTerminalFixture(`[]`, reason)
							if recovered {
								second = antigravityThoughtGenerationTwoLine
							}
							upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(first), antigravityThoughtStreamResponse(second)}}
							var headers []http.Header
							var urls []string
							upstream.onCall = func(req *http.Request, _ *queuedHTTPUpstreamStub) {
								headers = append(headers, req.Header.Clone())
								urls = append(urls, req.URL.String())
							}
							svc := newMissingTerminalReplayService(t, upstream)
							body := preheaderAGBody(protocol, stream)
							c, rec := newAntigravityCompatContext("POST", "/fixture", body)
							if stream {
								c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: map[string]streamPreheaderProtocol{"messages": streamPreheaderMessages, "chat": streamPreheaderChat, "responses": streamPreheaderResponses, "gemini": streamPreheaderGemini}[protocol], interval: time.Second})
								_, _ = c.Writer.WriteString(": committed\n\n")
								c.Writer.Flush()
								markStreamPreheaderBodyHeartbeat(c)
							}
							result, err := preheaderAGForward(svc, c, protocol, stream, body)
							require.Equal(t, 2, upstream.callCount)
							require.Equal(t, upstream.requestBodies[0], upstream.requestBodies[1], "byte-for-byte prepared envelope; no Continue. or model tail")
							require.Equal(t, headers[0], headers[1])
							require.Equal(t, urls[0], urls[1])
							require.NotNil(t, result)
							wire := rec.Body.String()
							cont := antigravityThoughtContinuationFromGin(c)
							require.True(t, cont.missingTerminalReplayUsed())
							require.False(t, cont.zeroOutputContinuationUsed())
							require.False(t, cont.canContinue())
							if recovered {
								require.NoError(t, err)
								require.Contains(t, wire, "answer")
								require.Equal(t, 28, result.Usage.InputTokens)
								require.Equal(t, 9, result.Usage.OutputTokens)
								require.Equal(t, 2, result.Usage.CacheReadInputTokens)
								if !stream || protocol != "gemini" {
									require.NotContains(t, wire, "sig-dead", "abandoned carriers must not be grafted onto replay content")
								}
								if stream && protocol == "messages" {
									require.Equal(t, 1, strings.Count(wire, "event: message_start\n"))
									require.Equal(t, 1, strings.Count(wire, "event: message_stop\n"))
								}
							} else {
								var failure *AntigravityNoUsableAnswerError
								require.ErrorAs(t, err, &failure)
								require.Equal(t, "antigravity_missing_terminal", failure.Code)
								require.Equal(t, 16, result.Usage.InputTokens)
								require.Equal(t, 10, result.Usage.OutputTokens)
								require.Equal(t, 4, result.Usage.CacheReadInputTokens)
								require.Contains(t, wire, failure.Code)
								if !stream {
									require.Equal(t, http.StatusBadGateway, rec.Code)
								}
								for _, success := range []string{"event: message_stop", "data: [DONE]", "response.completed", `"finishReason":"STOP"`} {
									require.NotContains(t, wire, success)
								}
							}
						})
					}
				}
			}
		}
	}
}

func TestAntigravityMissingTerminalReplayFailuresAndUsageOnly(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, second := range []string{"status", "transport", "read", "no_objects", "usage_only"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, second), func(t *testing.T) {
					// Usage without a candidate is a recognized zero-output response, too.
					first := `data: {"response":{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":2}}}`
					next := antigravityThoughtStreamResponse()
					var secondErr error
					wantInput, wantCache := 8, 2
					wantCode := "thought_continuation_error"
					switch second {
					case "status":
						next = &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"status":"INVALID_ARGUMENT","message":"fixture rejection"}}`))}
					case "transport":
						next, secondErr = nil, errors.New("connection refused")
					case "read":
						next.Body = &antigravityCompatErrorReader{data: []byte(first + "\n\n"), err: errors.New("connection reset by peer")}
						wantInput, wantCache = 16, 4
					case "usage_only":
						next = antigravityThoughtStreamResponse(first)
						wantInput, wantCache, wantCode = 16, 4, "antigravity_missing_terminal"
					case "no_objects":
						wantCode = "antigravity_missing_terminal"
					}
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(first), next}, errors: []error{nil, secondErr}}
					svc := newMissingTerminalReplayService(t, upstream)
					body := preheaderAGBody(protocol, stream)
					c, rec := newAntigravityCompatContext("POST", "/fixture", body)
					result, err := preheaderAGForward(svc, c, protocol, stream, body)
					require.True(t, IsAntigravityPartialResponseError(err), "err=%v", err)
					require.NotNil(t, result)
					require.Equal(t, 2, upstream.callCount)
					require.Equal(t, wantInput, result.Usage.InputTokens)
					require.Equal(t, wantCache, result.Usage.CacheReadInputTokens)
					require.Contains(t, rec.Body.String(), wantCode)
					if !stream {
						require.Equal(t, http.StatusBadGateway, rec.Code)
					}
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
				})
			}
		}
	}
}

func TestAntigravityMissingTerminalReplayEligibilityAndGates(t *testing.T) {
	for _, reason := range []string{"STOP", "MAX_TOKENS", "SAFETY", "RECITATION", "MALFORMED_FUNCTION_CALL", "FUTURE_FAILURE"} {
		g := newAntigravityThoughtGeneration()
		g.observeLine(missingTerminalFixture(`[]`, reason))
		require.False(t, g.missingTerminalZeroOutput(), reason)
	}
	for _, part := range []string{`{"text":"answer"}`, `{"thought":true,"text":"plan"}`, `{"thought":true,"text":" "}`, `{"functionCall":{"name":"lookup","args":{}}}`, `{"inlineData":{"data":"AA=="}}`, `{"fileData":{"fileUri":"gs://fixture"}}`} {
		g := newAntigravityThoughtGeneration()
		g.observeLine(missingTerminalFixture("["+part+"]", ""))
		require.False(t, g.missingTerminalZeroOutput(), part)
	}
	for _, payload := range []string{`{}`, `{"error":{"message":"failure"}}`, `{"candidates":[{"groundingMetadata":{"webSearchQueries":["q"]}}]}`, `{"promptFeedback":{"blockReason":"SAFETY"}}`} {
		g := newAntigravityThoughtGeneration()
		g.observePayload([]byte(payload))
		require.False(t, g.missingTerminalZeroOutput(), payload)
	}
	for _, enabled := range []bool{false, true} {
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "")), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
		svc := newMissingTerminalReplayService(t, upstream)
		svc.settingService.cfg.Gateway.AntigravityRetryMissingTerminal = enabled
		body, c, _ := newAntigravityThoughtClaudeRequest(true)
		_, _ = svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
		if enabled {
			require.Equal(t, 2, upstream.callCount)
		} else {
			require.Equal(t, 1, upstream.callCount)
		}
	}
	upstream := &queuedHTTPUpstreamStub{}
	svc := newMissingTerminalReplayService(t, upstream)
	cont := svc.newAntigravityThoughtContinuation(context.Background(), antigravityThoughtAccount(map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: false}), "fixture", "", "", "", "streamGenerateContent", []byte(`{"request":{"contents":[]}}`))
	cont.generation.observeLine(missingTerminalFixture(`[]`, ""))
	require.False(t, cont.canContinue(), "explicit account A=false disables automatic recovery")
	cont.retryMissingTerminal = true
	cont.generation.readFailed = true
	require.False(t, cont.canContinue(), "read errors are never clean EOF")
}
