package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravitySoftSafetyMissingTerminalAcrossProtocols(t *testing.T) {
	const safetyMessage = "Upstream stream ended after Gemini soft safety filter block"
	const safetyCode = "antigravity_soft_safety_stream_cut"
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
	for _, tc := range []struct {
		protocol, frameEvent, errorPath string
	}{
		{"messages", "event: error\n", "error"},
		{"chat", "", "error"},
		{"responses", "event: response.failed\n", "response.error"},
		{"gemini", "", "error"},
	} {
		for _, blocked := range []bool{false, true} {
			name := tc.protocol + "/ordinary_eof"
			if blocked {
				name = tc.protocol + "/soft_safety_eof"
			}
			t.Run(name, func(t *testing.T) {
				textLine := func(text string) string {
					parts, err := json.Marshal([]map[string]string{{"text": text}})
					require.NoError(t, err)
					return missingTerminalFixture(string(parts), "")
				}
				lines := []string{textLine("This request was blocked by Gemini's filters.")}
				if blocked {
					// Observe both markers and a marker split across upstream SSE frames.
					lines = append(lines, textLine(" See https://policies.google.com/terms/generative-"), textLine("ai/use-policy"))
				}
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(lines...)}}
				svc := newMissingTerminalReplayService(t, upstream)
				body := preheaderAGBody(tc.protocol, true)
				c, rec := newAntigravityCompatContext("POST", "/fixture", body)
				ConfigureStreamPreheader(c, cfg, tc.protocol, true)
				_, err := preheaderAGForward(svc, c, tc.protocol, true, body)
				require.ErrorIs(t, err, errStreamPreheaderTerminal)
				require.Equal(t, 1, upstream.callCount)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
				require.True(t, StreamPreheaderFinalized(c))
				require.Equal(t, blocked, antigravitySoftSafetyBlockedInContext(c))
				frames := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
				prefix := tc.frameEvent + "data: "
				lastFrame := frames[len(frames)-1]
				require.True(t, strings.HasPrefix(lastFrame, prefix), lastFrame)
				terminal := gjson.Get(strings.TrimPrefix(lastFrame, prefix), tc.errorPath)
				require.True(t, terminal.IsObject(), lastFrame)
				if tc.protocol == "gemini" {
					require.EqualValues(t, http.StatusBadGateway, terminal.Get("code").Int())
					require.Equal(t, "UNAVAILABLE", terminal.Get("status").String())
					require.Equal(t, "GATEWAY_STREAM_TERMINAL_ERROR", terminal.Get("details.0.reason").String())
				} else {
					require.Equal(t, "gateway_stream_terminal_error", terminal.Get("code").String())
					require.EqualValues(t, http.StatusBadGateway, terminal.Get("status").Int())
					require.Equal(t, "upstream_error", terminal.Get("type").String())
				}
				if blocked {
					require.Equal(t, safetyMessage, terminal.Get("message").String())
					require.Contains(t, terminal.Raw, `"upstream_code":"`+safetyCode+`"`)
					require.Equal(t, safetyCode, terminal.Get("upstream_code").String())
					require.Contains(t, terminal.Raw, `"retryable":false`)
					require.Equal(t, gjson.False, terminal.Get("retryable").Type)
				} else {
					require.Equal(t, "Upstream stream ended before a terminal event", terminal.Get("message").String())
					require.False(t, terminal.Get("upstream_code").Exists())
					require.False(t, terminal.Get("retryable").Exists())
				}
				for _, success := range []string{"event: message_stop", "[DONE]", "response.completed", `"finishReason":"STOP"`} {
					require.NotContains(t, rec.Body.String(), success)
				}
			})
		}
	}
}
