package service

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAntigravityMissingTerminalReplayInvalidSecondFramesFail(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, line := range []string{"data: [null]", "data: null", "data: malformed"} {
				t.Run(fmt.Sprintf("%s/%t/%s", protocol, stream, line), func(t *testing.T) {
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
						antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "")),
						antigravityThoughtStreamResponse(line),
					}}
					svc := newMissingTerminalReplayService(t, upstream)
					body := preheaderAGBody(protocol, stream)
					c, rec := newAntigravityCompatContext("POST", "/fixture", body)
					result, err := preheaderAGForward(svc, c, protocol, stream, body)
					var failure *AntigravityNoUsableAnswerError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, "antigravity_missing_terminal", failure.Code)
					require.NotNil(t, result)
					require.Equal(t, 8, result.Usage.InputTokens)
					require.Equal(t, 5, result.Usage.OutputTokens)
					require.Equal(t, 2, upstream.callCount)
					require.Contains(t, rec.Body.String(), failure.Code)
					require.NotContains(t, rec.Body.String(), "message_stop")
					require.NotContains(t, rec.Body.String(), "response.completed")
					require.NotContains(t, rec.Body.String(), "data: [DONE]")
				})
			}
		}
	}
}

func TestAntigravityMissingTerminalReplayIdleTimeoutKeepsUsage(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", protocol, stream), func(t *testing.T) {
				reader, writer := io.Pipe()
				defer writer.Close()
				next := antigravityThoughtStreamResponse()
				next.Body = reader
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
					antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "")), next,
				}}
				svc := newMissingTerminalReplayService(t, upstream)
				svc.settingService.cfg.Gateway.StreamDataIntervalTimeout = 1
				body := preheaderAGBody(protocol, stream)
				c, rec := newAntigravityCompatContext("POST", "/fixture", body)
				result, err := preheaderAGForward(svc, c, protocol, stream, body)
				require.True(t, IsAntigravityThoughtContinuationError(err), "err=%v", err)
				require.NotNil(t, result)
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				require.Equal(t, 2, result.Usage.CacheReadInputTokens)
				require.Equal(t, 2, upstream.callCount)
				require.Contains(t, rec.Body.String(), "thought_continuation_error")
				if !stream {
					require.Equal(t, http.StatusBadGateway, rec.Code)
				}
			})
		}
	}
}

func TestAntigravityMissingTerminalReplayNeverReplaysEnvelopeErrors(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", protocol, stream), func(t *testing.T) {
				line := `data: {"error":{"message":"upstream error"},"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":8}}}`
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
				svc := newMissingTerminalReplayService(t, upstream)
				body := preheaderAGBody(protocol, stream)
				c, _ := newAntigravityCompatContext("POST", "/fixture", body)
				_, _ = preheaderAGForward(svc, c, protocol, stream, body)
				require.Equal(t, 1, upstream.callCount)
				require.True(t, antigravityThoughtContinuationFromGin(c).generation.hasUpstreamError)
			})
		}
	}
}

func TestAntigravityMissingTerminalReplayNeverForgetsDisqualifiers(t *testing.T) {
	g := newAntigravityThoughtGeneration()
	g.observeLine(missingTerminalFixture(`[]`, "STOP"))
	g.observeLine(missingTerminalFixture(`[]`, "FINISH_REASON_UNSPECIFIED"))
	require.Equal(t, "STOP", g.finishReason)
	require.False(t, g.missingTerminalZeroOutput())

	g = newAntigravityThoughtGeneration()
	g.observePayload([]byte(`{"error":{"message":"upstream error"},"response":{"candidates":[{"content":{"parts":[]}}]}}`))
	require.True(t, g.hasUpstreamError)
	require.False(t, g.missingTerminalZeroOutput())

	g = newAntigravityThoughtGeneration()
	g.observePayload([]byte(`{"candidates":[{"content":{"parts":[]},"groundingMetadata":{"groundingChunks":[{"retrievedContext":{"uri":"fixture://source"}}]}}]}`))
	require.True(t, g.hasGroundingCarrier)
	require.False(t, g.missingTerminalZeroOutput(), "non-rendered grounding is still substantive provider output")
}
