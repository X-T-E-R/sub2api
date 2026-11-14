package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestStripThoughtSignaturesPreservesHistory(t *testing.T) {
	req := &antigravity.ClaudeRequest{
		Thinking: &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
		Messages: []antigravity.ClaudeMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"thinking","thinking":"  plan  ","signature":"foreign"},
				{"type":"thinking","thinking":" \n\t","signature":"carrier"},
				{"type":"text","text":"answer","signature":"foreign-text","cache_control":{"type":"ephemeral"}},
				{"type":"tool_use","id":"t1","name":"lookup","input":{"n":9007199254740993},"signature":"foreign-tool"}
			]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)},
		},
	}
	thinking := req.Thinking
	changed, err := stripThoughtSignaturesFromClaudeRequest(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Same(t, thinking, req.Thinking)
	require.JSONEq(t, `[
		{"type":"thinking","thinking":"  plan  "},
		{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}},
		{"type":"tool_use","id":"t1","name":"lookup","input":{"n":9007199254740993}}
	]`, string(req.Messages[1].Content))
	require.Contains(t, string(req.Messages[1].Content), "9007199254740993")
	require.Equal(t, `"hello"`, string(req.Messages[0].Content))
	require.Equal(t, `[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`, string(req.Messages[2].Content))
	changed, err = stripThoughtSignaturesFromClaudeRequest(req)
	require.NoError(t, err)
	require.False(t, changed, "sanitization must be idempotent")
}

func TestStripThoughtSignaturesEmptyCarriers(t *testing.T) {
	for _, content := range []string{
		`[{"type":"thinking","thinking":"","signature":"sig"}]`,
		`[{"type":"thinking","signature":""},{"type":"thinking","thinking":"  ","signature":null}]`,
	} {
		req := &antigravity.ClaudeRequest{Messages: []antigravity.ClaudeMessage{{Role: "assistant", Content: json.RawMessage(content)}}}
		changed, err := stripThoughtSignaturesFromClaudeRequest(req)
		require.NoError(t, err)
		require.True(t, changed)
		require.JSONEq(t, `[{"type":"thinking","thinking":""}]`, string(req.Messages[0].Content))
	}
	req := &antigravity.ClaudeRequest{Messages: []antigravity.ClaudeMessage{{Content: json.RawMessage(`[{"type":"thinking","thinking":""},{"type":"redacted_thinking","data":"opaque"}]`)}}}
	changed, err := stripThoughtSignaturesFromClaudeRequest(req)
	require.NoError(t, err)
	require.False(t, changed, "unsigned blocks are not signature carriers")
}

func TestAntigravityForwardCrossAccountSignatures(t *testing.T) {
	for _, tc := range []struct {
		name, model                              string
		switched, reactive, wantStripped, search bool
	}{
		{name: "gemini switched proactively", model: "gemini-3.8-flash", switched: true, wantStripped: true},
		{name: "gemini stable retains signatures", model: "gemini-3.8-flash"},
		{name: "claude switched retains signatures", model: "claude-opus-4-6-thinking", switched: true},
		{name: "gemini stage zero retains thought and tools", model: "gemini-3.8-flash", reactive: true, wantStripped: true},
		{name: "claude starts at thinking-only", model: "claude-opus-4-6-thinking", reactive: true},
		{name: "claude search uses Gemini proactively", model: "claude-opus-4-6-thinking", switched: true, search: true, wantStripped: true},
		{name: "claude search uses Gemini stage zero", model: "claude-opus-4-6-thinking", reactive: true, search: true, wantStripped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := []byte(`{"model":"` + tc.model + `","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[
				{"role":"user","content":"hello"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"foreign-thought"},{"type":"thinking","signature":"empty-carrier"},{"type":"text","text":"visible","signature":"foreign-text"},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"},"signature":"foreign-tool"}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
			]}`)
			if tc.search {
				var err error
				body, err = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"web_search_20250305","name":"web_search"}]`))
				require.NoError(t, err)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			response := func(status int, payload string) *http.Response {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(bytes.NewBufferString(payload))}
			}
			upstream := &queuedHTTPUpstreamStub{}
			if tc.reactive {
				upstream.responses = append(upstream.responses, response(400, `{"error":{"message":"Corrupted thought signature"}}`))
			}
			upstream.responses = append(upstream.responses, response(200, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3}}}\n\n"))
			svc := &AntigravityGatewayService{
				settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}),
				tokenProvider:  &AntigravityTokenProvider{}, httpUpstream: upstream,
			}
			account := &Account{ID: 73, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Concurrency: 8,
				Credentials: map[string]any{"access_token": "test-token", "project_id": "proj", "model_mapping": map[string]any{tc.model: tc.model}}}
			ctx := WithAntigravityAccountSwitched(context.Background(), tc.switched)
			result, err := svc.Forward(ctx, c, account, body, false)
			require.NoError(t, err)
			require.NotNil(t, result)
			wantCalls := 1
			if tc.reactive {
				wantCalls = 2
			}
			require.Len(t, upstream.requestBodies, wantCalls)
			wire := upstream.requestBodies[wantCalls-1]
			if tc.search {
				require.Equal(t, antigravity.DefaultWebSearchFallbackModel, gjson.GetBytes(wire, "model").String())
			}
			if tc.wantStripped {
				require.NotContains(t, string(wire), "foreign-")
				require.NotContains(t, string(wire), "empty-carrier")
				require.Contains(t, string(wire), antigravity.DummyThoughtSignature)
				require.True(t, gjson.GetBytes(wire, "request.generationConfig.thinkingConfig.includeThoughts").Bool())
				var thought, tool, result bool
				for _, content := range gjson.GetBytes(wire, "request.contents").Array() {
					for _, part := range content.Get("parts").Array() {
						if part.Get("text").String() == "plan" {
							thought = part.Get("thought").Bool()
							require.Equal(t, antigravity.DummyThoughtSignature, part.Get("thoughtSignature").String())
						}
						tool = tool || part.Get("functionCall.name").String() == "lookup"
						result = result || part.Get("functionResponse.name").String() == "lookup"
					}
				}
				require.True(t, thought)
				require.True(t, tool)
				require.True(t, result)
			} else if tc.reactive {
				require.NotContains(t, string(wire), "foreign-thought")
				require.Contains(t, string(wire), "foreign-tool")
				require.False(t, gjson.GetBytes(wire, "request.generationConfig.thinkingConfig.includeThoughts").Bool())
			} else {
				require.Contains(t, string(wire), "foreign-thought")
				require.Contains(t, string(wire), "foreign-tool")
				require.Contains(t, string(wire), "foreign-text")
			}
		})
	}
}

func TestAntigravityForwardSwitchedThinkingTail(t *testing.T) {
	for _, hasThought := range []bool{false, true} {
		name := "reject carrier-only tail"
		if hasThought {
			name = "preserve split thinking tail"
		}
		t.Run(name, func(t *testing.T) {
			messages := []antigravity.ClaudeMessage{{Role: "user", Content: json.RawMessage(`"question"`)}}
			if hasThought {
				messages = append(messages, antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"foreign-thought"}]`)})
			}
			messages = append(messages, antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","signature":"foreign-carrier"}]`)})
			body, err := json.Marshal(antigravity.ClaudeRequest{Model: "gemini-3.8-flash", MaxTokens: 4096, Thinking: &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}, Messages: messages})
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))}}}
			svc := &AntigravityGatewayService{settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}), tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream}
			account := &Account{ID: 73, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Concurrency: 8,
				Credentials: map[string]any{"access_token": "test-token", "project_id": "proj", "model_mapping": map[string]any{"gemini-3.8-flash": "gemini-3.8-flash"}},
				Extra:       map[string]any{AntigravityContinueThoughtOnlyExtraKey: true}}
			result, err := svc.Forward(WithAntigravityAccountSwitched(context.Background(), true), c, account, body, false)
			if !hasThought {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.requestBodies)
				return
			}
			require.NoError(t, err)
			require.Len(t, upstream.requestBodies, 1)
			contents := gjson.GetBytes(upstream.requestBodies[0], "request.contents").Array()
			require.Len(t, contents, 2)
			require.Equal(t, "model", contents[1].Get("role").String())
			require.Equal(t, "plan", contents[1].Get("parts.0.text").String())
			require.True(t, contents[1].Get("parts.0.thought").Bool())
			require.NotContains(t, string(upstream.requestBodies[0]), "foreign-")
			require.NotContains(t, string(upstream.requestBodies[0]), "(content removed)")
		})
	}
}
