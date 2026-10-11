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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Formal-caller fixtures for Task24 A/B: they drive the same public entry points the gateway
// uses (Forward, ForwardGemini, ForwardAsChatCompletions, ForwardAsResponses) and assert the
// downstream wire, the exact upstream envelopes, the usage and the single append budget.

// gen1: the upstream returns only thinking (with its real signature) and completes normally.
const antigravityThoughtGenerationOneLine = `data: {"response":{"responseId":"resp_gen1","candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`

// gen2: the appended generation answers with visible text.
const antigravityThoughtGenerationTwoLine = `data: {"response":{"responseId":"resp_gen2","candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":4}}}`

const (
	antigravityThoughtGenerationOneInput  = 8
	antigravityThoughtGenerationOneOutput = 5 // candidatesTokenCount + thoughtsTokenCount
	antigravityThoughtGenerationTwoInput  = 20
	antigravityThoughtGenerationTwoOutput = 4
)

func newAntigravityThoughtTestService(t *testing.T, global AntigravityThoughtPolicySettings, upstream HTTPUpstream) *AntigravityGatewayService {
	t.Helper()
	// The published pair is process state shared with other tests, so it is restored.
	previous := antigravityThoughtPolicySnapshot.Load()
	antigravityThoughtPolicySnapshot.Store(nil)
	t.Cleanup(func() { antigravityThoughtPolicySnapshot.Store(previous) })

	repo := newThoughtPolicySettingRepo()
	settings := NewSettingService(repo, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}})
	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(context.Background(), global))
	tokenProvider := NewAntigravityTokenProvider(nil, &antigravityCompatTokenCache{token: "fresh-oauth-token"}, nil)
	return NewAntigravityGatewayService(nil, nil, nil, tokenProvider, nil, upstream, settings, nil)
}

func antigravityThoughtStreamResponse(lines ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"request-3757"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))),
	}
}

// antigravityThoughtAccount maps the client alias used by the fixtures to a Gemini target,
// so the same account drives the Claude, Chat Completions and Responses entry points.
func antigravityThoughtAccount(extra map[string]any) *Account {
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	account.Credentials["model_mapping"] = map[string]any{
		"client-alias":        "gemini-3.8-flash",
		"gemini-3.1-pro-high": "gemini-3.1-pro-high",
	}
	account.Extra = extra
	return account
}

func newAntigravityThoughtClaudeRequest(stream bool) ([]byte, *gin.Context, *httptest.ResponseRecorder) {
	body := []byte(`{"model":"client-alias","stream":` + boolLiteral(stream) + `,"max_tokens":64,"messages":[{"role":"user","content":"question"}]}`)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	return body, c, recorder
}

func boolLiteral(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// upstreamBodies decodes every upstream request the stub recorded.
func upstreamBodies(t *testing.T, upstream *queuedHTTPUpstreamStub) []antigravity.V1InternalRequest {
	t.Helper()
	requests := make([]antigravity.V1InternalRequest, 0, len(upstream.requestBodies))
	for _, body := range upstream.requestBodies {
		var request antigravity.V1InternalRequest
		require.NoError(t, json.Unmarshal(body, &request))
		requests = append(requests, request)
	}
	return requests
}

// requireAppendedThinkingTail asserts that the appended generation reuses the exact original
// upstream request envelope and only adds the received thinking/signature model tail.
func requireAppendedThinkingTail(
	t *testing.T,
	first, second antigravity.V1InternalRequest,
	wantTail []antigravity.GeminiContent,
) {
	t.Helper()
	require.Equal(t, first.Project, second.Project)
	require.Equal(t, first.RequestID, second.RequestID, "the appended generation reuses the same upstream request id")
	require.Equal(t, first.UserAgent, second.UserAgent)
	require.Equal(t, first.RequestType, second.RequestType)
	require.Equal(t, first.Model, second.Model, "same resolved model")
	require.Equal(t, first.Request.SessionID, second.Request.SessionID, "same session")
	require.Equal(t, first.Request.SystemInstruction, second.Request.SystemInstruction, "no added system text")
	require.Equal(t, first.Request.GenerationConfig, second.Request.GenerationConfig, "same effort/generation config")
	require.Equal(t, first.Request.ToolConfig, second.Request.ToolConfig)
	require.Equal(t, first.Request.Tools, second.Request.Tools)

	require.Len(t, second.Request.Contents, len(first.Request.Contents)+len(wantTail))
	require.Equal(t, first.Request.Contents, second.Request.Contents[:len(first.Request.Contents)],
		"the original history is repeated unchanged")
	require.Equal(t, wantTail, second.Request.Contents[len(first.Request.Contents):],
		"only the received thinking/signature model tail is appended")
}

// --- Claude protocol (Forward) ---

func TestAntigravityThoughtPolicyCombinationsOnClaudeForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name             string
		global           AntigravityThoughtPolicySettings
		accountExtra     map[string]any
		wantUpstreamCall int
		wantTerminal     bool
		wantText         bool
	}{
		{
			name:             "both off keeps the legacy completed response",
			wantUpstreamCall: 1,
			wantTerminal:     true,
		},
		{
			name:             "A only suppresses the successful terminal",
			global:           AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
			wantUpstreamCall: 1,
			wantTerminal:     false,
		},
		{
			name:             "B only appends one generation",
			global:           AntigravityThoughtPolicySettings{ContinueThoughtOnly: true},
			wantUpstreamCall: 2,
			wantTerminal:     true,
			wantText:         true,
		},
		{
			name:             "A and B together continue and keep the terminal of the last generation",
			global:           AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true},
			wantUpstreamCall: 2,
			wantTerminal:     true,
			wantText:         true,
		},
		{
			name:             "account explicit false beats a global true pair",
			global:           AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true},
			accountExtra:     map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: false, AntigravityContinueThoughtOnlyExtraKey: false},
			wantUpstreamCall: 1,
			wantTerminal:     true,
		},
		{
			name:             "account explicit true turns both switches on under a global off pair",
			accountExtra:     map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: true, AntigravityContinueThoughtOnlyExtraKey: true},
			wantUpstreamCall: 2,
			wantTerminal:     true,
			wantText:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t, tt.global, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(true)

			result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(tt.accountExtra), body, false)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.wantUpstreamCall, upstream.callCount, "one appended generation at most")

			wire := recorder.Body.String()
			require.Contains(t, wire, "signature")
			require.Contains(t, wire, "sig-plan")
			require.Equal(t, tt.wantTerminal, strings.Contains(wire, `"type":"message_stop"`),
				"the successful terminal must be withheld only when A decides so")
			require.Equal(t, tt.wantTerminal, strings.Contains(wire, `"stop_reason":"end_turn"`))
			require.Equal(t, tt.wantText, strings.Contains(wire, "answer"))
			require.Equal(t, 1, strings.Count(wire, "event: message_start"), "one legal downstream response")
			if !tt.wantTerminal {
				// A withheld terminal still reports the real usage with a null stop reason: the
				// client keeps true usage instead of losing it with the completion signal.
				require.Contains(t, wire, `"stop_reason":null`)
				require.Contains(t, wire, `"input_tokens":8`)
				require.Contains(t, wire, `"output_tokens":5`)
			}

			wantInput := antigravityThoughtGenerationOneInput
			wantOutput := antigravityThoughtGenerationOneOutput
			if tt.wantUpstreamCall == 2 {
				// Cumulative usage: both generations were billed upstream.
				wantInput += antigravityThoughtGenerationTwoInput
				wantOutput += antigravityThoughtGenerationTwoOutput
				require.Contains(t, wire, `"index":1`, "the appended generation continues the block sequence")
				require.Equal(t, 1, strings.Count(wire, `"type":"message_stop"`), "exactly one terminal")
			}
			require.Equal(t, wantInput, result.Usage.InputTokens)
			require.Equal(t, wantOutput, result.Usage.OutputTokens)

			if tt.wantUpstreamCall == 2 {
				requests := upstreamBodies(t, upstream)
				requireAppendedThinkingTail(t, requests[0], requests[1], []antigravity.GeminiContent{{
					Role: "model",
					Parts: []antigravity.GeminiPart{{
						Text:             "plan",
						Thought:          true,
						ThoughtSignature: "sig-plan",
					}},
				}})
				require.NotContains(t, string(upstream.requestBodies[1]), "[sub2api:")
				require.NotContains(t, string(upstream.requestBodies[1]), "assistant-prefill")
			}
		})
	}
}

func TestAntigravityThoughtContinuationReusesEnvelopeAndCarriers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// The upstream reports thinking text with its own signature carrier, plus a second carrier
	// that arrived without any text field: every received part is replayed verbatim and in
	// order, and an absent text field stays absent instead of being invented as "".
	firstGeneration := `data: {"response":{"responseId":"resp_gen1","candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"},{"text":"","thoughtSignature":"sig-carrier"},{"thoughtSignature":"sig-extra"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(firstGeneration),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	var urls []string
	var authorizations []string
	upstream.onCall = func(req *http.Request, _ *queuedHTTPUpstreamStub) {
		urls = append(urls, req.URL.String())
		authorizations = append(authorizations, req.Header.Get("Authorization"))
	}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount)
	require.Len(t, urls, 2)
	require.Equal(t, urls[0], urls[1], "the appended generation reuses the exact upstream envelope")
	require.Equal(t, authorizations[0], authorizations[1])

	requests := upstreamBodies(t, upstream)
	// Every received thinking part and signature carrier is replayed verbatim, in order: no
	// signature is moved onto another part and no text field is invented.
	requireAppendedThinkingTail(t, requests[0], requests[1], []antigravity.GeminiContent{{
		Role: "model",
		Parts: []antigravity.GeminiPart{
			{Text: "plan", Thought: true, ThoughtSignature: "sig-plan"},
			{Text: "", ThoughtSignature: "sig-carrier"},
			{ThoughtSignature: "sig-extra"},
		},
	}})
	appended := gjson.GetBytes(upstream.requestBodies[1], "request.contents.1.parts").Raw
	// Verbatim replay: the exact bytes the upstream sent, in the same order, including the
	// absent text field of the second carrier.
	require.Equal(t,
		`[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"},{"text":"","thoughtSignature":"sig-carrier"},{"thoughtSignature":"sig-extra"}]`,
		appended)
	require.NotContains(t, appended, `{"text":"","thoughtSignature":"sig-extra"}`,
		"a carrier whose text field was absent is not given an empty one")
	require.Contains(t, recorder.Body.String(), "answer")
}

// Task23 的消费方以 `isolated := *gateway; isolated.httpUpstream = observedUpstream` 复制服务来
// 观察追加请求。B 必须使用同一个 HTTPUpstream 端口：服务本身不能持有第二套传输，也不能有实例级
// 可变状态，否则复制出来的服务既看不到追加生成，观察到的用量也会不完整。
func TestAntigravityThoughtContinuationUsesTheIsolatedUpstreamPort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
	}}
	observed := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, original)
	isolated := *svc
	isolated.httpUpstream = observed
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)

	result, err := isolated.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, original.callCount, "追加生成必须走复制后的上游端口，而不是原服务的传输")
	require.Equal(t, 2, observed.callCount, "观察方看到第一代与追加生成两个请求")
	require.Contains(t, recorder.Body.String(), "answer")
	require.Equal(t, antigravityThoughtGenerationOneInput+antigravityThoughtGenerationTwoInput, result.Usage.InputTokens)
}

func TestAntigravityThoughtContinuationIsCappedAtOneAppend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount, "a second thought-only generation never triggers a third request")
	// B alone leaves the terminal to the resolved A setting, which is off here.
	require.Contains(t, recorder.Body.String(), "message_stop")
	require.Equal(t, antigravityThoughtGenerationOneInput*2, result.Usage.InputTokens)
	require.Equal(t, antigravityThoughtGenerationOneOutput*2, result.Usage.OutputTokens)
}

func TestAntigravityThoughtSuppressionSkipsTerminalForThinkingOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
	}}
	svc := newAntigravityThoughtTestService(t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount, "the append budget is one")
	wire := recorder.Body.String()
	require.NotContains(t, wire, `"type":"message_stop"`)
	require.NotContains(t, wire, "end_turn")
	require.Contains(t, wire, "sig-plan", "thinking and signature are kept")
	require.Contains(t, wire, `"index":1`, "the two generations still form one ordered response")
	require.Equal(t, 2, strings.Count(wire, `"type":"content_block_stop"`))
	// The withheld terminal still carries the real cumulative usage to the client.
	require.Contains(t, wire, `"stop_reason":null`)
	require.Contains(t, wire, `"input_tokens":16`)
	require.Contains(t, wire, `"output_tokens":10`)
	require.Equal(t, 2*antigravityThoughtGenerationOneInput, result.Usage.InputTokens)
	require.Equal(t, 2*antigravityThoughtGenerationOneOutput, result.Usage.OutputTokens)
}

// --- non-eligible responses keep their own semantics ---

func TestAntigravityThoughtPolicyLeavesNonEligibleResponsesUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	thinkingPart := `{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}`
	tests := []struct {
		name         string
		body         string
		wantUpstream int
		wantStop     string
	}{
		{
			name:         "output limit",
			body:         `data: {"response":{"candidates":[{"content":{"parts":[` + thinkingPart + `]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"max_tokens"`,
		},
		{
			name:         "refusal",
			body:         `data: {"response":{"candidates":[{"content":{"parts":[` + thinkingPart + `]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"end_turn"`,
		},
		{
			name:         "tool call",
			body:         `data: {"response":{"candidates":[{"content":{"parts":[` + thinkingPart + `,{"functionCall":{"id":"call-a","name":"echo","args":{}}}]},"finishReason":"STOP"}]}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"tool_use"`,
		},
		{
			name:         "visible text",
			body:         `data: {"response":{"candidates":[{"content":{"parts":[` + thinkingPart + `,{"text":"answer"}]},"finishReason":"STOP"}]}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"end_turn"`,
		},
		{
			name:         "truly empty",
			body:         `data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4}}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"end_turn"`,
		},
		{
			name:         "truncated stream",
			body:         `data: {"response":{"candidates":[{"content":{"parts":[` + thinkingPart + `]},"finishReason":""}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}}`,
			wantUpstream: 1,
			wantStop:     `"stop_reason":"end_turn"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(tt.body),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t,
				AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(true)

			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

			require.NoError(t, err)
			require.Equal(t, tt.wantUpstream, upstream.callCount, "ineligible responses are never continued")
			require.Contains(t, recorder.Body.String(), tt.wantStop)
		})
	}
}

func TestAntigravityThoughtPolicyKeepsUpstreamErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid request"}}`)),
	}}}
	svc := newAntigravityThoughtTestService(t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, upstream)
	body, c, _ := newAntigravityThoughtClaudeRequest(true)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 1, upstream.callCount, "an upstream error is neither suppressed nor continued")
}

func TestAntigravityThoughtPolicyKeepsClientCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	partial := antigravityThoughtGenerationOneLine
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &antigravityCompatErrorReader{
			data: []byte(partial),
			err:  context.Canceled,
		},
	}}}
	svc := newAntigravityThoughtTestService(t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(true)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, upstream.callCount, "a canceled read is never continued")
	require.NotContains(t, recorder.Body.String(), "message_stop")
}

// --- Claude non-streaming client ---

func TestAntigravityThoughtSuppressionOnClaudeNonStreamingClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name     string
		global   AntigravityThoughtPolicySettings
		wantStop string
		appended bool
	}{
		{name: "A suppresses the non-streaming stop reason", global: AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, wantStop: `"stop_reason":null`},
		{name: "off keeps the legacy stop reason", wantStop: `"stop_reason":"end_turn"`},
		{name: "B appends and keeps the finishing generation", global: AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, wantStop: `"stop_reason":"end_turn"`, appended: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t, tt.global, upstream)
			body, c, recorder := newAntigravityThoughtClaudeRequest(false)

			result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, recorder.Body.String(), tt.wantStop)
			require.Contains(t, recorder.Body.String(), "sig-plan")
			require.Equal(t, tt.appended, strings.Contains(recorder.Body.String(), "answer"))
			if tt.appended {
				require.Equal(t, 2, upstream.callCount)
				require.Equal(t, antigravityThoughtGenerationOneInput+antigravityThoughtGenerationTwoInput, result.Usage.InputTokens)
				require.Equal(t, antigravityThoughtGenerationOneOutput+antigravityThoughtGenerationTwoOutput, result.Usage.OutputTokens)
				require.Equal(t, int64(antigravityThoughtGenerationOneInput+antigravityThoughtGenerationTwoInput),
					gjson.Get(recorder.Body.String(), "usage.input_tokens").Int(), "the body reports cumulative usage")
			} else {
				require.Equal(t, 1, upstream.callCount)
			}
		})
	}
}

// --- tail adapter wiring ---

func TestAntigravityThoughtContinuationSwitchKeepsClientThinkingTail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"model":"client-alias","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig-plan"}]}]}`)
	for _, tt := range []struct {
		name          string
		global        AntigravityThoughtPolicySettings
		extra         map[string]any
		wantModelTail bool
	}{
		{name: "B effective keeps the signed thinking tail", global: AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, wantModelTail: true},
		{name: "A only keeps the legacy trim", global: AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}},
		{name: "account false beats the global continuation switch", global: AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, extra: map[string]any{AntigravityContinueThoughtOnlyExtraKey: false}},
		{name: "both off keeps the legacy trim"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t, tt.global, upstream)
			account := antigravityThoughtAccount(tt.extra)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", requestBody)

			_, err := svc.Forward(context.Background(), c, account, requestBody, false)

			require.NoError(t, err)
			require.Equal(t, 1, upstream.callCount)
			contents := gjson.GetBytes(upstream.requestBodies[0], "request.contents")
			if tt.wantModelTail {
				require.Equal(t, "model", contents.Array()[1].Get("role").String())
				require.Equal(t, "plan", contents.Array()[1].Get("parts.0.text").String())
				require.Equal(t, "sig-plan", contents.Array()[1].Get("parts.0.thoughtSignature").String())
				require.NotContains(t, contents.Raw, "[sub2api:")
			} else {
				// Legacy trim: the client's partial thinking tail is dropped at the user
				// boundary instead of being sent back as a model turn.
				require.Len(t, contents.Array(), 1)
				require.Equal(t, "user", contents.Array()[0].Get("role").String())
				require.NotContains(t, contents.Raw, "sig-plan")
			}
		})
	}
}

// --- native Gemini protocol (ForwardGemini) ---

func TestAntigravityThoughtPolicyOnNativeGeminiStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
	for _, tt := range []struct {
		name            string
		global          AntigravityThoughtPolicySettings
		wantUpstream    int
		wantStopSignals int
		wantCumulative  bool
	}{
		{name: "off keeps the upstream finish reason", wantUpstream: 1, wantStopSignals: 1},
		{name: "A strips only the successful finish reason", global: AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, wantUpstream: 1},
		{name: "B appends one generation and the client sees one terminal", global: AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, wantUpstream: 2, wantStopSignals: 1, wantCumulative: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t, tt.global, upstream)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/client-alias:streamGenerateContent", requestBody)

			result, err := svc.ForwardGemini(context.Background(), c, antigravityThoughtAccount(nil), "gemini-3.1-pro-high", "streamGenerateContent", true, requestBody, false)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.wantUpstream, upstream.callCount)
			wire := recorder.Body.String()
			require.Contains(t, wire, "sig-plan", "thinking and signature always reach the client")
			require.Equal(t, tt.wantStopSignals, strings.Count(wire, `"finishReason":"STOP"`))
			if tt.wantCumulative {
				require.Contains(t, wire, "answer")
				require.Contains(t, wire, `"promptTokenCount":28`, "the appended generation reports cumulative usage in gemini units")
				requests := upstreamBodies(t, upstream)
				requireAppendedThinkingTail(t, requests[0], requests[1], []antigravity.GeminiContent{{
					Role: "model",
					Parts: []antigravity.GeminiPart{{
						Text:             "plan",
						Thought:          true,
						ThoughtSignature: "sig-plan",
					}},
				}})
			}
			wantInput := antigravityThoughtGenerationOneInput
			wantOutput := antigravityThoughtGenerationOneOutput
			if tt.wantUpstream == 2 {
				wantInput += antigravityThoughtGenerationTwoInput
				wantOutput += antigravityThoughtGenerationTwoOutput
			}
			require.Equal(t, wantInput, result.Usage.InputTokens)
			require.Equal(t, wantOutput, result.Usage.OutputTokens)
		})
	}
}

func TestAntigravityThoughtPolicyOnNativeGeminiNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/client-alias:generateContent", requestBody)

	result, err := svc.ForwardGemini(context.Background(), c, antigravityThoughtAccount(nil), "gemini-3.1-pro-high", "generateContent", false, requestBody, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, upstream.callCount)
	require.NotContains(t, recorder.Body.String(), "finishReason", "the successful finish reason is suppressed")
	require.Contains(t, recorder.Body.String(), "sig-plan")
	require.Contains(t, recorder.Body.String(), "usageMetadata")
	require.Equal(t, antigravityThoughtGenerationOneInput, result.Usage.InputTokens)
}

// --- compatibility protocols (Chat Completions / Responses) ---

func TestAntigravityThoughtSuppressionOnCompatStreams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name      string
		path      string
		body      []byte
		call      func(*AntigravityGatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
		absent    []string
		expectOne []string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"client-alias","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"question"}]}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
			// The existing Chat converters surface the received thinking as reasoning
			// content; they never carry a provider signature, so only the thinking text
			// reaches this protocol. The withheld terminal is replaced by the usage-only
			// chunk the client asked for (include_usage): it still reports the real usage of
			// the suppressed response.
			absent:    []string{`"finish_reason":"stop"`, "data: [DONE]"},
			expectOne: []string{`"reasoning_content"`, `"plan"`, `"prompt_tokens":8`, `"completion_tokens":5`},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"client-alias","stream":true,"input":"question"}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
			// A suppressed response ends as an explicitly incomplete one that still carries
			// the real cumulative usage, instead of a fabricated response.completed.
			absent:    []string{"response.completed"},
			expectOne: []string{"response.output_item.done", "response.created", "encrypted_content", "response.incomplete", `"status":"incomplete"`, `"input_tokens":8`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
			}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
			c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, tt.body)

			_, err := tt.call(svc, context.Background(), c, antigravityThoughtAccount(nil), tt.body)

			require.NoError(t, err)
			require.Equal(t, 1, upstream.callCount)
			wire := recorder.Body.String()
			for _, absent := range tt.absent {
				require.NotContains(t, wire, absent, "the successful terminal must not be synthesized after EOF")
			}
			for _, expected := range tt.expectOne {
				require.Contains(t, wire, expected)
			}
		})
	}
}

func TestAntigravityThoughtContinuationOnCompatStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name string
		path string
		body []byte
		call func(*AntigravityGatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
		// terminal 是成功路径上唯一允许出现的完成信号，usage 是下游看到的累计用量。
		terminal []string
		usage    []string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"client-alias","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"question"}]}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
			terminal: []string{`"finish_reason":"stop"`, "data: [DONE]"},
			// 28/9 是两代上游用量的累计值（第一代 8/5，追加生成 20/4）。
			usage: []string{`"prompt_tokens":28`, `"completion_tokens":9`},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"client-alias","stream":true,"input":"question"}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
			terminal: []string{"event: response.completed"},
			usage:    []string{`"input_tokens":28`, `"output_tokens":9`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
			c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, tt.body)

			result, err := tt.call(svc, context.Background(), c, antigravityThoughtAccount(nil), tt.body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 2, upstream.callCount)
			wire := recorder.Body.String()
			require.Contains(t, wire, "answer")
			// 只允许一个完成信号：两代仍然是一条合法的下游响应。
			for _, terminal := range tt.terminal {
				require.Equal(t, 1, strings.Count(wire, terminal), "exactly one terminal frame")
			}
			for _, usage := range tt.usage {
				require.Contains(t, wire, usage, "下游与计费看到的是两代上游用量的累计值")
			}
			require.Equal(t, antigravityThoughtGenerationOneInput+antigravityThoughtGenerationTwoInput, result.Usage.InputTokens)
			require.Equal(t, antigravityThoughtGenerationOneOutput+antigravityThoughtGenerationTwoOutput, result.Usage.OutputTokens)
		})
	}
}

// 兼容协议的「非流式客户端」也走同一条 collect 路径：追加生成失败时下游身体是第一代的
// 非成功终态，调用方拿到可计费结果与 Task24 终态错误，不会 failover，也没有第二次上游请求。
func TestAntigravityThoughtAppendedFailureOnCompatNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name      string
		path      string
		body      []byte
		call      func(*AntigravityGatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
		want      []string
		notExpect []string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"}]}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
			want:      []string{`"finish_reason":null`, "plan"},
			notExpect: []string{`"finish_reason":"stop"`, "answer"},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"client-alias","input":"question"}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
			want:      []string{`"status":"incomplete"`, "plan"},
			notExpect: []string{`"status":"completed"`, "answer"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{
				responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine)},
				errors:    []error{nil, errors.New("dial tcp: connection refused")},
			}
			svc := newAntigravityThoughtTestService(t,
				AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
			c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, tt.body)

			result, err := tt.call(svc, context.Background(), c, antigravityThoughtAccount(nil), tt.body)

			require.Error(t, err, "追加生成失败不得被伪装成成功完成")
			require.True(t, IsAntigravityThoughtContinuationError(err), "err=%v", err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr))
			require.NotNil(t, result, "第一代必须保持可计费")
			require.Equal(t, 2, upstream.callCount, "没有重试、没有第二次上游请求")
			require.Equal(t, antigravityThoughtGenerationOneInput, result.Usage.InputTokens)
			require.Equal(t, antigravityThoughtGenerationOneOutput, result.Usage.OutputTokens)

			wire := recorder.Body.String()
			for _, want := range tt.want {
				require.Contains(t, wire, want)
			}
			for _, absent := range tt.notExpect {
				require.NotContains(t, wire, absent)
			}
		})
	}
}

func TestAntigravityThoughtSuppressionOnCompatNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name      string
		path      string
		body      []byte
		call      func(*AntigravityGatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
		expect    string
		notExpect string
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"}]}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
			expect:    `"finish_reason":null`,
			notExpect: `"finish_reason":"stop"`,
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"client-alias","input":"question"}`),
			call: func(svc *AntigravityGatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
			expect:    `"status":"incomplete"`,
			notExpect: `"status":"completed"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine),
			}}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
			c, recorder := newAntigravityCompatContext(http.MethodPost, tt.path, tt.body)

			result, err := tt.call(svc, context.Background(), c, antigravityThoughtAccount(nil), tt.body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 1, upstream.callCount)
			wire := recorder.Body.String()
			require.Contains(t, wire, tt.expect)
			require.NotContains(t, wire, tt.notExpect)
			require.Contains(t, wire, "plan", "the thinking is still delivered")
			require.Equal(t, antigravityThoughtGenerationOneInput, result.Usage.InputTokens)
		})
	}
}

// --- continuation failures keep the first generation billable ---

// thoughtFailingReader makes the body of a successfully opened upstream response fail while it
// is being read, which is the "断流" case of the appended generation.
type thoughtFailingReader struct{}

func (thoughtFailingReader) Read([]byte) (int, error) {
	return 0, errors.New("read tcp 10.0.0.1:443: connection reset by peer")
}

func (thoughtFailingReader) Close() error { return nil }

// requireFailedAppendedGeneration is the shared contract for every failure of the appended
// generation, as the formal callers (gateway handlers) see it:
//
//   - exactly one appended generation was attempted: no retry, no account switch, no new slot;
//   - Forward returns the first generation's billable result together with a Task24 terminal
//     error, so callers record the usage and then return the error;
//   - that error is neither an UpstreamFailoverError nor a context error, so the callers' only
//     failover path stays closed and no second upstream attempt can start;
//   - the client never sees a fabricated successful terminal, while the collected thinking and
//     the real usage still reach it.
func requireFailedAppendedGeneration(
	t *testing.T,
	upstream *queuedHTTPUpstreamStub,
	result *ForwardResult,
	err error,
	recorder *httptest.ResponseRecorder,
) {
	t.Helper()
	require.Error(t, err, "a failed appended generation must not be reported as a success")
	require.True(t, IsAntigravityThoughtContinuationError(err), "err=%v", err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "the terminal error must never trigger failover")
	require.False(t, errors.Is(err, context.Canceled), "the terminal error is not an ambiguous cancel")
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	require.NotNil(t, result, "the first generation must stay billable")
	require.Equal(t, 2, upstream.callCount, "exactly one appended generation: no retry, no account switch")
	require.Equal(t, antigravityThoughtGenerationOneInput, result.Usage.InputTokens, "the first generation stays billed")
	require.Equal(t, antigravityThoughtGenerationOneOutput, result.Usage.OutputTokens)

	wire := recorder.Body.String()
	require.Contains(t, wire, "sig-plan", "the collected thinking and signature are preserved")
	require.Contains(t, wire, "thought_continuation_error", "the failure is surfaced as a protocol error")
	require.Contains(t, wire, `"input_tokens":8`, "the real first-generation usage still reaches the client")
	require.Contains(t, wire, `"stop_reason":null`, "no successful stop reason is fabricated")
	require.NotContains(t, wire, `"stop_reason":"end_turn"`)
	require.NotContains(t, wire, `"type":"message_stop"`, "the failed append never reports a completed response")
	require.NotContains(t, wire, "answer")
	require.Equal(t, 1, strings.Count(wire, "event: message_start"), "still exactly one downstream response")
}

func TestAntigravityThoughtAppendedGenerationFailuresAreNeverSuccessfulTerminals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jsonError := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	tests := []struct {
		name       string
		second     *http.Response
		err        error
		wantStatus int
	}{
		{
			name: "transport failure",
			err:  errors.New("dial tcp: connection refused"),
		},
		{
			name:       "upstream 400",
			second:     jsonError(http.StatusBadRequest, `{"error":{"message":"invalid argument"}}`),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "upstream 503 overloaded",
			second:     jsonError(http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "redirect 302 is not a success",
			second: &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://example.invalid/retry"}},
				Body:       io.NopCloser(strings.NewReader("")),
			},
			wantStatus: http.StatusFound,
		},
		{
			name: "read failure after a successful status",
			second: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(thoughtFailingReader{}),
			},
		},
	}

	for _, tt := range tests {
		for _, suppress := range []bool{false, true} {
			policyName := "A off"
			global := AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}
			if suppress {
				policyName = "A on"
				global.SuppressThoughtOnlyStop = true
			}
			t.Run(tt.name+", "+policyName, func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{
					responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine), tt.second},
					errors:    []error{nil, tt.err},
				}
				svc := newAntigravityThoughtTestService(t, global, upstream)
				body, c, recorder := newAntigravityThoughtClaudeRequest(true)

				result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

				requireFailedAppendedGeneration(t, upstream, result, err, recorder)
				var terminalErr *AntigravityThoughtContinuationError
				require.ErrorAs(t, err, &terminalErr)
				require.Equal(t, tt.wantStatus, terminalErr.StatusCode,
					"the terminal error carries the upstream status the appended generation saw")
				require.NotEmpty(t, terminalErr.Stage)
				require.True(t, IsResponseCommitted(c) || recorder.Body.Len() > 0)
			})
		}
	}
}

func TestAntigravityThoughtAppendedGenerationFailureOnNonStreamingClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine)},
		errors:    []error{nil, errors.New("dial tcp: connection refused")},
	}
	svc := newAntigravityThoughtTestService(t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, upstream)
	body, c, recorder := newAntigravityThoughtClaudeRequest(false)

	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)

	// 非流式客户端同样拿到"第一代可计费结果 + Task24 终态错误"：调用方先入账再返回错误。
	require.Error(t, err)
	require.True(t, IsAntigravityThoughtContinuationError(err), "err=%v", err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.callCount)
	wire := recorder.Body.String()
	require.Contains(t, wire, "sig-plan")
	require.Contains(t, wire, `"stop_reason":null`)
	require.NotContains(t, wire, `"stop_reason":"end_turn"`)
	require.NotContains(t, wire, "answer")
	require.Equal(t, int64(antigravityThoughtGenerationOneInput), gjson.Get(wire, "usage.input_tokens").Int(),
		"the client still receives the real first-generation usage")
	require.Equal(t, int64(antigravityThoughtGenerationOneOutput), gjson.Get(wire, "usage.output_tokens").Int())
	require.Equal(t, antigravityThoughtGenerationOneInput, result.Usage.InputTokens)
	require.Equal(t, antigravityThoughtGenerationOneOutput, result.Usage.OutputTokens)
}
