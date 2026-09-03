package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func antigravityMessagesMappedTestAccount(model string) *Account {
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	account.Credentials["model_mapping"] = map[string]any{"client-alias": model}
	return account
}

func TestAntigravityMessagesCompatibilityForwardScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		model   string
		policy  config.GatewayAntigravityGeminiMessagesConfig
		adapted bool
	}{
		{name: "default enabled", model: "gemini-3.8-flash", adapted: true},
		{name: "disabled", model: "gemini-3.8-flash", policy: config.GatewayAntigravityGeminiMessagesConfig{Disabled: true}},
		{name: "allowlisted", model: "gemini-3.8-flash", policy: config.GatewayAntigravityGeminiMessagesConfig{Models: []string{"gemini-3.8-flash"}}, adapted: true},
		{name: "model opted out", model: "gemini-3.8-flash", policy: config.GatewayAntigravityGeminiMessagesConfig{Models: []string{"gemini-2.5-flash"}}},
		{name: "Claude unchanged", model: "claude-sonnet-4-5"},
		{name: "Grok unchanged", model: "grok-4"},
		{name: "OpenAI unchanged", model: "gpt-5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize, AntigravityGeminiMessages: tt.policy,
			}, upstream)
			body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix:"}]}`)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			result, err := svc.Forward(context.Background(), c, antigravityMessagesMappedTestAccount(tt.model), body, false)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, tt.model, result.UpstreamModel)
			require.Equal(t, "client-alias", result.Model)
			require.Equal(t, 1, upstream.callCount)
			var request antigravity.V1InternalRequest
			require.NoError(t, json.Unmarshal(upstream.requestBodies[0], &request))
			require.Equal(t, tt.model, request.Model)
			require.Equal(t, "question", request.Request.Contents[0].Parts[0].Text)
			require.Equal(t, "model", request.Request.Contents[1].Role)
			require.Equal(t, "prefix:", request.Request.Contents[1].Parts[0].Text)
			if tt.adapted {
				require.Len(t, request.Request.Contents, 3)
				require.Equal(t, "user", request.Request.Contents[2].Role)
				require.Contains(t, request.Request.Contents[2].Parts[0].Text, "[sub2api:assistant-prefill]")
			} else {
				require.Len(t, request.Request.Contents, 2)
			}
		})
	}
}

func TestAntigravityMessagesCompatibilityLocalErrorsDoNotForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, content := range []string{
		`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]}`,
		`{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://private.example/secret"}}]}`,
		`{"role":"user","content":[{"type":"text","text":"keep me"},{"type":"document","source":{"type":"text","data":"secret-document"}}]}`,
	} {
		upstream := &queuedHTTPUpstreamStub{}
		svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
		body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"},` + content + `]}`)
		c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
		result, err := svc.Forward(context.Background(), c, antigravityMessagesMappedTestAccount("gemini-3.8-flash"), body, false)
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Equal(t, "invalid_request_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
		require.Contains(t, gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(), "messages[1]")
		require.NotContains(t, recorder.Body.String(), "secret")
		require.Equal(t, 0, upstream.callCount)
	}
}

func TestAntigravityMessagesCompatibilityIsMessagesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "chat", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			account := antigravityMessagesMappedTestAccount("gemini-3.8-flash")
			var body []byte
			switch protocol {
			case "responses":
				body = []byte(`{"model":"client-alias","input":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix:"}]}`)
			case "chat":
				body = []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix:"}]}`)
			case "gemini":
				body = []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]},{"role":"model","parts":[{"text":"prefix:"}]}]}`)
			}
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/mock", body)
			var result *ForwardResult
			var err error
			switch protocol {
			case "responses":
				result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
			case "chat":
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			case "gemini":
				result, err = svc.ForwardGemini(context.Background(), c, account, "client-alias", "generateContent", false, body, false)
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, 1, upstream.callCount)
			contents := gjson.GetBytes(upstream.requestBodies[0], "request.contents").Array()
			require.Len(t, contents, 2, "Messages policy must not change another route's finalizer")
			require.Equal(t, "user", contents[0].Get("role").String())
			require.Equal(t, "model", contents[1].Get("role").String())
			require.Equal(t, "prefix:", contents[1].Get("parts.0.text").String())
			require.NotContains(t, string(upstream.requestBodies[0]), "[sub2api:")
		})
	}
}

func TestAntigravityMessagesCompatibilityDoesNotLeakThroughWebSearchFallback(t *testing.T) {
	upstream := &queuedHTTPUpstreamStub{}
	svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix:"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	var request antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal(body, &request))
	converted, err := svc.buildAntigravityCompatGeminiBody(context.Background(), body, &request, "mock-project", "claude-sonnet-4-5")
	require.NoError(t, err)
	require.Equal(t, "gemini-2.5-flash", gjson.GetBytes(converted, "model").String())
	require.Equal(t, int64(2), gjson.GetBytes(converted, "request.contents.#").Int())
	require.Equal(t, "model", gjson.GetBytes(converted, "request.contents.1.role").String())
}

func TestAntigravityMessagesCompatibilityRetainsGeminiRateLimitSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, sticky := range []bool{false, true} {
		upstream := &queuedHTTPUpstreamStub{}
		svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
		account := antigravityMessagesMappedTestAccount("gemini-3.8-flash")
		account.Extra = map[string]any{modelRateLimitsKey: map[string]any{
			"gemini-3.8-flash": map[string]any{"rate_limit_reset_at": time.Now().Add(30 * time.Second).Format(time.RFC3339)},
		}}
		body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix:"}]}`)
		c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
		result, err := svc.Forward(context.Background(), c, account, body, sticky)
		require.Nil(t, result)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr)
		require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
		require.Equal(t, sticky, failoverErr.ForceCacheBilling)
		require.Zero(t, upstream.callCount)
	}
}

func TestAntigravityMessagesCompatibilityImageToolForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name, model string
		disabled    bool
		wantStatus  int
		wantImage   bool
	}{
		{name: "Gemini3 image", model: "gemini-3.8-flash", wantStatus: 200, wantImage: true},
		{name: "older Gemini capability error", model: "gemini-2.5-flash", wantStatus: 400},
		{name: "disabled retains legacy", model: "gemini-3.8-flash", disabled: true, wantStatus: 200},
		{name: "Claude retains legacy", model: "claude-sonnet-4-5", wantStatus: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize, AntigravityGeminiMessages: config.GatewayAntigravityGeminiMessagesConfig{Disabled: tt.disabled}}, upstream)
			body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"capture"},{"role":"assistant","content":[{"type":"tool_use","id":"capture-1","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"capture-1","content":[{"type":"text","text":"image output"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}]}`)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			_, err := svc.Forward(context.Background(), c, antigravityMessagesMappedTestAccount(tt.model), body, false)
			require.Equal(t, tt.wantStatus, recorder.Code)
			if tt.wantStatus == 400 {
				require.ErrorContains(t, err, "Gemini 3+ final model")
				require.Zero(t, upstream.callCount)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, upstream.callCount)
			result := gjson.GetBytes(upstream.requestBodies[0], "request.contents.2.parts.0.functionResponse")
			require.Equal(t, "capture-1", result.Get("id").String())
			require.Equal(t, "capture", result.Get("name").String())
			require.Equal(t, "image output", result.Get("response.result").String())
			if tt.wantImage {
				require.Equal(t, "aW1hZ2U=", result.Get("parts.0.inlineData.data").String())
			} else {
				require.False(t, result.Get("parts").Exists())
			}
		})
	}
}
