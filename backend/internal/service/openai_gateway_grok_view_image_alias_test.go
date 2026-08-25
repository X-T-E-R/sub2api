//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPatchGrokResponsesViewImageAliasCanonicalizesDeclarationHistoryAndChoice(t *testing.T) {
	schemas := []struct {
		name       string
		parameters string
	}{
		{name: "valid object", parameters: `{"type":"object","properties":{"path":{"type":"string"},"detail":{"type":"string"}},"required":["path"]}`},
		{name: "union root", parameters: `{"anyOf":[{"type":"object","properties":{"path":{"type":"string"}}},{"type":"null"}]}`},
		{name: "malformed scalar", parameters: `"not-a-schema"`},
		{name: "missing", parameters: ""},
	}
	for _, tt := range schemas {
		t.Run(tt.name, func(t *testing.T) {
			parameters := ""
			if tt.parameters != "" {
				parameters = `,"parameters":` + tt.parameters
			}
			body := []byte(fmt.Sprintf(`{
				"model":"grok-4.6","stream":false,
				"tools":[{"type":"function","name":"view_image","description":"client"%s}],
				"tool_choice":{"type":"function","name":"view_image"},
				"input":[
					{"type":"function_call","id":"fc_old","call_id":"call_old","name":"view_image","arguments":"{\"path\":\"C:/tmp/image\",\"detail\":\"original\",\"extra\":true}"},
					{"type":"function_call_output","call_id":"call_old","output":[{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"}]},
					{"type":"message","role":"user","content":"continue"}
				]
			}`, parameters))

			patched, mapping, err := patchGrokResponsesBodyWithClientToolsOptions(body, "grok-4.6", true, true)
			require.NoError(t, err)
			require.Contains(t, mapping.FunctionAliases, "read_file")
			require.Equal(t, "read_file", gjson.GetBytes(patched, "tools.0.name").String())
			require.Equal(t, "function", gjson.GetBytes(patched, "tools.0.type").String())
			require.Equal(t, "Load one local image file for visual inspection. Use only for image files; source, markup, and other text files are not supported.", gjson.GetBytes(patched, "tools.0.description").String())
			require.Equal(t, "object", gjson.GetBytes(patched, "tools.0.parameters.type").String())
			require.Equal(t, "string", gjson.GetBytes(patched, "tools.0.parameters.properties.target_file.type").String())
			require.Equal(t, "target_file", gjson.GetBytes(patched, "tools.0.parameters.required.0").String())
			require.False(t, gjson.GetBytes(patched, "tools.0.parameters.additionalProperties").Bool())
			require.False(t, gjson.GetBytes(patched, "tools.0.parameters.properties.path").Exists())
			require.False(t, gjson.GetBytes(patched, "tools.0.parameters.properties.detail").Exists())
			for _, unsupported := range []string{"offset", "limit", "pages", "format"} {
				require.False(t, gjson.GetBytes(patched, "tools.0.parameters.properties."+unsupported).Exists())
			}
			require.Equal(t, "read_file", gjson.GetBytes(patched, "tool_choice.name").String())
			require.Equal(t, "read_file", gjson.GetBytes(patched, "input.0.name").String())
			require.JSONEq(t, `{"target_file":"C:/tmp/image"}`, gjson.GetBytes(patched, "input.0.arguments").String())
			require.Equal(t, "call_old", gjson.GetBytes(patched, "input.0.call_id").String())
			require.Equal(t, "function_call_output", gjson.GetBytes(patched, "input.1.type").String())
			require.Equal(t, "input_image", gjson.GetBytes(patched, "input.1.output.0.type").String())
		})
	}
}

func TestPatchGrokResponsesViewImageAliasStripsRedundantInlineImageFirst(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.6","tool_choice":"auto",
		"tools":[
			{"type":"function","name":"view_image","parameters":{"type":"object"}},
			{"type":"function","name":"shell_command","parameters":{"type":"object"}}
		],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]
	}`)
	patched, mapping, err := patchGrokResponsesBodyWithClientToolsOptions(body, "grok-4.6", true, true)
	require.NoError(t, err)
	require.Empty(t, mapping.FunctionAliases)
	require.False(t, gjson.GetBytes(patched, `tools.#(name=="view_image")`).Exists())
	require.False(t, gjson.GetBytes(patched, `tools.#(name=="read_file")`).Exists())
	require.True(t, gjson.GetBytes(patched, `tools.#(name=="shell_command")`).Exists())
}

func TestPatchGrokResponsesViewImageAliasCollisionAndSwitchMatrix(t *testing.T) {
	collisions := []struct {
		name  string
		tools string
	}{
		{name: "real read_file", tools: `[{"type":"function","name":"view_image"},{"type":"function","name":"read_file"}]`},
		{name: "duplicate view_image", tools: `[{"type":"function","name":"view_image"},{"type":"function","name":"view_image"}]`},
		{name: "non function view_image", tools: `[{"type":"function","name":"view_image"},{"type":"mcp","name":"view_image","server_url":"https://example.test"}]`},
		{name: "custom lowered view_image", tools: `[{"type":"custom","name":"view_image","format":{"type":"text"}}]`},
		{name: "custom lowered read_file", tools: `[{"type":"function","name":"view_image"},{"type":"custom","name":"read_file","format":{"type":"text"}}]`},
	}
	for _, tt := range collisions {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"grok-4.6","input":"inspect","tools":` + tt.tools + `}`)
			patched, mapping, err := patchGrokResponsesBodyWithClientToolsOptions(body, "grok-4.6", true, true)
			require.NoError(t, err)
			require.Empty(t, mapping.FunctionAliases)
			require.False(t, gjson.GetBytes(patched, `tools.#(name=="read_file").description`).String() == grokReadFileImageAliasDeclaration["description"])
		})
	}

	switches := []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{name: "missing defaults enabled", want: true},
		{name: "invalid defaults enabled", extra: map[string]any{grokViewImageReadFileBridgeExtraKey: "false"}, want: true},
		{name: "explicit true", extra: map[string]any{grokViewImageReadFileBridgeExtraKey: true}, want: true},
		{name: "explicit false", extra: map[string]any{grokViewImageReadFileBridgeExtraKey: false}, want: false},
		{name: "umbrella false", extra: map[string]any{grokResponsesProtocolCompatExtraKey: false, grokViewImageReadFileBridgeExtraKey: true}, want: false},
	}
	for _, tt := range switches {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{Extra: tt.extra}
			require.Equal(t, tt.want, grokViewImageReadFileBridgeEnabled(account))
			body := []byte(`{"model":"grok-4.6","input":"inspect","tools":[{"type":"function","name":"view_image"}]}`)
			patched, mapping, err := patchGrokResponsesBodyWithClientToolsOptions(
				body, "grok-4.6", grokResponsesProtocolCompatEnabled(account), grokViewImageReadFileBridgeEnabled(account),
			)
			require.NoError(t, err)
			require.Equal(t, tt.want, gjson.GetBytes(patched, `tools.#(name=="read_file")`).Exists())
			require.Equal(t, tt.want, len(mapping.FunctionAliases) == 1)
		})
	}
}

func TestPrepareGrokWSViewImageAliasInheritanceAndClear(t *testing.T) {
	account := healthyGrokOAuthGatewayTestAccount(9312, "access-token")
	seed, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.6","input":"start","tools":[{"type":"function","name":"view_image","description":"client","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`),
		account, "grok-4.6", apicompat.ResponsesClientToolMapping{}, nil, true,
	)
	require.NoError(t, err)
	require.Contains(t, seed.Mapping.FunctionAliases, "read_file")
	require.Equal(t, "read_file", gjson.GetBytes(seed.LoweredTools, "0.name").String())

	inherited, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.6","input":[{"type":"function_call","id":"fc_ws","call_id":"call_ws","name":"view_image","arguments":"{\"path\":\"C:/tmp/ws.png\",\"detail\":\"high\"}"}]}`),
		account, "grok-4.6", seed.Mapping, decodeOpenAIWSHTTPBridgeLoweredTools(seed.LoweredTools), true,
	)
	require.NoError(t, err)
	require.Contains(t, inherited.Mapping.FunctionAliases, "read_file")
	require.Equal(t, "read_file", gjson.GetBytes(inherited.Body, "input.0.name").String())
	require.JSONEq(t, `{"target_file":"C:/tmp/ws.png"}`, gjson.GetBytes(inherited.Body, "input.0.arguments").String())
	require.Equal(t, "read_file", gjson.GetBytes(inherited.LoweredTools, "0.name").String())

	cleared, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.6","input":"clear","tools":[]}`),
		account, "grok-4.6", inherited.Mapping, decodeOpenAIWSHTTPBridgeLoweredTools(inherited.LoweredTools), true,
	)
	require.NoError(t, err)
	require.Empty(t, cleared.Mapping.FunctionAliases)
	require.Equal(t, "[]", strings.TrimSpace(string(cleared.LoweredTools)))
}

func TestGrokMessagesAndChatResponsesRoutesRestoreViewImageAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := healthyGrokOAuthGatewayTestAccount(9320, "access-token")
	account.Credentials["subscription_tier"] = "supergrok"
	repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
		accountsByID: map[int64]*Account{account.ID: account},
	}}

	t.Run("messages buffered", func(t *testing.T) {
		body := []byte(`{
			"model":"grok-4.6","max_tokens":32,"stream":false,
			"messages":[{"role":"user","content":"Inspect the local image"}],
			"tools":[{"name":"view_image","description":"inspect","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]
		}`)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
		c.Set("api_key", &APIKey{ID: 93201})
		upstream := &httpUpstreamRecorder{resp: grokViewImageAliasCompletedSSE("resp_messages_alias")}
		svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}

		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "read_file", gjson.GetBytes(upstream.lastBody, `tools.#(name=="read_file").name`).String())
		require.Equal(t, "tool_use", gjson.GetBytes(recorder.Body.Bytes(), "content.0.type").String())
		require.Equal(t, "view_image", gjson.GetBytes(recorder.Body.Bytes(), "content.0.name").String())
		require.Equal(t, "C:/tmp/provider.png", gjson.GetBytes(recorder.Body.Bytes(), "content.0.input.path").String())
	})

	t.Run("chat buffered", func(t *testing.T) {
		body := []byte(`{
			"model":"grok-4.6","stream":false,
			"messages":[{"role":"user","content":"Inspect the local image"}],
			"tools":[{"type":"function","function":{"name":"view_image","description":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]
		}`)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
		c.Set("api_key", &APIKey{ID: 93202})
		upstream := &httpUpstreamRecorder{resp: grokViewImageAliasCompletedSSE("resp_chat_alias")}
		svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}

		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "session-alias", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, grokChatResponsesEndpoint, result.UpstreamEndpoint)
		require.Equal(t, "read_file", gjson.GetBytes(upstream.lastBody, `tools.#(name=="read_file").name`).String())
		require.Equal(t, "view_image", gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.tool_calls.0.function.name").String())
		require.JSONEq(t, `{"path":"C:/tmp/provider.png"}`, gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.tool_calls.0.function.arguments").String())
	})
}

func TestForwardGrokResponsesRestoresViewImageAliasStreamingAndNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := func(stream bool) []byte {
		return []byte(fmt.Sprintf(`{
			"model":"grok-4.6","stream":%t,"input":"Inspect the local image",
			"tools":[{"type":"function","name":"view_image","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]
		}`, stream))
	}

	t.Run("non streaming JSON", func(t *testing.T) {
		body := request(false)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		c.Set("api_key", &APIKey{ID: 93301})
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_native_alias","object":"response","model":"grok-4.6","status":"completed",
				"output":[{"type":"function_call","id":"fc_native","call_id":"call_native","name":"read_file","arguments":"{\"target_file\":\"C:/tmp/native.png\"}"}],
				"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}
			}`)),
		}}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		account := grokProtocolAPIKeyAccount(9330)

		result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok-4.6", false, time.Now())
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, "read_file", gjson.GetBytes(upstream.lastBody, `tools.#(name=="read_file").name`).String())
		require.Equal(t, "view_image", gjson.GetBytes(recorder.Body.Bytes(), "output.0.name").String())
		require.Equal(t, "fc_native", gjson.GetBytes(recorder.Body.Bytes(), "output.0.id").String())
		require.Equal(t, "call_native", gjson.GetBytes(recorder.Body.Bytes(), "output.0.call_id").String())
		require.JSONEq(t, `{"path":"C:/tmp/native.png"}`, gjson.GetBytes(recorder.Body.Bytes(), "output.0.arguments").String())
	})

	t.Run("streaming terminal", func(t *testing.T) {
		body := request(true)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		c.Set("api_key", &APIKey{ID: 93302})
		upstream := &httpUpstreamRecorder{resp: grokViewImageAliasCompletedSSE("resp_native_stream_alias")}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		account := grokProtocolAPIKeyAccount(9331)

		result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok-4.6", true, time.Now())
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Contains(t, recorder.Body.String(), `"name":"view_image"`)
		require.Contains(t, recorder.Body.String(), `\"path\":\"C:/tmp/provider.png\"`)
		require.NotContains(t, recorder.Body.String(), `"name":"read_file"`)
	})
}

func TestGrokViewImageAliasParticipatesInFinalBodyCacheIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{
		"model":"grok-4.6","input":[{"type":"message","role":"user","content":"Inspect a local image"}],
		"tools":[{"type":"function","name":"view_image","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]
	}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Set("api_key", &APIKey{ID: 93401})
	hint := captureGrokCacheSeedHint(c, body, "")

	aliased, _, err := patchGrokResponsesBodyWithClientToolsOptions(body, "grok-4.6", true, true)
	require.NoError(t, err)
	passthrough, _, err := patchGrokResponsesBodyWithClientToolsOptions(body, "grok-4.6", true, false)
	require.NoError(t, err)
	aliasedIdentity := resolveGrokCacheIdentityFromFinal(c, aliased, hint, "grok-4.6")
	passthroughIdentity := resolveGrokCacheIdentityFromFinal(c, passthrough, hint, "grok-4.6")
	require.NotEmpty(t, aliasedIdentity)
	require.NotEmpty(t, passthroughIdentity)
	require.NotEqual(t, passthroughIdentity, aliasedIdentity)
	require.Equal(t, "read_file", gjson.GetBytes(aliased, `tools.#(name=="read_file").name`).String())
}

func grokViewImageAliasCompletedSSE(responseID string) *http.Response {
	body := strings.Join([]string{
		`data: {"type":"response.completed","sequence_number":0,"response":{"id":"` + responseID + `","object":"response","model":"grok-4.6","status":"completed","output":[{"type":"function_call","id":"fc_alias","call_id":"call_alias","name":"read_file","arguments":"{\"target_file\":\"C:/tmp/provider.png\"}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
