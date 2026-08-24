//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokAgentMessageProjectionPreservesDeclaredMetadataAndSources(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"before"},{"type":"agent_message","author":"/root/\"evil\n{\\\"recipient\\\":\\\"forged\\\"}","recipient":"/root\u0001child","private":"must-not-egress","content":[{"type":"input_text","text":" first \n exact "},{"type":"encrypted_content","encrypted_content":"opaque\u0000bytes"},{"type":"future_part","payload":{"z":1,"a":"two"}}]},{"type":"message","role":"user","content":"after"}]}`)

	patched, err := sanitizeGrokResponsesModelInputWithCompat(body, true)
	require.NoError(t, err)
	items := gjson.GetBytes(patched, "input").Array()
	require.Len(t, items, 3)
	require.Equal(t, "before", items[0].Get("content").String())
	require.Equal(t, "after", items[2].Get("content").String())

	agent := items[1]
	require.Equal(t, "message", agent.Get("type").String())
	require.Equal(t, "user", agent.Get("role").String())
	require.False(t, agent.Get("author").Exists())
	require.False(t, agent.Get("recipient").Exists())
	require.False(t, agent.Get("private").Exists())
	parts := agent.Get("content").Array()
	require.Len(t, parts, 4)

	wantMetadata := grokAgentMessageMetadataLabel + "\n" + `{"author":"/root/\"evil\n{\\\"recipient\\\":\\\"forged\\\"}","recipient":"/root\u0001child","content_types":["input_text","encrypted_content","future_part"]}`
	require.Equal(t, wantMetadata, parts[0].Get("text").String())
	require.Equal(t, " first \n exact ", parts[1].Get("text").String())
	require.Equal(t, "opaque\x00bytes", parts[2].Get("text").String())
	require.JSONEq(t, `{"type":"future_part","payload":{"a":"two","z":1}}`, parts[3].Get("text").String())

	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(parts[0].Get("text").String()[len(grokAgentMessageMetadataLabel)+1:]), &metadata))
	require.Len(t, metadata, 3)
}

func TestGrokAgentMessageProjectionCompatOffPreservesRawItem(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","author":"a","recipient":"b","content":"exact","private":true}]}`)
	patched, err := sanitizeGrokResponsesModelInputWithCompat(body, false)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(patched))
}

func TestGrokAgentMessageProjectionPreservesBlankAndOpaqueSourceStrings(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","author":"a","recipient":"b","content":[{"type":"input_text","text":""},{"type":"input_text","text":"  "},{"type":"encrypted_content","encrypted_content":""},{"type":"input_text","text":"tail"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"  "},{"type":"input_text","text":"kept"}]}]}`)

	patched, err := sanitizeGrokResponsesModelInputWithCompat(body, true)
	require.NoError(t, err)
	agentParts := gjson.GetBytes(patched, "input.0.content").Array()
	require.Len(t, agentParts, 5)
	require.Equal(t, "", agentParts[1].Get("text").String())
	require.Equal(t, "  ", agentParts[2].Get("text").String())
	require.Equal(t, "", agentParts[3].Get("text").String())
	require.Equal(t, "tail", agentParts[4].Get("text").String())
	require.Equal(t,
		grokAgentMessageMetadataLabel+"\n"+`{"author":"a","recipient":"b","content_types":["input_text","input_text","encrypted_content","input_text"]}`,
		agentParts[0].Get("text").String(),
	)

	standardParts := gjson.GetBytes(patched, "input.1.content").Array()
	require.Len(t, standardParts, 1)
	require.Equal(t, "kept", standardParts[0].Get("text").String())
}

func TestNormalizeGrokFunctionParametersFiniteMatrix(t *testing.T) {
	tests := []struct {
		name        string
		schema      string
		disposition grokSchemaDisposition
		want        string
	}{
		{name: "direct object", schema: `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`},
		{name: "object and null type", schema: `{"type":["object","null"],"properties":{}}`, disposition: grokSchemaProvenObject, want: `{"type":"object","properties":{}}`},
		{name: "local ref with sibling", schema: `{"$ref":"#/$defs/base","required":["q"],"$defs":{"base":{"type":"object","properties":{"q":{"type":"string"}}}}}`, disposition: grokSchemaProvenObject, want: `{"type":"object","required":["q"],"properties":{"q":{"type":"string"}},"$defs":{"base":{"type":"object","properties":{"q":{"type":"string"}}}}}`},
		{name: "escaped local ref", schema: `{"$ref":"#/$defs/a~1b","$defs":{"a/b":{"type":"object","properties":{}}}}`, disposition: grokSchemaProvenObject, want: `{"type":"object","properties":{},"$defs":{"a/b":{"type":"object","properties":{}}}}`},
		{name: "object and null any of", schema: `{"anyOf":[{"type":"object","properties":{}},{"type":"null"}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","anyOf":[{"type":"object","properties":{}}]}`},
		{name: "all of object intersection", schema: `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","required":["a"]}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`},
		{name: "object const witness", schema: `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"const":{"q":"yes"}}`, disposition: grokSchemaProvenObject, want: `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"const":{"q":"yes"}}`},
		{name: "const compatible with any of", schema: `{"type":"object","const":{"x":1},"anyOf":[{"type":"object","const":{"x":1}}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","const":{"x":1},"anyOf":[{"type":"object","const":{"x":1}}]}`},
		{name: "const conflicts with any of", schema: `{"type":"object","const":{"x":2},"anyOf":[{"type":"object","const":{"x":1}}]}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "enum filtered by any of", schema: `{"type":"object","enum":[{"x":1},{"x":2}],"anyOf":[{"type":"object","const":{"x":1}}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","enum":[{"x":1}],"anyOf":[{"type":"object","const":{"x":1}}]}`},
		{name: "mixed enum keeps object", schema: `{"type":"object","enum":[1,{"ok":true},"x"]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","enum":[{"ok":true}]}`},
		{name: "enum has no object", schema: `{"enum":[1,"x",null]}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "one of finite exclusive witnesses", schema: `{"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`},
		{name: "const compatible with one of", schema: `{"const":{"kind":"a"},"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","const":{"kind":"a"},"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`},
		{name: "const conflicts with one of", schema: `{"const":{"kind":"c"},"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "one of finite witness conflicts with required sibling", schema: `{"properties":{"q":{"type":"string"}},"required":["q"],"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "enum filtered by one of", schema: `{"enum":[{"kind":"a"},{"kind":"c"}],"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`, disposition: grokSchemaProvenObject, want: `{"type":"object","enum":[{"kind":"a"}],"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`},
		{name: "non object const", schema: `{"type":"object","const":"no"}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "false schema", schema: `false`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "primitive root", schema: `"string"`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "mixed object primitive union", schema: `{"anyOf":[{"type":"object"},{"type":"string"}]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "object only one of without witness", schema: `{"oneOf":[{"type":"object"},{"type":"object","required":["q"],"properties":{"q":{"type":"string"}}}]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "external ref", schema: `{"$ref":"https://example.test/schema"}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "missing ref", schema: `{"$ref":"#/$defs/missing","$defs":{}}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "ref sibling type contradiction", schema: `{"$ref":"#/$defs/base","type":"string","$defs":{"base":{"type":"object"}}}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "cyclic ref", schema: `{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}}}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "dynamic ref", schema: `{"$dynamicRef":"#node"}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "root not remains unsupported", schema: `{"type":"object","not":{"const":{"x":1}}}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "one of nested not remains unsupported", schema: `{"oneOf":[{"const":{"x":1}},{"not":{"const":{"x":1}}}]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "any of nested not remains unsupported", schema: `{"anyOf":[{"type":"object","const":{"x":1}},{"not":{"const":{"x":1}}}]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "all of nested not remains unsupported", schema: `{"allOf":[{"type":"object"},{"not":{"const":{"x":1}}}]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "unsupported root keyword", schema: `{"type":"object","patternProperties":{".*":{}}}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "malformed type", schema: `{"type":["object",7]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "empty type union", schema: `{"type":[]}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "empty enum", schema: `{"type":"object","enum":[]}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "malformed bound", schema: `{"type":"object","minProperties":null}`, disposition: grokSchemaUnknown, want: grokSchemaFallbackJSON},
		{name: "impossible required property", schema: `{"type":"object","properties":{},"required":["q"],"additionalProperties":false}`, disposition: grokSchemaContradictory, want: grokSchemaFallbackJSON},
		{name: "annotations and extension", schema: `{"title":"kept","x-owner":"client","type":"object","properties":{}}`, disposition: grokSchemaProvenObject, want: `{"title":"kept","x-owner":"client","type":"object","properties":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, disposition := normalizeGrokFunctionParameters(json.RawMessage(tt.schema))
			require.Equal(t, tt.disposition, disposition)
			require.JSONEq(t, tt.want, string(got))
		})
	}
}

const grokSchemaFallbackJSON = `{"type":"object","properties":{},"additionalProperties":true}`

func TestNormalizeGrokFunctionParametersBudgetIsSchemaLocal(t *testing.T) {
	budget := &grokSchemaBudget{MaxSize: 8, MaxDepth: 32, MaxRefVisits: 128}
	got, disposition := normalizeGrokFunctionParametersWithBudget(json.RawMessage(`{"type":"object"}`), budget)
	require.Equal(t, grokSchemaUnknown, disposition)
	require.JSONEq(t, grokSchemaFallbackJSON, string(got))

	refBudget := &grokSchemaBudget{MaxSize: 1024, MaxDepth: 32, MaxRefVisits: 1}
	got, disposition = normalizeGrokFunctionParametersWithBudget(json.RawMessage(`{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"type":"object"}}}`), refBudget)
	require.Equal(t, grokSchemaUnknown, disposition)
	require.JSONEq(t, grokSchemaFallbackJSON, string(got))

	depthBudget := &grokSchemaBudget{MaxSize: 1024, MaxDepth: 1, MaxRefVisits: 128}
	got, disposition = normalizeGrokFunctionParametersWithBudget(json.RawMessage(`{"allOf":[{"allOf":[{"type":"object"}]}]}`), depthBudget)
	require.Equal(t, grokSchemaUnknown, disposition)
	require.JSONEq(t, grokSchemaFallbackJSON, string(got))
}

func TestNormalizeGrokFunctionParametersDepthBudgetCoversPreservedSubtrees(t *testing.T) {
	nestedProperties := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"deep":{"type":"string"}}}}}`)
	tooShallow := &grokSchemaBudget{MaxSize: 4096, MaxDepth: 1, MaxRefVisits: 128}
	got, disposition := normalizeGrokFunctionParametersWithBudget(nestedProperties, tooShallow)
	require.Equal(t, grokSchemaUnknown, disposition)
	require.JSONEq(t, grokSchemaFallbackJSON, string(got))

	atBoundary := &grokSchemaBudget{MaxSize: 4096, MaxDepth: 4, MaxRefVisits: 128}
	got, disposition = normalizeGrokFunctionParametersWithBudget(nestedProperties, atBoundary)
	require.Equal(t, grokSchemaProvenObject, disposition)
	require.JSONEq(t, string(nestedProperties), string(got))

	directBoundary := &grokSchemaBudget{MaxSize: 4096, MaxDepth: 0, MaxRefVisits: 128}
	got, disposition = normalizeGrokFunctionParametersWithBudget(json.RawMessage(`{"type":"object"}`), directBoundary)
	require.Equal(t, grokSchemaProvenObject, disposition)
	require.JSONEq(t, `{"type":"object"}`, string(got))

	for _, schema := range []json.RawMessage{
		json.RawMessage(`{"type":"object","properties":{"values":{"type":"array","items":{"type":"string"}}}}`),
		json.RawMessage(`{"type":"object","definitions":{"nested":{"type":"object","properties":{"value":{"type":"string"}}}}}`),
	} {
		budget := &grokSchemaBudget{MaxSize: 4096, MaxDepth: 1, MaxRefVisits: 128}
		got, disposition = normalizeGrokFunctionParametersWithBudget(schema, budget)
		require.Equal(t, grokSchemaUnknown, disposition)
		require.JSONEq(t, grokSchemaFallbackJSON, string(got))
	}
}

func TestNormalizeGrokFunctionParametersIsIdempotentAfterRepair(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"type":"object","const":{"x":1},"anyOf":[{"type":"object","const":{"x":1}}]}`),
		json.RawMessage(`{"const":{"kind":"a"},"oneOf":[{"const":{"kind":"a"}},{"const":{"kind":"b"}}]}`),
		json.RawMessage(`{"type":"object","const":{"x":2},"anyOf":[{"type":"object","const":{"x":1}}]}`),
	} {
		first, _ := normalizeGrokFunctionParameters(raw)
		second, _ := normalizeGrokFunctionParameters(first)
		require.JSONEq(t, string(first), string(second))
	}
}

func TestSanitizeGrokResponsesToolsSchemaFallbackIsPerToolAndCompatGated(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"good","description":"keep","parameters":{"type":"object","properties":{}},"strict":true,"x-outer":"one"},{"type":"function","name":"bad","parameters":false,"strict":true,"x-outer":"two"},{"type":"web_search"}],"tool_choice":"auto"}`)
	patched, err := sanitizeGrokResponsesToolsWithCompat(body, true)
	require.NoError(t, err)
	tools := gjson.GetBytes(patched, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "good", tools[0].Get("name").String())
	require.True(t, tools[0].Get("strict").Bool())
	require.Equal(t, "one", tools[0].Get("x-outer").String())
	require.Equal(t, "bad", tools[1].Get("name").String())
	require.False(t, tools[1].Get("strict").Bool())
	require.JSONEq(t, grokSchemaFallbackJSON, tools[1].Get("parameters").Raw)
	require.Equal(t, "two", tools[1].Get("x-outer").String())
	require.Equal(t, "web_search", tools[2].Get("type").String())

	off, err := sanitizeGrokResponsesToolsWithCompat(body, false)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(off))
}

func TestGrokSchemaSevenCaseRouteMatrix(t *testing.T) {
	cases := []struct {
		name       string
		parameters string
		missing    bool
	}{
		{name: "missing", missing: true},
		{name: "null", parameters: `null`},
		{name: "local ref", parameters: `{"$ref":"#/$defs/args","$defs":{"args":{"type":"object","properties":{"q":{"type":"string"}}}}}`},
		{name: "object one of", parameters: `{"oneOf":[{"type":"object","const":{"kind":"a"}},{"type":"object","const":{"kind":"b"}}]}`},
		{name: "false", parameters: `false`},
		{name: "malformed combinator", parameters: `{"anyOf":{"type":"object"}}`},
		{name: "json string", parameters: `"object"`},
	}
	account := healthyGrokOAuthGatewayTestAccount(8801, "access-token")
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			functionField := `{"name":"lookup","strict":true`
			responsesTool := `{"type":"function","name":"lookup","strict":true`
			if !tt.missing {
				functionField += `,"parameters":` + tt.parameters
				responsesTool += `,"parameters":` + tt.parameters
			}
			functionField += `}`
			responsesTool += `}`

			native, _, err := patchGrokResponsesBodyWithClientToolsCompat(
				[]byte(`{"model":"grok-4.6","input":"hi","tools":[`+responsesTool+`]}`),
				"grok-4.6",
				true,
			)
			require.NoError(t, err)
			assertGrokRouteHasObjectParameters(t, native)

			anthropicSchema := json.RawMessage(nil)
			if !tt.missing {
				anthropicSchema = json.RawMessage(tt.parameters)
			}
			messagesReq := &apicompat.AnthropicRequest{
				Model:     "grok-4.6",
				MaxTokens: 32,
				Messages:  []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
				Tools:     []apicompat.AnthropicTool{{Name: "lookup", InputSchema: anthropicSchema}},
			}
			convertedMessages, err := apicompat.AnthropicToResponses(messagesReq)
			require.NoError(t, err)
			messagesBody, err := json.Marshal(convertedMessages)
			require.NoError(t, err)
			messagesBody, err = patchGrokResponsesBodyBaseWithCompat(messagesBody, "grok-4.6", true)
			require.NoError(t, err)
			assertGrokRouteHasObjectParameters(t, messagesBody)

			chatBody := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":` + functionField + `}]}`)
			eligible, reason := grokChatResponsesBridgeEligibilityWithCompat(chatBody, true)
			require.True(t, eligible, reason)
			var chatReq apicompat.ChatCompletionsRequest
			require.NoError(t, json.Unmarshal(chatBody, &chatReq))
			convertedChat, err := apicompat.ChatCompletionsToResponses(&chatReq)
			require.NoError(t, err)
			chatResponsesBody, err := json.Marshal(convertedChat)
			require.NoError(t, err)
			chatResponsesBody, err = patchGrokResponsesBodyBaseWithCompat(chatResponsesBody, "grok-4.6", true)
			require.NoError(t, err)
			assertGrokRouteHasObjectParameters(t, chatResponsesBody)

			wsPayload := []byte(`{"type":"response.create","model":"grok-4.6","input":"hi","tools":[` + responsesTool + `]}`)
			prepared, err := prepareGrokWSResponsesBody(wsPayload, account, "grok-4.6", apicompat.ResponsesClientToolMapping{}, nil, true)
			require.NoError(t, err)
			assertGrokRouteHasObjectParameters(t, prepared.Body)
		})
	}
}

func assertGrokRouteHasObjectParameters(t *testing.T, body []byte) {
	t.Helper()
	parameters := gjson.GetBytes(body, "tools.0.parameters")
	require.True(t, parameters.IsObject(), string(body))
	require.Equal(t, "object", parameters.Get("type").String(), string(body))
	require.False(t, parameters.Get("$ref").Exists(), string(body))
}

func TestGrokChatSchemaAdmissionCompatOnlyRelaxesParameters(t *testing.T) {
	for _, body := range []string{
		`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":false}}]}`,
		`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":"object"}}]}`,
	} {
		ok, reason := grokChatResponsesBridgeEligibilityWithCompat([]byte(body), true)
		require.True(t, ok, reason)
		ok, reason = grokChatResponsesBridgeEligibilityWithCompat([]byte(body), false)
		require.False(t, ok)
		require.Equal(t, "invalid_tool_function_parameters", reason)
	}
	badStrict := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":false,"strict":"false"}}]}`)
	ok, reason := grokChatResponsesBridgeEligibilityWithCompat(badStrict, true)
	require.False(t, ok)
	require.Equal(t, "invalid_tool_function_strict", reason)
}

func TestForwardGrokOAuthChatSchemaFallbackUsesActualResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"stream":false,"tools":[{"type":"function","function":{"name":"lookup","description":"keep","parameters":false,"strict":true}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, grokChatRawEndpoint, bytes.NewReader(body))
	c.Set("api_key", &APIKey{ID: 7201})
	account := grokChatBridgeTestAccount(7201)
	repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &httpUpstreamRecorder{resp: grokChatBridgeCompletedResponse("resp_schema_fallback", 0)}
	svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: NewGrokTokenProvider(repo, nil), accountRepo: repo}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
	require.NoError(t, err)
	require.Equal(t, grokChatResponsesEndpoint, result.UpstreamEndpoint)
	require.Equal(t, xai.DefaultCLIBaseURL+"/responses", upstream.lastReq.URL.String())
	require.Equal(t, "lookup", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
	require.JSONEq(t, grokSchemaFallbackJSON, gjson.GetBytes(upstream.lastBody, "tools.0.parameters").Raw)
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
	require.Equal(t,
		independentlyExpectedGrokToolPrefixIdentity(t, 7201, "grok-4.6", stripGrokPromptCacheKey(upstream.lastBody)),
		gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(),
	)
}

func TestForwardGrokNativeAgentAndSchemaCompatibilityOAuthAndAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			body := []byte(`{"model":"grok-4.6","input":[{"type":"agent_message","author":"/root/child","recipient":"/root","content":[{"type":"input_text","text":"result"}]}],"stream":false,"tools":[{"type":"function","name":"lookup","parameters":false,"strict":true}]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Set("api_key", &APIKey{ID: 7301})
			account := &Account{ID: 7301, Name: "grok", Platform: PlatformGrok, Type: accountType, Credentials: map[string]any{}}
			var repo *grokQuotaAccountRepo
			if accountType == AccountTypeOAuth {
				account = healthyGrokOAuthGatewayTestAccount(7301, "access-token")
				repo = &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
			} else {
				account.Credentials["api_key"] = "xai-test-key"
				account.Credentials["base_url"] = "https://api.x.ai/v1"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_native_compat","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}}
			svc := &OpenAIGatewayService{httpUpstream: upstream, accountRepo: repo}
			if repo != nil {
				svc.grokTokenProvider = NewGrokTokenProvider(repo, nil)
			}

			result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok-4.6", false, time.Now())
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Empty(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="agent_message")`).Array())
			require.Equal(t, "message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
			require.JSONEq(t, grokSchemaFallbackJSON, gjson.GetBytes(upstream.lastBody, "tools.0.parameters").Raw)
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
			identity := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
			require.NotEmpty(t, identity)
			require.Equal(t, identity, upstream.lastReq.Header.Get(grokConversationIDHeader))
		})
	}
}

func TestGrokFinalBodyIdentityIncludesFreeAugmentationAndExplicitSeedWins(t *testing.T) {
	c := newGrokCacheTestContext(731)
	account := healthyGrokOAuthGatewayTestAccount(731, "access-token")
	account.Credentials["subscription_tier"] = "free"
	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"hello"}]}`)
	hint := captureGrokCacheSeedHint(c, body, "")
	require.True(t, canDeriveGrokCacheIdentity(c, body, hint, "grok-4.5"))
	augmented, err := augmentGrokResponsesCacheRoute(c, body, body, account, true)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(augmented, "tools").Array(), 2)
	require.Equal(t, "none", gjson.GetBytes(augmented, "tool_choice").String())
	identity := resolveGrokCacheIdentityFromFinal(c, augmented, hint, "grok-4.5")
	require.Equal(t, independentlyExpectedGrokToolPrefixIdentity(t, 731, "grok-4.5", augmented), identity)

	explicit := grokCacheSeedHint{explicit: "client-seed"}
	first := resolveGrokCacheIdentityFromFinal(c, body, explicit, "grok-4.5")
	second := resolveGrokCacheIdentityFromFinal(c, augmented, explicit, "grok-4.5")
	require.Equal(t, first, second)
	require.NotContains(t, string(augmented), "client-seed")
}

func TestPrepareGrokWSResponsesBodyMatchesFirstEffectiveProjection(t *testing.T) {
	account := healthyGrokOAuthGatewayTestAccount(811, "access-token")
	account.Credentials["subscription_tier"] = "free"
	c := newGrokCacheTestContext(811)
	payload := []byte(`{"type":"response.create","model":"grok-4.5","input":[{"type":"agent_message","author":"/root/child","recipient":"/root","content":[{"type":"input_text","text":"done"}]}],"tools":[{"type":"custom","name":"exec","format":{"type":"text"}},{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":false}]}]}`)

	seedPrepared, err := prepareGrokWSResponsesBody(payload, account, "grok-4.5", apicompat.ResponsesClientToolMapping{}, nil, true)
	require.NoError(t, err)
	effectivePrepared, err := prepareGrokWSResponsesBody(payload, account, "grok-4.5", apicompat.ResponsesClientToolMapping{}, nil, true)
	require.NoError(t, err)
	require.JSONEq(t, string(seedPrepared.Body), string(effectivePrepared.Body))
	require.Empty(t, gjson.GetBytes(seedPrepared.Body, `input.#(type=="agent_message")`).Array())
	require.Len(t, gjson.GetBytes(seedPrepared.Body, "tools").Array(), 2)
	require.Equal(t, "object", gjson.GetBytes(seedPrepared.Body, "tools.1.parameters.type").String())
	require.False(t, gjson.GetBytes(seedPrepared.Body, "tools.1.strict").Bool())

	hint := captureGrokCacheSeedHint(c, payload, "")
	available := canDeriveGrokCacheIdentity(c, seedPrepared.Body, hint, "grok-4.5")
	seedAugmented, err := augmentGrokResponsesCacheRoute(c, seedPrepared.Body, seedPrepared.CacheIntentSource, account, available)
	require.NoError(t, err)
	effectiveAugmented, err := augmentGrokResponsesCacheRoute(c, effectivePrepared.Body, effectivePrepared.CacheIntentSource, account, available)
	require.NoError(t, err)
	require.JSONEq(t, string(seedAugmented), string(effectiveAugmented))
	require.Len(t, gjson.GetBytes(seedAugmented, "tools").Array(), 4)
	require.Equal(t,
		resolveGrokCacheIdentityFromFinal(c, seedAugmented, hint, "grok-4.5"),
		resolveGrokCacheIdentityFromFinal(c, effectiveAugmented, hint, "grok-4.5"),
	)

	inherited := decodeOpenAIWSHTTPBridgeLoweredTools(seedPrepared.LoweredTools)
	followup, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.5","input":"next"}`),
		account,
		"grok-4.5",
		seedPrepared.Mapping,
		inherited,
		true,
	)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(followup.Body, "tools").Array(), 2)
	require.Equal(t, "collaboration__send_message", gjson.GetBytes(followup.Body, "tools.1.name").String())
	require.JSONEq(t, grokSchemaFallbackJSON, gjson.GetBytes(followup.Body, "tools.1.parameters").Raw)
	require.False(t, gjson.GetBytes(followup.Body, "tools.1.strict").Bool())
}

func TestGrokResponsesProtocolCompatSwitchOnlyExplicitFalseDisables(t *testing.T) {
	for _, tt := range []struct {
		extra map[string]any
		want  bool
	}{
		{extra: nil, want: true},
		{extra: map[string]any{}, want: true},
		{extra: map[string]any{grokResponsesProtocolCompatExtraKey: true}, want: true},
		{extra: map[string]any{grokResponsesProtocolCompatExtraKey: false}, want: false},
		{extra: map[string]any{grokResponsesProtocolCompatExtraKey: "false"}, want: true},
	} {
		require.Equal(t, tt.want, grokResponsesProtocolCompatEnabled(&Account{Extra: tt.extra}))
	}
}

func TestGrokProtocolCompatOffKeepsNewSemanticsDisabledButCurrentRepairEnabled(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","input":[{"type":"agent_message","author":"a","recipient":"b","content":"raw"}],"tools":[{"type":"function","name":"present","parameters":false,"strict":true},{"type":"function","name":"missing","strict":true}]}`)
	patched, _, err := patchGrokResponsesBodyWithClientToolsCompat(body, "grok-4.6", false)
	require.NoError(t, err)
	require.Equal(t, "agent_message", gjson.GetBytes(patched, "input.0.type").String())
	require.Equal(t, gjson.False, gjson.GetBytes(patched, "tools.0.parameters").Type)
	require.True(t, gjson.GetBytes(patched, "tools.0.strict").Bool())
	require.JSONEq(t, `{"type":"object","properties":{}}`, gjson.GetBytes(patched, "tools.1.parameters").Raw)
	require.True(t, gjson.GetBytes(patched, "tools.1.strict").Bool())
}

func independentlyExpectedGrokToolPrefixIdentity(t *testing.T, apiKeyID int64, model string, body []byte) string {
	t.Helper()
	seed := "compat_csp_"
	appendCanonical := func(label string, raw string) {
		var value any
		require.NoError(t, json.Unmarshal([]byte(raw), &value))
		canonical, err := json.Marshal(value)
		require.NoError(t, err)
		seed += "|" + label + "=" + string(canonical)
	}
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		appendCanonical("tools", tools.Raw)
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Exists() && instructions.String() != "" {
		appendCanonical("instructions", instructions.Raw)
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		role := item.Get("role").String()
		if role == "system" || role == "developer" {
			appendCanonical(role, item.Get("content").Raw)
		}
	}
	isolated := fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed)
	hash := sha256.Sum256([]byte(isolated))
	bytes := hash[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}
