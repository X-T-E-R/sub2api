//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type grokSchemaMatrixFixtures struct {
	Model string `json:"model"`
	Cases []struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	} `json:"cases"`
}

func TestGrokResponsesSharedSchemaStageElevenCaseMatrix(t *testing.T) {
	fixtures := loadGrokSchemaMatrixFixtures(t)
	fallbackReasons := map[string]string{
		"root_anyof_object_string": "mixed_non_object_root",
		"cyclic_local_ref":         "cyclic_ref",
		"missing_local_ref":        "missing_ref",
		"external_ref":             "external_ref_unsupported",
		"false_schema":             "false_schema",
		"malformed_combinator":     "malformed_combinator",
		"malformed_ref_value":      "malformed_ref",
		"json_string_schema":       "expected_schema_object",
	}

	for _, schemaCase := range fixtures.Cases {
		t.Run(schemaCase.Name, func(t *testing.T) {
			body := []byte(`{"model":"grok","input":[],"tools":[{"type":"function","name":"matrix_tool","description":"outer-kept","strict":true,"parameters":` + string(schemaCase.Schema) + `,"x_outer":{"kept":true}}]}`)
			original := append([]byte(nil), body...)
			final, report, err := sanitizeGrokResponsesTools(body, true)
			require.NoError(t, err)
			require.Equal(t, original, body)
			require.Len(t, report.Schemas, 1)
			assertGrokSchemaMatrixFinalTool(t, final, schemaCase.Schema, fallbackReasons[schemaCase.Name])
			require.Equal(t, "outer-kept", gjson.GetBytes(final, "tools.0.description").String())
			require.True(t, gjson.GetBytes(final, "tools.0.x_outer.kept").Bool())

			second, secondReport, err := sanitizeGrokResponsesTools(final, true)
			require.NoError(t, err)
			require.Equal(t, final, second)
			require.NotEqual(t, "fallback", compatibilitySingleSchemaOutcome(secondReport))
			require.Equal(t, final, buildGrokCompatibilityRequest(t, final))
		})
	}
}

func TestGrokResponsesSchemaStageMissingNullNameAndCacheEquivalence(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"alpha","description":"missing","x":1},{"type":"function","name":"beta","description":"null","parameters":null,"strict":true,"x":2},{"type":"function","name":"codex_app__automation_update","parameters":{"$ref":"#/$defs/missing","$defs":{}},"strict":true},{"type":"function","name":"ordinary","parameters":{"$ref":"#/$defs/missing","$defs":{}},"strict":true}]}`)
	final, report, err := sanitizeGrokResponsesTools(body, true)
	require.NoError(t, err)
	require.Equal(t, 4, report.SchemaFallback)
	require.Equal(t, "missing_parameters", report.Schemas[0].Reason)
	require.Equal(t, grokMissingParametersFingerprint(), report.Schemas[0].Fingerprint)
	require.Equal(t, "null_parameters", report.Schemas[1].Reason)
	require.Equal(t, report.Schemas[2].Reason, report.Schemas[3].Reason)
	require.Equal(t, report.Schemas[2].Fingerprint, report.Schemas[3].Fingerprint)
	for index := 0; index < 4; index++ {
		require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(final, fmt.Sprintf("tools.%d.parameters", index)).Raw)
		require.False(t, gjson.GetBytes(final, fmt.Sprintf("tools.%d.strict", index)).Bool())
	}
	require.Equal(t, "missing", gjson.GetBytes(final, "tools.0.description").String())
	require.Equal(t, int64(2), gjson.GetBytes(final, "tools.1.x").Int())

	firstRaw := []byte(`{"model":"grok-4.5","input":[],"tools":[{"type":"function","name":"same","parameters":false,"strict":true}]}`)
	secondRaw := []byte(`{"model":"grok-4.5","input":[],"tools":[{"type":"function","name":"same","parameters":{"$ref":"#/$defs/missing","$defs":{}},"strict":true}]}`)
	first, _, err := patchGrokResponsesBodyWithCompatibility(firstRaw, "grok-4.5", true)
	require.NoError(t, err)
	second, _, err := patchGrokResponsesBodyWithCompatibility(secondRaw, "grok-4.5", true)
	require.NoError(t, err)
	require.JSONEq(t, string(first), string(second))
	cacheRecorder := httptest.NewRecorder()
	cacheContext, _ := gin.CreateTestContext(cacheRecorder)
	cacheContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	cacheContext.Set("api_key", &APIKey{ID: 8801})
	identityFirst := resolveGrokCacheIdentity(cacheContext, first, "", "grok-4.5")
	identitySecond := resolveGrokCacheIdentity(cacheContext, second, "", "grok-4.5")
	require.NotEmpty(t, identityFirst)
	require.Equal(t, identityFirst, identitySecond)
}

func TestGrokResponsesSchemaFallbackAlwaysWritesBooleanStrictFalse(t *testing.T) {
	tests := []struct {
		name           string
		strictFragment string
	}{
		{name: "missing"},
		{name: "true", strictFragment: `,"strict":true`},
		{name: "false", strictFragment: `,"strict":false`},
		{name: "null", strictFragment: `,"strict":null`},
		{name: "string", strictFragment: `,"strict":"false"`},
		{name: "object", strictFragment: `,"strict":{"value":false}`},
		{name: "array", strictFragment: `,"strict":[false]`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"tools":[{"type":"function","name":"strict_case","parameters":false` + testCase.strictFragment + `}]}`)
			final, report, err := sanitizeGrokResponsesTools(body, true)
			require.NoError(t, err)
			require.Equal(t, 1, report.SchemaFallback)
			strict := gjson.GetBytes(final, "tools.0.strict")
			require.True(t, strict.Exists())
			require.Equal(t, gjson.False, strict.Type)
			require.Equal(t, "false", strict.Raw)
			require.False(t, report.Schemas[0].StrictAfter)
			require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(final, "tools.0.parameters").Raw)
		})
	}

	for _, strict := range []string{"true", "false"} {
		body := []byte(`{"tools":[{"type":"function","name":"preserved","parameters":{"type":"object","properties":{}},"strict":` + strict + `}]}`)
		final, report, err := sanitizeGrokResponsesTools(body, true)
		require.NoError(t, err)
		require.Equal(t, 1, report.SchemaUnchanged)
		require.Equal(t, strict, gjson.GetBytes(final, "tools.0.strict").Raw)
	}
}

func TestGrokResponsesSchemaStageRouteMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixtures := loadGrokSchemaMatrixFixtures(t)
	fallbackCases := map[string]bool{
		"root_anyof_object_string": true, "cyclic_local_ref": true, "missing_local_ref": true,
		"external_ref": true, "false_schema": true, "malformed_combinator": true,
		"malformed_ref_value": true, "json_string_schema": true,
	}

	for index, schemaCase := range fixtures.Cases {
		t.Run(schemaCase.Name, func(t *testing.T) {
			// Native Responses construction and final request-body recorder.
			native := []byte(`{"model":"grok","input":[],"tools":[{"type":"function","name":"matrix_tool","description":"native","strict":true,"parameters":` + string(schemaCase.Schema) + `}]}`)
			nativeFinal, _, nativeReport, err := patchGrokResponsesBodyWithClientToolsCompatibility(native, fixtures.Model, true)
			require.NoError(t, err)
			nativeFinal, agentReport, err := normalizeGrokResponsesProtocolCompatibility(nativeFinal)
			require.NoError(t, err)
			require.Zero(t, agentReport.AgentItems)
			require.Equal(t, fallbackCases[schemaCase.Name], nativeReport.SchemaFallback == 1)
			assertGrokSchemaMatrixRoot(t, grokSchemaToolByName(t, nativeFinal, "matrix_tool").Get("parameters"))
			require.Equal(t, nativeFinal, buildGrokCompatibilityRequest(t, nativeFinal))

			// Anthropic Messages -> Responses uses the same base/schema owner.
			messagesReq := &apicompat.AnthropicRequest{
				Model: fixtures.Model, MaxTokens: 256,
				Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"probe"`)}},
				Tools:    []apicompat.AnthropicTool{{Name: "matrix_tool", Description: "kiki-control", InputSchema: schemaCase.Schema}},
			}
			responsesReq, err := apicompat.AnthropicToResponses(messagesReq)
			require.NoError(t, err)
			messagesBody, err := json.Marshal(responsesReq)
			require.NoError(t, err)
			messagesFinal, _, err := prepareGrokMessagesResponsesBody(nil, messagesBody, "", fixtures.Model, &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey})
			require.NoError(t, err)
			messagesTool := grokSchemaToolByName(t, messagesFinal, "matrix_tool")
			assertGrokSchemaMatrixRoot(t, messagesTool.Get("parameters"))
			require.Equal(t, "kiki-control", messagesTool.Get("description").String())
			if schemaCase.Name == "kiki_like_pure_object_control" {
				require.JSONEq(t, string(schemaCase.Schema), messagesTool.Get("parameters").Raw)
			}
			require.Equal(t, messagesFinal, buildGrokCompatibilityRequest(t, messagesFinal))

			// Actual OAuth Chat admission and selector must reach Responses for all schemas.
			chatBody := []byte(`{"model":"` + fixtures.Model + `","messages":[{"role":"user","content":"probe"}],"stream":false,"prompt_cache_key":"matrix-` + fmt.Sprint(index) + `","tools":[{"type":"function","function":{"name":"matrix_tool","description":"chat","parameters":` + string(schemaCase.Schema) + `,"strict":true}}]}`)
			eligible, reason := grokChatResponsesBridgeEligibility(chatBody)
			require.True(t, eligible, reason)
			chatRecorder := httptest.NewRecorder()
			chatContext, _ := gin.CreateTestContext(chatRecorder)
			chatContext.Request = httptest.NewRequest(http.MethodPost, grokChatRawEndpoint, bytes.NewReader(chatBody))
			chatContext.Set("api_key", &APIKey{ID: int64(8900 + index)})
			chatAccount := grokChatBridgeTestAccount(int64(8300 + index))
			chatRepo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{chatAccount.ID: chatAccount}}}
			chatUpstream := &httpUpstreamRecorder{resp: grokChatBridgeCompletedResponse("resp_matrix_chat_"+fmt.Sprint(index), 128)}
			chatService := &OpenAIGatewayService{httpUpstream: chatUpstream, grokTokenProvider: NewGrokTokenProvider(chatRepo, nil), accountRepo: chatRepo}
			chatResult, err := chatService.ForwardAsChatCompletions(context.Background(), chatContext, chatAccount, chatBody, "", "")
			require.NoError(t, err)
			require.Equal(t, grokChatResponsesEndpoint, chatResult.UpstreamEndpoint)
			assertGrokSchemaMatrixRoot(t, grokSchemaToolByName(t, chatUpstream.lastBody, "matrix_tool").Get("parameters"))

			// Actual WS effective body uses shared schema stage and reaches the HTTP builder.
			wsPayload := []byte(`{"type":"response.create","generate":true,"model":"grok","stream":true,"input":[],"tools":[{"type":"function","name":"matrix_tool","description":"ws","strict":true,"parameters":` + string(schemaCase.Schema) + `}]}`)
			wsRecorder := httptest.NewRecorder()
			wsContext, _ := gin.CreateTestContext(wsRecorder)
			wsContext.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			wsContext.Set("api_key", &APIKey{ID: int64(8950 + index)})
			wsUpstream := &httpUpstreamRecorder{resp: grokCompatibilityStreamingResponse("resp_matrix_ws_" + fmt.Sprint(index))}
			wsService := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: wsUpstream}
			wsAccount := &Account{ID: int64(8400 + index), Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"base_url": "https://grok.example.test/v1", "api_key": "test-key"}}
			firstIdentity, err := resolveGrokWSCacheIdentity(wsContext, wsAccount, wsPayload, wsPayload, "grok")
			require.NoError(t, err)
			require.NotEmpty(t, firstIdentity)
			stableIdentity, err := resolveGrokWSCacheIdentity(wsContext, wsAccount, wsPayload, wsPayload, "grok")
			require.NoError(t, err)
			require.Equal(t, firstIdentity, stableIdentity)
			wsResult, err := wsService.proxyOpenAIWSHTTPBridgeTurn(context.Background(), wsContext, wsAccount, "token", wsPayload, len(wsPayload), "grok", "", "", "", "matrix-ws", 1, func([]byte) error { return nil })
			require.NoError(t, err)
			require.NotNil(t, wsResult)
			assertGrokSchemaMatrixRoot(t, grokSchemaToolByName(t, wsUpstream.lastBody, "matrix_tool").Get("parameters"))
		})
	}
}

func TestGrokChatResponsesBridgeDerivesProviderIdentityAfterSchemaFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bodies := [][]byte{
		[]byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"probe"}],"stream":false,"tools":[{"type":"function","function":{"name":"same_tool","description":"same","parameters":false,"strict":true}}]}`),
		[]byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"probe"}],"stream":false,"tools":[{"type":"function","function":{"name":"same_tool","description":"same","parameters":{"$ref":"#/$defs/missing","$defs":{}},"strict":true}}]}`),
	}
	preflight := make([]string, 0, len(bodies))
	provider := make([]string, 0, len(bodies))
	providerBodies := make([][]byte, 0, len(bodies))
	for index, body := range bodies {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, grokChatRawEndpoint, bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 9901})
		preflight = append(preflight, resolveGrokCacheIdentity(c, body, "", "grok-4.5"))

		account := grokChatBridgeTestAccount(int64(9100 + index))
		repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
		upstream := &httpUpstreamRecorder{resp: grokChatBridgeCompletedResponse("resp_identity_"+fmt.Sprint(index), 128)}
		svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
		require.NoError(t, err)
		require.Equal(t, grokChatResponsesEndpoint, result.UpstreamEndpoint)
		provider = append(provider, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
		providerBodies = append(providerBodies, append([]byte(nil), upstream.lastBody...))
	}
	require.NotEmpty(t, preflight[0])
	require.NotEqual(t, preflight[0], preflight[1], "raw Chat schemas must affect admission identity only")
	require.NotEmpty(t, provider[0])
	require.Equal(t, provider[0], provider[1], "provider identity must use the shared schema-normalized Responses body")
	require.JSONEq(t, grokSchemaToolByName(t, providerBodies[0], "same_tool").Raw, grokSchemaToolByName(t, providerBodies[1], "same_tool").Raw)
}

func TestGrokChatResponsesCompatibilityKillSwitchRestoresPredecessorAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name             string
		parameters       string
		wantEligible     bool
		wantReason       string
		wantEndpoint     string
		wantUnchangedRef bool
	}{
		{name: "false schema returns to raw Chat", parameters: `false`, wantReason: "invalid_tool_function_parameters", wantEndpoint: grokChatRawEndpoint},
		{name: "missing parameters returns to raw Chat", parameters: ``, wantReason: "invalid_tool_function_parameters", wantEndpoint: grokChatRawEndpoint},
		{name: "object bad ref keeps predecessor Responses admission", parameters: `{"$ref":"#/$defs/missing","$defs":{}}`, wantEligible: true, wantEndpoint: grokChatResponsesEndpoint, wantUnchangedRef: true},
	}

	for index, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			parametersField := ""
			if testCase.parameters != "" {
				parametersField = `,"parameters":` + testCase.parameters
			}
			body := []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"rollback"}],"stream":false,"prompt_cache_key":"rollback-key","tools":[{"type":"function","function":{"name":"rollback_tool","description":"keep"` + parametersField + `,"strict":true}}]}`)
			enabled, reason := grokChatResponsesBridgeEligibilityWithCompatibility(body, true)
			require.True(t, enabled, reason)
			disabled, reason := grokChatResponsesBridgeEligibilityWithCompatibility(body, false)
			require.Equal(t, testCase.wantEligible, disabled)
			require.Equal(t, testCase.wantReason, reason)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, grokChatRawEndpoint, bytes.NewReader(body))
			c.Set("api_key", &APIKey{ID: int64(9950 + index)})
			account := grokChatBridgeTestAccount(int64(9300 + index))
			account.Extra = map[string]any{grokResponsesProtocolCompatibilityExtraKey: false}
			repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"chat_rollback","object":"chat.completion","model":"grok-4.5","choices":[{"index":0,"message":{"role":"assistant","content":"raw ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)),
			}
			if testCase.wantEndpoint == grokChatResponsesEndpoint {
				response = grokChatBridgeCompletedResponse("resp_rollback_"+fmt.Sprint(index), 64)
			}
			upstream := &httpUpstreamRecorder{resp: response}
			svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}
			before := snapshotGrokResponsesCompatibilityMetrics()
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, testCase.wantEndpoint, GetActualOpenAIUpstreamEndpoint(c))
			require.Equal(t, testCase.wantEndpoint, upstream.lastReq.URL.Path)
			after := snapshotGrokResponsesCompatibilityMetrics()
			require.Equal(t, before, after, "disabled Chat routing must not emit v2 compatibility metrics")
			if testCase.wantUnchangedRef {
				tool := grokSchemaToolByName(t, upstream.lastBody, "rollback_tool")
				require.Equal(t, "#/$defs/missing", tool.Get("parameters.$ref").String())
				require.True(t, tool.Get("strict").Bool())
				require.Equal(t, "rollback-key", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
				require.Equal(t, "rollback-key", upstream.lastReq.Header.Get(grokConversationIDHeader))
			}
		})
	}
}

func TestGrokWSSchemaFallbackReappliesToInheritedTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		grokCompatibilityStreamingResponse("resp_inherited_first"),
		grokCompatibilityStreamingResponse("resp_inherited_second"),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream}
	account := &Account{ID: 9201, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"base_url": "https://grok.example.test/v1", "api_key": "test-key"}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	first := []byte(`{"type":"response.create","model":"grok-4.5","stream":true,"input":[],"tools":[{"type":"namespace","name":"matrix_ns","tools":[{"type":"function","name":"inherited_bad","description":"keep","strict":true,"parameters":{"$ref":"#/$defs/missing","$defs":{}}}]}]}`)
	_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "token", first, len(first), "grok-4.5", "", "", "", "inherited-cache", 1, func([]byte) error { return nil })
	require.NoError(t, err)

	second := []byte(`{"type":"response.create","model":"grok-4.5","stream":true,"input":[]}`)
	_, err = svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "token", second, len(second), "grok-4.5", "", "", "", "inherited-cache", 2, func([]byte) error { return nil })
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	for _, body := range upstream.bodies {
		tool := grokSchemaToolByName(t, body, "matrix_ns__inherited_bad")
		require.Equal(t, "keep", tool.Get("description").String())
		require.JSONEq(t, string(permissiveGrokObjectSchema()), tool.Get("parameters").Raw)
		require.False(t, tool.Get("strict").Bool())
	}
}

func loadGrokSchemaMatrixFixtures(t *testing.T) grokSchemaMatrixFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "grok_schema_compat_matrix.json"))
	require.NoError(t, err)
	var fixtures grokSchemaMatrixFixtures
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.Equal(t, 11, len(fixtures.Cases))
	return fixtures
}

func assertGrokSchemaMatrixFinalTool(t *testing.T, body []byte, original json.RawMessage, fallbackReason string) {
	t.Helper()
	tool := grokSchemaToolByName(t, body, "matrix_tool")
	assertGrokSchemaMatrixRoot(t, tool.Get("parameters"))
	if fallbackReason != "" {
		require.JSONEq(t, string(permissiveGrokObjectSchema()), tool.Get("parameters").Raw)
		require.False(t, tool.Get("strict").Bool())
		return
	}
	require.True(t, tool.Get("strict").Bool())
	if gjson.GetBytes(original, "type").String() == "object" {
		require.JSONEq(t, string(original), tool.Get("parameters").Raw)
	}
}

func assertGrokSchemaMatrixRoot(t *testing.T, parameters gjson.Result) {
	t.Helper()
	require.True(t, parameters.IsObject())
	require.Equal(t, "object", parameters.Get("type").String())
	require.False(t, parameters.Get("$ref").Exists())
	assertNoGrokRootRefOrNull(t, parameters.Value())
}

func grokSchemaToolByName(t *testing.T, body []byte, name string) gjson.Result {
	t.Helper()
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() == "function" && tool.Get("name").String() == name {
			return tool
		}
	}
	t.Fatalf("function tool %q not found in %s", name, string(body))
	return gjson.Result{}
}

func TestGrokResponsesSchemaStageRouteMatrixFixtureIsCurrent(t *testing.T) {
	fixtures := loadGrokSchemaMatrixFixtures(t)
	require.Equal(t, "grok-4.5", fixtures.Model)
	require.Equal(t, []string{
		"kiki_like_pure_object_control", "root_anyof_object_string", "root_anyof_object_null",
		"local_ref_object_oneof", "cyclic_local_ref", "missing_local_ref", "external_ref",
		"false_schema", "malformed_combinator", "malformed_ref_value", "json_string_schema",
	}, func() []string {
		names := make([]string, 0, len(fixtures.Cases))
		for _, schemaCase := range fixtures.Cases {
			names = append(names, schemaCase.Name)
		}
		return names
	}())
}
