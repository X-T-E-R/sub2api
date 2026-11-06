package service

import (
	"bufio"
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

func TestSPH02MessagesChatFallbackRequiresChatTerminal(t *testing.T) {
	cases := []struct {
		name          string
		chunk         string
		wantErr       bool
		wantStop      bool
		wantFinalized bool
	}{
		{
			name: "clean EOF without finish reason is a terminal failure",
			chunk: `data: {"id":"chatcmpl-review","object":"chat.completion.chunk","created":1,"model":"review","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}

`,
			wantErr:       true,
			wantStop:      false,
			wantFinalized: true,
		},
		{
			name: "finish reason permits normal conversion finalization",
			chunk: `data: {"id":"chatcmpl-review","object":"chat.completion.chunk","created":1,"model":"review","choices":[{"index":0,"delta":{"role":"assistant","content":"complete"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}

`,
			wantErr:       false,
			wantStop:      true,
			wantFinalized: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			cfg := &config.Config{Gateway: config.GatewayConfig{
				StreamPreheaderKeepaliveEnabled: true,
				StreamKeepaliveInterval:         1,
			}}
			ConfigureStreamPreheader(c, cfg, "messages", true)

			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tc.chunk)),
			}
			svc := &OpenAIGatewayService{cfg: cfg}
			result, err := svc.streamChatCompletionsAsAnthropic(
				c, resp, "review-model", "review-model", "review-model", nil, nil, time.Now(),
			)

			if tc.wantErr {
				require.ErrorIs(t, err, errStreamPreheaderTerminal)
			} else {
				require.NoError(t, err)
			}
			require.NotNil(t, result)
			require.Equal(t, tc.wantFinalized, StreamPreheaderFinalized(c))
			wire := w.Body.String()
			if tc.wantStop {
				require.Contains(t, wire, "event: message_stop")
				require.NotContains(t, wire, streamPreheaderTerminalCode)
			} else {
				require.NotContains(t, wire, "event: message_stop")
				require.Contains(t, wire, streamPreheaderTerminalCode)
			}
		})
	}
}

func TestSPH02NativeAnthropicChatRequiresMessageStop(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	cfg := &config.Config{Gateway: config.GatewayConfig{
		StreamPreheaderKeepaliveEnabled: true,
		StreamKeepaliveInterval:         1,
	}}
	ConfigureStreamPreheader(c, cfg, "chat", true)

	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_review","type":"message","role":"assistant","content":[],"model":"review","stop_reason":null,"usage":{"input_tokens":8,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"input_tokens":8,"output_tokens":3}}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &OpenAIGatewayService{cfg: cfg}
	result, err := svc.handleCCStreamingFromNativeAnthropic(
		resp, c, "review-model", "review-model", "review-model", nil, time.Now(), false,
	)

	require.ErrorIs(t, err, errStreamPreheaderTerminal)
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.True(t, StreamPreheaderFinalized(c))
	require.Contains(t, w.Body.String(), streamPreheaderTerminalCode)
	require.NotContains(t, w.Body.String(), "event: message_stop")
}

func TestSPH02NativeAnthropicReaderKeepsIdleDeadline(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	cfg := &config.Config{Gateway: config.GatewayConfig{
		StreamPreheaderKeepaliveEnabled: true,
		StreamKeepaliveInterval:         1,
	}}
	ConfigureStreamPreheader(c, cfg, "chat", true)

	pipeReader, pipeWriter := io.Pipe()
	source := bufio.NewScanner(pipeReader)
	nextLine, stopReader := newOpenAINativeAnthropicLineReader(c, source, 5*time.Millisecond)
	_, err := nextLine()
	require.True(t, errors.Is(err, errAnthropicNativeStreamIdle), "expected idle deadline, got %v", err)
	_ = pipeReader.Close()
	_ = pipeWriter.Close()
	stopReader()
}
