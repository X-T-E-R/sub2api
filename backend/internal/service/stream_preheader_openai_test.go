package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStreamPreheaderOpenAIPassthroughSuccess(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, stream bool
	}{
		{"off-stream", false, true},
		{"on-nonstream", true, false},
		{"on-slow-headers", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.enabled && tc.stream {
					select {
					case <-time.After(1150 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("x-request-id", "upstream-original-id")
				const completed = `{"id":"resp_original","object":"response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":3}}`
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+completed+"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, completed)
				}
			}))
			t.Cleanup(upstream.Close)
			base, err := url.Parse(upstream.URL)
			require.NoError(t, err)
			transport := &preheaderHTTPUpstream{base: base, client: upstream.Client()}
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: tc.enabled, StreamKeepaliveInterval: 1}}
			type outcome struct {
				result *OpenAIForwardResult
				err    error
			}
			outcomes := make(chan outcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				ConfigureStreamPreheader(c, cfg, "responses", tc.stream)
				account := &Account{ID: 9302, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"api_key": "fixture-token"}, Extra: map[string]any{"openai_passthrough": true}}
				body := []byte(`{"model":"gpt-5.2","input":"fixture question","stream":` + strconv.FormatBool(tc.stream) + `}`)
				out, forwardErr := (&OpenAIGatewayService{cfg: cfg, httpUpstream: transport}).Forward(r.Context(), c, account, body)
				outcomes <- outcome{out, forwardErr}
			}))
			t.Cleanup(server.Close)
			response, err := (&http.Client{Timeout: 5 * time.Second}).Post(server.URL+"/v1/responses", "application/json", nil)
			require.NoError(t, err)
			defer response.Body.Close()
			require.Equal(t, http.StatusOK, response.StatusCode)
			if tc.enabled && tc.stream {
				readPreheaderPing(t, response, "responses")
			}
			wire, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NotContains(t, string(wire), ": keepalive")
			require.NotContains(t, string(wire), streamPreheaderTerminalCode)
			require.Contains(t, string(wire), "resp_original")
			if !tc.stream {
				require.Equal(t, "application/json", response.Header.Get("Content-Type"))
			}
			out := <-outcomes
			require.NoError(t, out.err)
			require.NotNil(t, out.result)
			require.Equal(t, "upstream-original-id", out.result.RequestID)
			require.Equal(t, "resp_original", out.result.ResponseID)
			require.Equal(t, 8, out.result.Usage.InputTokens)
			require.Equal(t, 3, out.result.Usage.OutputTokens)
			require.EqualValues(t, 1, transport.calls.Load())
		})
	}
}
