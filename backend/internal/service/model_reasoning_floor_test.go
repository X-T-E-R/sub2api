package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func floorTestSettings(t *testing.T, model string) *SettingService {
	t.Helper()
	s := NewSettingService(newCodexVersionSyncSettingRepoStub(nil), nil)
	require.NoError(t, s.SetModelReasoningFloorSettings(context.Background(), ModelReasoningFloorSettings{Enabled: true, Rules: []ModelReasoningFloorRule{{Model: model, MinEffort: "xhigh"}}}))
	return s
}

func BenchmarkModelReasoningFloorLargeNoop(b *testing.B) {
	body := []byte(`{"input":"` + strings.Repeat("x", 1024*1024) + `","reasoning":{"effort":"xhigh"}}`)
	settings := ModelReasoningFloorSettings{Enabled: true, Rules: []ModelReasoningFloorRule{{Model: "target", MinEffort: "xhigh"}}}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got := applyModelReasoningFloor(body, "target", "reasoning.effort", settings)
		if &got[0] != &body[0] {
			b.Fatal("already-sufficient request was copied")
		}
	}
}

func TestModelReasoningFloorOrderAndNoops(t *testing.T) {
	settings := ModelReasoningFloorSettings{Enabled: true, Rules: []ModelReasoningFloorRule{{Model: "target", MinEffort: "xhigh"}}}
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
			t.Run(path+"/"+effort, func(t *testing.T) {
				body := []byte(`{"model":"target","input":[{"text":"untouched"}]}`)
				if effort != "" {
					if path == "reasoning.effort" {
						body = []byte(`{"reasoning":{"effort":"` + effort + `","summary":"auto"}}`)
					} else {
						body = []byte(`{"reasoning_effort":"` + effort + `"}`)
					}
				}
				got := applyModelReasoningFloor(body, "target", path, settings)
				want := "xhigh"
				if effort == "max" || effort == "ultra" {
					want = effort
				}
				require.Equal(t, want, gjson.GetBytes(got, path).String())
				require.Equal(t, body, applyModelReasoningFloor(body, "other", path, settings))
				off := settings
				off.Enabled = false
				require.Equal(t, body, applyModelReasoningFloor(body, "target", path, off))
				if effort == "xhigh" || effort == "max" || effort == "ultra" {
					require.True(t, bytes.Equal(body, got))
				}
			})
		}
	}
	for _, malformed := range []string{`{"reasoning":5}`, `{"reasoning":[]}`, `{"reasoning":{"effort":true}}`} {
		require.Equal(t, malformed, string(applyModelReasoningFloor([]byte(malformed), "target", "reasoning.effort", settings)))
	}
	groupBody, _ := ApplyOpenAIReasoningEffortPolicy([]byte(`{"reasoning":{"effort":"max"}}`), "medium", nil)
	require.Equal(t, "xhigh", gjson.GetBytes(applyModelReasoningFloor(groupBody, "target", "reasoning.effort", settings), "reasoning.effort").String())
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		mixed := applyModelReasoningFloor([]byte(`{"reasoning":{"effort":"low"},"reasoning_effort":"medium"}`), "target", path, settings)
		require.Equal(t, "xhigh", gjson.GetBytes(mixed, "reasoning.effort").String())
		require.Equal(t, "xhigh", gjson.GetBytes(mixed, "reasoning_effort").String())
	}
	legacyMax := []byte(`{"reasoning_effort":"max"}`)
	require.Equal(t, legacyMax, applyModelReasoningFloor(legacyMax, "target", "reasoning.effort", settings))
}

func TestModelReasoningFloorHTTPRetryUsesFallbackModel(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "transformed", true: "passthrough"}[passthrough], func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","stream":false,"instructions":"compact fixture","reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
			c := newOpenAICompactFallbackTestContext(t, "/v1/responses")
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			MarkOpenAINativeCompactionV2(c)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"context_length_exceeded","message":"context window exceeded"}}`))},
				{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_compact_fixture","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))},
			}}
			s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}}, httpUpstream: upstream, settingService: floorTestSettings(t, "gpt-5.4")}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "synthetic-account"}, Extra: map[string]any{"openai_passthrough": passthrough}}
			result, err := s.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Len(t, upstream.bodies, 2)
			require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.bodies[1], "model").String())
			require.Equal(t, "xhigh", gjson.GetBytes(upstream.bodies[1], "reasoning.effort").String())
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, "xhigh", *result.ReasoningEffort)
		})
	}
}

func TestModelReasoningFloorPersistenceAndValidation(t *testing.T) {
	repo := newCodexVersionSyncSettingRepoStub(nil)
	s := NewSettingService(repo, nil)
	value, err := s.GetModelReasoningFloorSettings(context.Background())
	require.NoError(t, err)
	require.False(t, value.Enabled)
	require.Empty(t, value.Rules)
	valid := ModelReasoningFloorSettings{Enabled: true, Rules: []ModelReasoningFloorRule{{Model: " target ", MinEffort: "extra-high"}}}
	require.NoError(t, s.SetModelReasoningFloorSettings(context.Background(), valid))
	valid.Rules[0].Model = "changed outside snapshot"
	gw := &OpenAIGatewayService{settingService: s}
	account := &Account{Platform: PlatformOpenAI}
	body := []byte(`{"model":"target"}`)
	require.Equal(t, "xhigh", gjson.GetBytes(gw.applyModelReasoningFloor(account, "target", body, "reasoning.effort"), "reasoning.effort").String())
	restarted := NewSettingService(repo, nil)
	require.NoError(t, restarted.LoadModelReasoningFloorSettings(context.Background()))
	require.Equal(t, s.modelReasoningFloorCache.Load(), restarted.modelReasoningFloorCache.Load())
	for _, rules := range [][]ModelReasoningFloorRule{{{Model: "*", MinEffort: "xhigh"}}, {{Model: "target", MinEffort: "invalid"}}, {{Model: "same", MinEffort: "high"}, {Model: "same", MinEffort: "low"}}, make([]ModelReasoningFloorRule, 65)} {
		require.ErrorIs(t, s.SetModelReasoningFloorSettings(context.Background(), ModelReasoningFloorSettings{Rules: rules}), ErrInvalidModelReasoningFloor)
	}
	repo.setErr = errors.New("database unavailable")
	require.Error(t, s.SetModelReasoningFloorSettings(context.Background(), ModelReasoningFloorSettings{}))
	repo.getErr = errors.New("request path must not query SQL")
	require.Equal(t, "xhigh", gjson.GetBytes(gw.applyModelReasoningFloor(account, "target", body, "reasoning.effort"), "reasoning.effort").String())
	account.Platform = PlatformGrok
	require.Equal(t, body, gw.applyModelReasoningFloor(account, "target", body, "reasoning.effort"))
}

func TestModelReasoningFloorHTTPForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat", "messages"} {
		for _, mode := range []string{"oauth", "passthrough", "apikey", "raw-chat"} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.1","instructions":"Be concise","input":"hello","reasoning":{"effort":"low"},"stream":true}`)
				url := "/v1/responses"
				if endpoint == "chat" {
					url = "/v1/chat/completions"
					body = []byte(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low","stream":true}`)
				}
				if endpoint == "messages" {
					url = "/v1/messages"
					body = []byte(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hello"}],"max_tokens":200,"output_config":{"effort":"low"},"stream":true}`)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "synthetic-account", "api_key": "sk-synthetic", "model_mapping": map[string]any{"gpt-5.1": "gpt-5.6-luna"}}, Extra: map[string]any{}}
				if mode == "passthrough" {
					account.Extra["openai_passthrough"] = true
				}
				if mode == "apikey" || mode == "raw-chat" {
					account.Type = AccountTypeAPIKey
					account.Extra["openai_responses_supported"] = mode != "raw-chat"
				}
				responseBody := `data: {"type":"response.completed","response":{"id":"resp_fixture","model":"gpt-5.6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
				if mode == "raw-chat" {
					responseBody = `data: {"id":"chatcmpl_fixture","model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n" + `data: {"id":"chatcmpl_fixture","model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(responseBody))}}
				expectedModel := "gpt-5.6-luna"
				if endpoint == "responses" && mode == "passthrough" {
					expectedModel = "gpt-5.1" // passthrough deliberately does not apply account aliases
				}
				gw := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, settingService: floorTestSettings(t, expectedModel)}
				ctx := WithRequestedReasoningEffort(context.Background(), "low")
				var result *OpenAIForwardResult
				var err error
				switch endpoint {
				case "responses":
					result, err = gw.Forward(ctx, c, account, body)
				case "chat":
					result, err = gw.ForwardAsChatCompletions(ctx, c, account, body, "", "")
				case "messages":
					result, err = gw.ForwardAsAnthropic(ctx, c, account, body, "", "")
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				path := "reasoning.effort"
				if mode == "raw-chat" {
					path = "reasoning_effort"
				}
				require.Equal(t, expectedModel, gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(upstream.lastBody, path).String(), string(upstream.lastBody))
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, "xhigh", *result.ReasoningEffort)
			})
		}
	}
}
