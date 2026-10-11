package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestAntigravityRealForwardStream(t *testing.T) {
	effortTestSettings(t, "extra-low")
	cases := []struct {
		name, frames, model, effort, target string
		success                             bool
	}{
		{"content terminal echo", `data: {"response":{"modelVersion":"runtime-first","candidates":[{"content":{"parts":[{"text":"Hello "}]}}]}}

data: {"response":{"modelVersion":"runtime-final","candidates":[{"content":{"parts":[{"text":"world"}]},"finishReason":"STOP"}]}}

`, "gemini-custom", "high", "gemini-custom-high", true},
		{"template named default", `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}}

`, "gemini-custom", "", "gemini-custom-high", true},
		{"complete suffix wins", `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}}

`, "gemini-custom-low", "high", "gemini-custom-low", true},
		{"premature EOF", `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}}

`, "gemini-custom", "", "gemini-custom-high", false},
		{"thought only terminal", `data: {"response":{"candidates":[{"content":{"parts":[{"text":"private thought","thought":true,"thoughtSignature":"fixture"}]},"finishReason":"STOP"}]}}

`, "gemini-custom", "", "gemini-custom-high", false},
		{"empty terminal", `data: {"response":{"candidates":[{"finishReason":"STOP"}]}}

`, "gemini-custom", "", "gemini-custom-high", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.frames))}}}
			gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			service := &AccountTestService{antigravityGatewayService: gateway}
			account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort:high}"})
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
			c.Set(accountTestPromptKey, "原词 exactly unchanged")
			c.Set(accountTestRequestedKey, tt.model)
			c.Set(accountTestEffortKey, tt.effort)
			err := service.testAntigravityAccountConnection(c, account, tt.model)
			if tt.success {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Len(t, upstream.requestBodies, 1)
			require.Equal(t, tt.target, gjson.GetBytes(upstream.requestBodies[0], "model").String())
			require.Contains(t, string(upstream.requestBodies[0]), "原词 exactly unchanged")
			require.Equal(t, tt.success, strings.Contains(recorder.Body.String(), `"success":true`))
			require.Contains(t, recorder.Body.String(), `"stream_mode":"stream"`)
			require.NotContains(t, recorder.Body.String(), "private thought")
			if tt.name == "content terminal echo" {
				require.Contains(t, recorder.Body.String(), `"upstream_response_model":"runtime-final"`)
				require.Contains(t, recorder.Body.String(), `"upstream_response_model_conflict":true`)
				require.Contains(t, recorder.Body.String(), `"text":"Hello "`)
				require.Contains(t, recorder.Body.String(), `"text":"world"`)
			}
			if tt.name == "template named default" {
				require.Contains(t, recorder.Body.String(), `"effective_effort":"high"`)
				require.NotContains(t, recorder.Body.String(), `"upstream_response_model":"gemini-custom-high"`)
			}
		})
	}
}

func TestAccountTestRawTapErrorsAndCancellation(t *testing.T) {
	service := &AccountTestService{}
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	state := &accountTestRawStreamState{service: service, admin: c, observer: &upstreamResponseModelObserver{}}
	state.observe([]byte(`data: {"response":{"modelVersion":"raw-runtime","candidates":[{"finishReason":"STOP"}]}}`), io.ErrUnexpectedEOF)
	require.ErrorIs(t, state.failure, io.ErrUnexpectedEOF)
	require.False(t, state.terminal)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	writer := &accountTestForwardWriter{ResponseWriter: c.Writer, service: service, admin: c, headers: make(http.Header)}
	_, err := fmt.Fprintf(writer, "data: %s\n\n", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"after cancel"}}`)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, recorder.Body.String())
}

type accountTestIncrementalRecorder struct {
	*httptest.ResponseRecorder
	content chan struct{}
}

func (r *accountTestIncrementalRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if strings.Contains(string(p), `"type":"content"`) {
		select {
		case r.content <- struct{}{}:
		default:
		}
	}
	return n, err
}
func TestAccountTestAntigravityEmitsBeforeUpstreamCompletes(t *testing.T) {
	effortTestSettings(t)
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	recorder := &accountTestIncrementalRecorder{ResponseRecorder: httptest.NewRecorder(), content: make(chan struct{}, 1)}
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Request = httptest.NewRequest("POST", "/test", nil).WithContext(ctx)
	c.Set(accountTestPromptKey, "incremental fixture")
	account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort}"})
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}}}
	svc := &AccountTestService{antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)}
	release := make(chan struct{})
	var once sync.Once
	releaseUpstream := func() { once.Do(func() { close(release) }) }
	defer releaseUpstream()
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer func() { _ = writer.Close() }()
		_, _ = io.WriteString(writer, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first delta\"}]}}]}}\n\n")
		<-release
		_, _ = io.WriteString(writer, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" second delta\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
	}()
	done := make(chan error, 1)
	go func() { done <- svc.testAntigravityAccountConnection(c, account, "gemini-custom") }()
	select {
	case <-recorder.content:
	case <-time.After(3 * time.Second):
		t.Fatal("first delta was buffered until EOF")
	}
	select {
	case err := <-done:
		t.Fatalf("generation finished before upstream terminal: %v", err)
	default:
	}
	releaseUpstream()
	require.NoError(t, <-done)
	<-producerDone
	require.Contains(t, recorder.Body.String(), `"text":"first delta"`)
	require.Contains(t, recorder.Body.String(), `"text":" second delta"`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

type accountTestErrorReader struct{}

func (accountTestErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestAccountTestTerminalCannotMaskTransportError(t *testing.T) {
	c, recorder := newAntigravityCompatContext("POST", "/test", nil)
	body := io.MultiReader(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"usable\"}\n\ndata: {\"type\":\"response.completed\"}"), accountTestErrorReader{})
	err := (&AccountTestService{}).processOpenAIStream(c, body)
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), `"type":"error"`)
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestAntigravityWithThoughtContinuation(t *testing.T) {
	effortTestSettings(t)
	for _, failAppend := range []bool{false, true} {
		t.Run(fmt.Sprintf("append_failure_%t", failAppend), func(t *testing.T) {
			first := strings.Replace(antigravityThoughtGenerationOneLine, `"responseId":"resp_gen1"`, `"responseId":"resp_gen1","modelVersion":"runtime-first"`, 1)
			second := antigravityThoughtStreamResponse(strings.Replace(antigravityThoughtGenerationTwoLine, `"responseId":"resp_gen2"`, `"responseId":"resp_gen2","modelVersion":"runtime-final"`, 1))
			if failAppend {
				second = &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture quota"}}`))}
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(first), second}}
			gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			service := &AccountTestService{antigravityGatewayService: gateway}
			account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort}"})
			account.Extra = map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: true, AntigravityContinueThoughtOnlyExtraKey: true}
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
			c.Set(accountTestPromptKey, "原词 exactly unchanged")
			c.Set(accountTestRequestedKey, "gemini-custom")
			c.Set(accountTestEffortKey, "medium")
			err := service.testAntigravityAccountConnection(c, account, "gemini-custom")
			if failAppend {
				require.Error(t, err)
				require.Contains(t, recorder.Body.String(), `"type":"error"`)
				require.NotContains(t, recorder.Body.String(), `"success":true`)
			} else {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), `"text":"answer"`)
				require.Contains(t, recorder.Body.String(), `"success":true`)
				require.Contains(t, recorder.Body.String(), `"upstream_response_model":"runtime-final"`)
			}
			require.Len(t, upstream.requestBodies, 2, "the admin stream uses the same single continuation budget")
			require.NotContains(t, recorder.Body.String(), "sig-plan")
			require.NotContains(t, recorder.Body.String(), `"text":"plan"`)
			requests := upstreamBodies(t, upstream)
			require.Equal(t, "gemini-custom-medium", requests[0].Model)
			require.Equal(t, requests[0].Model, requests[1].Model)
			require.Equal(t, requests[0].Request.GenerationConfig, requests[1].Request.GenerationConfig)
			require.Equal(t, requests[0].Request.SystemInstruction, requests[1].Request.SystemInstruction)
			n := len(requests[0].Request.Contents)
			require.Len(t, requests[1].Request.Contents, n+1)
			require.Equal(t, requests[0].Request.Contents, requests[1].Request.Contents[:n])
			tail := gjson.GetBytes(upstream.requestBodies[1], "request.contents").Array()[n]
			require.Equal(t, "model", tail.Get("role").String())
			require.Equal(t, "plan", tail.Get("parts.0.text").String())
			require.Equal(t, "sig-plan", tail.Get("parts.0.thoughtSignature").String())
		})
	}
}
