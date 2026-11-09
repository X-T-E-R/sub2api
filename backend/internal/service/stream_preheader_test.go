package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type preheaderHTTPUpstream struct {
	base   *url.URL
	client *http.Client
	calls  atomic.Int32
}

func (u *preheaderHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	copy := req.Clone(req.Context())
	copy.URL.Scheme, copy.URL.Host = u.base.Scheme, u.base.Host
	return u.client.Do(copy)
}

func (u *preheaderHTTPUpstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, account, concurrency)
}

type preheaderForwardOutcome struct {
	result    *ForwardResult
	err       error
	finalized bool
}

func preheaderAGBody(protocol string, stream bool) []byte {
	streamValue := "false"
	if stream {
		streamValue = "true"
	}
	switch protocol {
	case "gemini":
		return []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
	case "chat":
		return []byte(`{"model":"client-alias","stream":` + streamValue + `,"messages":[{"role":"user","content":"question"}]}`)
	case "responses":
		return []byte(`{"model":"client-alias","stream":` + streamValue + `,"input":"question"}`)
	default:
		return []byte(`{"model":"client-alias","stream":` + streamValue + `,"max_tokens":64,"messages":[{"role":"user","content":"question"}]}`)
	}
}

func preheaderAGForward(svc *AntigravityGatewayService, c *gin.Context, protocol string, stream bool, body []byte) (*ForwardResult, error) {
	account := antigravityMessagesMappedTestAccount("gemini-4-argon")
	switch protocol {
	case "gemini":
		return svc.ForwardGemini(c.Request.Context(), c, account, "client-alias", "streamGenerateContent", stream, body, false)
	case "chat":
		return svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, nil)
	case "responses":
		return svc.ForwardAsResponses(c.Request.Context(), c, account, body, nil)
	default:
		return svc.Forward(c.Request.Context(), c, account, body, false)
	}
}

func preheaderAGServer(t *testing.T, protocol string, stream bool, upstreamHandler http.HandlerFunc, configure func(*config.Config)) (*httptest.Server, *preheaderHTTPUpstream, <-chan preheaderForwardOutcome) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstreamServer.Close)
	base, err := url.Parse(upstreamServer.URL)
	require.NoError(t, err)
	u := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1, StreamPreheaderKeepaliveEnabled: true}}
	if configure != nil {
		configure(cfg)
	}
	svc := newAntigravityCompatService(cfg.Gateway, u)
	finished := make(chan preheaderForwardOutcome, 1)
	body := preheaderAGBody(protocol, stream)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		ConfigureStreamPreheader(c, cfg, protocol, stream)
		result, err := preheaderAGForward(svc, c, protocol, stream, body)
		if !c.Writer.Written() && err != nil && r.Context().Err() == nil {
			var rejection *UpstreamFailoverError
			if errors.As(err, &rejection) {
				for name, values := range rejection.ResponseHeaders {
					for _, value := range values {
						c.Writer.Header().Add(name, value)
					}
				}
				c.Data(rejection.StatusCode, "application/json", rejection.ResponseBody)
			}
		}
		finished <- preheaderForwardOutcome{result, err, StreamPreheaderFinalized(c)}
	}))
	t.Cleanup(server.Close)
	return server, u, finished
}

func readPreheaderPing(t *testing.T, response *http.Response, protocol string) {
	t.Helper()
	want := ": keepalive\n\n"
	if protocol == "messages" {
		want = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	}
	got := make([]byte, len(want))
	_, err := io.ReadFull(response.Body, got)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func awaitPreheaderOutcome(t *testing.T, outcomes <-chan preheaderForwardOutcome) preheaderForwardOutcome {
	t.Helper()
	select {
	case result := <-outcomes:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
		return preheaderForwardOutcome{}
	}
}

func TestStreamPreheaderSlowHTTPHeadersFourProtocols(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		for _, status := range []int{200, 401, 429} {
			t.Run(protocol+"/"+http.StatusText(status), func(t *testing.T) {
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				server, upstream, outcomes := preheaderAGServer(t, protocol, true, func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("X-Request-Id", "late-real-id")
					w.Header().Set("Retry-After", "17")
					w.WriteHeader(status)
					if status == 200 {
						resp := antigravityCompatSuccessResponse()
						defer resp.Body.Close()
						_, _ = io.Copy(w, resp.Body)
					} else {
						_, _ = io.WriteString(w, `{"error":{"message":"fixture rejection"}}`)
					}
				}, nil)
				client := &http.Client{Timeout: 5 * time.Second}
				response, err := client.Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody(protocol, true)))
				require.NoError(t, err)
				defer response.Body.Close()
				require.Equal(t, 200, response.StatusCode)
				require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
				readPreheaderPing(t, response, protocol)
				unblock()
				rest, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				outcome := awaitPreheaderOutcome(t, outcomes)
				require.EqualValues(t, 1, upstream.calls.Load())
				if status == 200 {
					require.NoError(t, outcome.err)
					require.NotNil(t, outcome.result)
					require.Equal(t, 8, outcome.result.Usage.InputTokens)
					require.Equal(t, 3, outcome.result.Usage.OutputTokens)
					require.False(t, outcome.finalized)
					require.NotContains(t, string(rest), streamPreheaderTerminalCode)
					switch protocol {
					case "messages":
						require.Equal(t, 1, strings.Count(string(rest), "event: message_stop"))
					case "chat":
						require.Equal(t, 1, strings.Count(string(rest), "data: [DONE]"))
					case "responses":
						require.Equal(t, 1, strings.Count(string(rest), "event: response.completed"))
					case "gemini":
						require.Contains(t, string(rest), `"finishReason":"STOP"`)
					}
				} else {
					require.ErrorIs(t, outcome.err, errStreamPreheaderTerminal)
					require.True(t, outcome.finalized)
					require.Nil(t, outcome.result)
					require.Equal(t, 1, strings.Count(string(rest), "data: "))
					require.NotContains(t, string(rest), "message_start")
					require.NotContains(t, string(rest), "message_stop")
					require.NotContains(t, string(rest), "response.completed")
					require.NotContains(t, string(rest), "[DONE]")
					if protocol == "gemini" {
						require.Contains(t, string(rest), `"code":`+strconv.Itoa(status))
						require.Contains(t, string(rest), `"message":"fixture rejection"`)
						require.Contains(t, string(rest), `"domain":"sub2api.gateway"`)
						require.Contains(t, string(rest), `"reason":"GATEWAY_STREAM_TERMINAL_ERROR"`)
						require.Contains(t, string(rest), `"retryDelay":"17s"`)
						require.NotContains(t, string(rest), "event: error")
					} else {
						require.Contains(t, string(rest), `"code":"gateway_stream_terminal_error"`)
						require.Contains(t, string(rest), `"message":"fixture rejection"`)
						require.Contains(t, string(rest), `"retry_after":"17"`)
						wantType := "authentication_error"
						if status == 429 {
							wantType = "rate_limit_error"
						}
						require.Contains(t, string(rest), `"type":"`+wantType+`"`)
					}
				}
			})
		}
	}
}

func TestStreamPreheaderWireFixtures(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for protocol, name := range map[streamPreheaderProtocol]string{
		streamPreheaderMessages: "messages", streamPreheaderGemini: "gemini",
		streamPreheaderChat: "chat", streamPreheaderResponses: "responses",
	} {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile("testdata/stream_preheader/" + name + "_429.sse")
			require.NoError(t, err)
			got := streamPreheaderPing(protocol) + buildStreamPreheaderError(protocol, 429, "rate_limit_error", "Upstream rate limit exceeded", "17", now,
				streamPreheaderResponseMeta{id: "resp_terminal_fixture", model: "fixture-model"})
			require.Equal(t, string(want), got)
		})
	}
}

func TestStreamPreheaderCancellationStopsSingleUpstream(t *testing.T) {
	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			canceled := make(chan struct{})
			server, upstream, outcomes := preheaderAGServer(t, protocol, true, func(w http.ResponseWriter, r *http.Request) {
				// net/http starts monitoring the connection for cancellation only
				// after the request body has been consumed.
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-time.After(4 * time.Second):
				}
			}, nil)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody(protocol, true)))
			require.NoError(t, err)
			readPreheaderPing(t, response, protocol)
			require.NoError(t, response.Body.Close())
			outcome := awaitPreheaderOutcome(t, outcomes)
			require.ErrorIs(t, outcome.err, context.Canceled)
			require.Nil(t, outcome.result)
			require.True(t, outcome.finalized)
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream context not canceled")
			}
			require.EqualValues(t, 1, upstream.calls.Load())
		})
	}
}

func TestStreamPreheaderPolicyOptInOnly(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, stream bool
		interval        int
		protocol        string
	}{
		{"default", false, true, 10, "messages"},
		{"nonstream", true, false, 10, "messages"},
		{"disabled-interval", true, true, 0, "messages"},
		{"native-ws", true, true, 10, "websocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newAntigravityCompatContext("POST", "/v1/messages", nil)
			ConfigureStreamPreheader(c, &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: tc.enabled, StreamKeepaliveInterval: tc.interval}}, tc.protocol, tc.stream)
			require.False(t, StreamPreheaderConfigured(c))
			req := httptest.NewRequest("POST", "http://fixture.invalid", nil)
			resp, err, terminal := requestStreamPreheader(c, req, nil, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"17"}, "X-Fixture": []string{"original"}}, Body: io.NopCloser(strings.NewReader("original body"))}, nil
			}, nil)
			require.NoError(t, err)
			require.False(t, terminal)
			require.Equal(t, 429, resp.StatusCode)
			require.Equal(t, "17", resp.Header.Get("Retry-After"))
			require.Equal(t, "original", resp.Header.Get("X-Fixture"))
			body, _ := io.ReadAll(resp.Body)
			require.Equal(t, "original body", string(body))
			require.False(t, c.Writer.Written())
			require.Empty(t, recorder.Body.String())
		})
	}
}

func TestStreamPreheaderGoogleErrorInfoAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ header, raw, delay string }{
		{"17", "17", "17s"},
		{now.Add(23 * time.Second).Format(http.TimeFormat), now.Add(23 * time.Second).Format(http.TimeFormat), "23s"},
		{now.Add(-time.Minute).Format(http.TimeFormat), now.Add(-time.Minute).Format(http.TimeFormat), "0s"},
		{"nonsense", "", ""},
		{"-1", "", ""},
		{"315576000001", "315576000001", ""},
	} {
		t.Run(tc.header, func(t *testing.T) {
			frame := buildStreamPreheaderError(streamPreheaderGemini, 429, "RESOURCE_EXHAUSTED", "slow", tc.header, now)
			payload := strings.TrimSpace(strings.TrimPrefix(frame, "data: "))
			require.True(t, gjson.Valid(payload))
			require.Equal(t, gjson.Number, gjson.Get(payload, "error.code").Type)
			require.EqualValues(t, 429, gjson.Get(payload, "error.code").Int())
			require.Equal(t, "RESOURCE_EXHAUSTED", gjson.Get(payload, "error.status").String())
			require.Equal(t, "sub2api.gateway", gjson.Get(payload, "error.details.0.domain").String())
			require.Equal(t, "GATEWAY_STREAM_TERMINAL_ERROR", gjson.Get(payload, "error.details.0.reason").String())
			require.Equal(t, tc.raw, gjson.Get(payload, "error.details.0.metadata.retryAfter").String())
			require.Equal(t, tc.delay, gjson.Get(payload, "error.details.1.retryDelay").String())
		})
	}
	for _, protocol := range []streamPreheaderProtocol{streamPreheaderMessages, streamPreheaderChat, streamPreheaderResponses} {
		frame := buildStreamPreheaderError(protocol, 429, "custom_limit_type", "slow", "17", now)
		require.Contains(t, frame, `"type":"custom_limit_type"`)
		require.Contains(t, frame, `"status":429`)
		require.Contains(t, frame, `"code":"gateway_stream_terminal_error"`)
		require.NotContains(t, frame, "executed")
	}
}

func TestStreamPreheaderHandoffKeepsSlowFirstTokenAlive(t *testing.T) {
	head, token := make(chan struct{}), make(chan struct{})
	var headOnce, tokenOnce sync.Once
	releaseHead := func() { headOnce.Do(func() { close(head) }) }
	releaseToken := func() { tokenOnce.Do(func() { close(token) }) }
	defer releaseHead()
	defer releaseToken()
	server, upstream, outcomes := preheaderAGServer(t, "messages", true, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-head:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "handoff-actual-request")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		comments := time.NewTicker(25 * time.Millisecond)
		defer comments.Stop()
		for {
			select {
			case <-comments.C:
				_, _ = io.WriteString(w, ": filtered upstream comment\n\n")
				w.(http.Flusher).Flush()
			case <-token:
				resp := antigravityCompatSuccessResponse()
				defer resp.Body.Close()
				_, _ = io.Copy(w, resp.Body)
				return
			case <-r.Context().Done():
				return
			}
		}
	}, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("messages", true)))
	require.NoError(t, err)
	defer response.Body.Close()
	readPreheaderPing(t, response, "messages")
	releaseHead()
	const bodyPing = "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	got := make([]byte, len(bodyPing))
	_, err = io.ReadFull(response.Body, got)
	require.NoError(t, err)
	require.Equal(t, bodyPing, string(got), "the existing body heartbeat, not the preheader timer, keeps the slow first token alive")
	releaseToken()
	wire, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "filtered upstream comment")
	require.Equal(t, 1, strings.Count(string(wire), "event: message_stop"))
	require.EqualValues(t, 1, upstream.calls.Load())
	outcome := awaitPreheaderOutcome(t, outcomes)
	require.NoError(t, outcome.err)
	require.Equal(t, "handoff-actual-request", outcome.result.RequestID)
	require.Equal(t, 8, outcome.result.Usage.InputTokens)
	require.Equal(t, 3, outcome.result.Usage.OutputTokens)
}

func TestStreamPreheaderConvertedBodyWaitAndMissingTerminal(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			result := make(chan preheaderForwardOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				ConfigureStreamPreheader(c, cfg, protocol, true)
				gateway := &GatewayService{cfg: cfg}
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: pr}
				var res *ForwardResult
				var err error
				if protocol == "chat" {
					res, err = gateway.handleCCStreamingFromAnthropic(resp, c, "fixture-model", "fixture-model", nil, time.Now(), false)
				} else {
					res, err = gateway.handleResponsesStreamingResponse(resp, c, "fixture-model", "fixture-model", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				}
				result <- preheaderForwardOutcome{res, err, StreamPreheaderFinalized(c)}
			}))
			t.Cleanup(server.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Get(server.URL)
			require.NoError(t, err)
			defer response.Body.Close()
			readPreheaderPing(t, response, protocol)
			_, err = io.WriteString(pw, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_fixture_same\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[],\"usage\":{\"input_tokens\":8}}}\n\n")
			require.NoError(t, err)
			require.NoError(t, pw.Close()) // No upstream message_stop: must not synthesize success.
			wire, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			outcome := awaitPreheaderOutcome(t, result)
			require.ErrorIs(t, outcome.err, errStreamPreheaderTerminal)
			require.True(t, outcome.finalized)
			require.Equal(t, 8, outcome.result.Usage.InputTokens)
			require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
			require.NotContains(t, string(wire), "[DONE]")
			require.NotContains(t, string(wire), "response.completed")
			if protocol == "responses" {
				require.Contains(t, string(wire), "event: response.failed\n")
				require.Contains(t, string(wire), `"id":"resp_fixture_same"`)
			}
		})
	}
}

func TestStreamPreheaderLateTransportEOFDoesNotRetry(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server, upstream, outcomes := preheaderAGServer(t, "messages", true, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("messages", true)))
	require.NoError(t, err)
	defer response.Body.Close()
	readPreheaderPing(t, response, "messages")
	unblock()
	wire, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(wire), "event: error"))
	require.Contains(t, string(wire), `"code":"gateway_stream_terminal_error"`)
	require.Contains(t, string(wire), `"status":502`)
	require.NotContains(t, string(wire), "message_stop")
	outcome := awaitPreheaderOutcome(t, outcomes)
	require.ErrorIs(t, outcome.err, errStreamPreheaderTerminal)
	require.Nil(t, outcome.result)
	require.EqualValues(t, 1, upstream.calls.Load())
}

func TestStreamPreheaderLateErrorBodyKeepsAliveAndCancels(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelClient), func(t *testing.T) {
			head, body := make(chan struct{}), make(chan struct{})
			var headOnce, bodyOnce sync.Once
			releaseHead := func() { headOnce.Do(func() { close(head) }) }
			releaseBody := func() { bodyOnce.Do(func() { close(body) }) }
			defer releaseHead()
			defer releaseBody()
			canceled := make(chan struct{})
			server, upstream, outcomes := preheaderAGServer(t, "messages", true, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-head:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(http.StatusTooManyRequests)
				w.(http.Flusher).Flush()
				select {
				case <-body:
					_, _ = io.WriteString(w, `{"error":{"type":"fixture_rate_limit","message":"slow rejection"}}`)
				case <-r.Context().Done():
					close(canceled)
				}
			}, nil)
			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Post(server.URL, "application/json", bytes.NewReader(preheaderAGBody("messages", true)))
			require.NoError(t, err)
			defer response.Body.Close()
			readPreheaderPing(t, response, "messages")
			releaseHead()
			readPreheaderPing(t, response, "messages")
			if cancelClient {
				require.NoError(t, response.Body.Close())
				select {
				case <-canceled:
				case <-time.After(2 * time.Second):
					t.Fatal("late rejection body did not stop on cancellation")
				}
			} else {
				releaseBody()
				wire, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, 1, strings.Count(string(wire), "event: error"))
				require.Contains(t, string(wire), `"type":"fixture_rate_limit"`)
				require.Contains(t, string(wire), `"retry_after":"17"`)
				require.Contains(t, string(wire), `"code":"gateway_stream_terminal_error"`)
			}
			outcome := awaitPreheaderOutcome(t, outcomes)
			if cancelClient {
				require.ErrorIs(t, outcome.err, context.Canceled)
			} else {
				require.ErrorIs(t, outcome.err, errStreamPreheaderTerminal)
			}
			require.True(t, outcome.finalized)
			require.Nil(t, outcome.result)
			require.EqualValues(t, 1, upstream.calls.Load())
		})
	}
}

func TestStreamPreheaderNativeBodyWaitAndFailure(t *testing.T) {
	for _, route := range []string{"messages", "messages-passthrough", "responses", "responses-passthrough", "gemini", "responses-fast-headers"} {
		t.Run(route, func(t *testing.T) {
			kind := strings.TrimSuffix(route, "-fast-headers")
			fastHeaders := kind != route
			protocol := strings.TrimSuffix(kind, "-passthrough")
			head, body := make(chan struct{}), make(chan struct{})
			var headOnce, bodyOnce sync.Once
			releaseHead := func() { headOnce.Do(func() { close(head) }) }
			releaseBody := func() { bodyOnce.Do(func() { close(body) }) }
			defer releaseHead()
			defer releaseBody()
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-head:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				select {
				case <-body:
				case <-r.Context().Done():
					return
				}
				switch protocol {
				case "messages":
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"usage\":{\"input_tokens\":8}}}\n\n")
				case "responses":
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				case "gemini":
					_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}\n\n")
				}
			}))
			t.Cleanup(upstreamServer.Close)
			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			outcomes := make(chan preheaderForwardOutcome, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				ConfigureStreamPreheader(c, cfg, protocol, true)
				account := &Account{ID: 9300, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				request, _ := http.NewRequestWithContext(r.Context(), "POST", upstreamServer.URL, nil)
				openai := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				resp, err, terminal := openai.doOpenAIStreamUpstream(c, request, "", account)
				if err != nil || terminal {
					outcomes <- preheaderForwardOutcome{err: err, finalized: terminal}
					return
				}
				defer resp.Body.Close()
				result := &ForwardResult{}
				switch kind {
				case "messages", "messages-passthrough":
					gateway := &GatewayService{cfg: cfg}
					var sr *streamingResult
					if kind == "messages" {
						sr, err = gateway.handleStreamingResponse(r.Context(), resp, c, account, time.Now(), "fixture", "fixture", false)
					} else {
						sr, err = gateway.handleStreamingResponseAnthropicAPIKeyPassthrough(r.Context(), resp, c, account, time.Now(), "fixture")
					}
					if sr != nil {
						result.Usage = *sr.usage
					}
				case "responses":
					_, err = openai.handleStreamingResponseWithReasoning(r.Context(), resp, c, account, time.Now(), "fixture", "fixture", "")
				case "responses-passthrough":
					_, err = openai.handleStreamingResponsePassthrough(r.Context(), resp, c, account, time.Now(), "fixture", "fixture")
				case "gemini":
					var sr *geminiNativeStreamResult
					sr, err = (&GeminiMessagesCompatService{cfg: cfg}).handleNativeStreamingResponse(c, resp, time.Now(), false)
					if sr != nil {
						result.Usage = *sr.usage
					}
				}
				outcomes <- preheaderForwardOutcome{result, err, StreamPreheaderFinalized(c)}
			}))
			t.Cleanup(server.Close)
			if fastHeaders {
				releaseHead()
			}
			response, err := (&http.Client{Timeout: 6 * time.Second}).Get(server.URL)
			require.NoError(t, err)
			defer response.Body.Close()
			if !fastHeaders {
				readPreheaderPing(t, response, protocol)
				releaseHead()
			}
			reader := bufio.NewReader(response.Body)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			if fastHeaders {
				require.Equal(t, ":\n", line, "the existing body owner committed the response")
			} else {
				require.True(t, strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event: ping"), line)
			}
			releaseBody()
			wire, err := io.ReadAll(reader)
			require.NoError(t, err)
			outcome := awaitPreheaderOutcome(t, outcomes)
			require.Error(t, outcome.err)
			require.True(t, outcome.finalized)
			if protocol == "gemini" {
				require.Contains(t, string(wire), `"reason":"GATEWAY_STREAM_TERMINAL_ERROR"`)
			} else {
				require.Equal(t, 1, strings.Count(string(wire), streamPreheaderTerminalCode))
			}
			require.NotContains(t, string(wire), "response.completed")
			require.NotContains(t, string(wire), "message_stop")
			if protocol != "responses" {
				require.Equal(t, 8, outcome.result.Usage.InputTokens)
			}
			require.EqualValues(t, 1, upstream.calls.Load())
		})
	}
}

func TestStreamPreheaderGeminiMessagesDoesNotSynthesizeSuccess(t *testing.T) {
	for _, payload := range []string{
		"",
		`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`,
		`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"complete"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c, recorder := newAntigravityCompatContext("POST", "/v1/messages", nil)
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			ConfigureStreamPreheader(c, cfg, "messages", true)
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(streamPreheaderPing(streamPreheaderMessages))
			c.Writer.Flush()
			markStreamPreheaderBodyHeartbeat(c)
			body := ""
			if payload != "" {
				body = "data: " + payload + "\n\n"
			}
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			result, err := (&GeminiMessagesCompatService{cfg: cfg}).handleStreamingResponse(c, resp, time.Now(), "arbitrary-alias")
			wire := recorder.Body.String()
			if strings.Contains(payload, `"finishReason"`) {
				require.NoError(t, err)
				require.Equal(t, 1, strings.Count(wire, "event: message_stop"))
				require.NotContains(t, wire, streamPreheaderTerminalCode)
			} else {
				require.ErrorIs(t, err, errStreamPreheaderTerminal)
				require.Equal(t, 1, strings.Count(wire, streamPreheaderTerminalCode))
				require.NotContains(t, wire, "event: message_stop")
			}
			if payload == "" {
				require.NotContains(t, wire, "event: message_start")
				require.Zero(t, result.usage.InputTokens)
			} else {
				require.Equal(t, 8, result.usage.InputTokens)
				require.Equal(t, 3, result.usage.OutputTokens)
			}
		})
	}
}

func TestFinishStreamPreheaderFailover_PreservesResponseBodyMessageAndRetryDelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}

	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			c, recorder := newAntigravityCompatContext("POST", "/v1/"+protocol, nil)
			ConfigureStreamPreheader(c, cfg, protocol, true)
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
			markStreamPreheaderBodyHeartbeat(c)

			failoverErr := &UpstreamFailoverError{
				StatusCode: http.StatusTooManyRequests,
				ResponseBody: []byte(`{"error":{"code":429,"message":"Quota exceeded for model claude-sonnet-4-5 at ?key=AIzaSySecretKey12345678901234567890","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"9.5s"}]}}`),
			}
			handled := FinishStreamPreheaderFailover(c, failoverErr)
			require.True(t, handled)
			require.True(t, StreamPreheaderFinalized(c))

			wire := recorder.Body.String()
			require.Contains(t, wire, "Quota exceeded for model claude-sonnet-4-5 at ?key=***")
			require.NotContains(t, wire, "AIzaSySecretKey")
			if protocol == "gemini" {
				require.Contains(t, wire, `"retryDelay":"9.5s"`)
				require.Contains(t, wire, `"status":"RESOURCE_EXHAUSTED"`)
			} else {
				require.Contains(t, wire, `"retry_after":"9.5"`)
				require.Contains(t, wire, `"code":"gateway_stream_terminal_error"`)
			}
		})
	}
}

func TestStreamPreheaderLateError_PreservesStructuredDiagnosticsWithoutReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lateErrorJSON := `{"response":{"error":{"code":400,"type":"custom_tool_schema_error","status":"INVALID_ARGUMENT","message":"Invalid tool parameter at ?key=AIzaSySecretKey12345678901234567890","param":"tools[2].input_schema","requestId":"agy-late-req-42","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","violationId":9007199254740997,"fieldViolations":[{"field":"tools[2].input_schema","description":"schema depth exceeded"}]}]}}}`

	for _, protocol := range []string{"messages", "gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			head := make(chan struct{})
			var headOnce sync.Once
			releaseHead := func() { headOnce.Do(func() { close(head) }) }
			defer releaseHead()

			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-head:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Goog-Request-Id", "goog-hdr-req-42")
				w.Header().Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, lateErrorJSON)
			}))
			t.Cleanup(upstreamServer.Close)

			base, err := url.Parse(upstreamServer.URL)
			require.NoError(t, err)
			upstream := &preheaderHTTPUpstream{base: base, client: upstreamServer.Client()}
			cfg := &config.Config{Gateway: config.GatewayConfig{StreamPreheaderKeepaliveEnabled: true, StreamKeepaliveInterval: 1}}
			outcomes := make(chan preheaderForwardOutcome, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				ConfigureStreamPreheader(c, cfg, protocol, true)
				account := &Account{ID: 9500, Platform: PlatformAntigravity, Type: AccountTypeOAuth}
				request, _ := http.NewRequestWithContext(r.Context(), "POST", upstreamServer.URL, nil)
				openai := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				resp, err, terminal := openai.doOpenAIStreamUpstream(c, request, "", account)
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				outcomes <- preheaderForwardOutcome{err: err, finalized: terminal}
			}))
			t.Cleanup(server.Close)

			response, err := (&http.Client{Timeout: 6 * time.Second}).Get(server.URL)
			require.NoError(t, err)
			defer response.Body.Close()

			// Wait for initial keepalive ping (commits HTTP 200 to downstream)
			readPreheaderPing(t, response, protocol)
			require.Equal(t, http.StatusOK, response.StatusCode)

			// Now release upstream late HTTP 400 error
			releaseHead()

			wireBytes, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			wire := string(wireBytes)

			outcome := awaitPreheaderOutcome(t, outcomes)
			require.ErrorIs(t, outcome.err, errStreamPreheaderTerminal)
			require.True(t, outcome.finalized)
			// Strictly 1 upstream call — never replayed
			require.EqualValues(t, 1, upstream.calls.Load())

			// Credentials sanitized
			require.NotContains(t, wire, "AIzaSySecretKey12345678901234567890")
			require.Contains(t, wire, "Invalid tool parameter at ?key=***")
			// 64-bit integer and structured diagnostic fields preserved
			require.Contains(t, wire, `"violationId":9007199254740997`)
			require.Contains(t, wire, `"schema depth exceeded"`)
			require.Contains(t, wire, `"tools[2].input_schema"`)
			require.Contains(t, wire, `"agy-late-req-42"`)
			require.Contains(t, wire, `"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`)

			if protocol == "gemini" {
				require.Contains(t, wire, `"reason":"GATEWAY_STREAM_TERMINAL_ERROR"`)
				require.Contains(t, wire, `"status":"INVALID_ARGUMENT"`)
			} else {
				require.Equal(t, 1, strings.Count(wire, streamPreheaderTerminalCode))
				require.Contains(t, wire, `"type":"custom_tool_schema_error"`)
			}
		})
	}
}
