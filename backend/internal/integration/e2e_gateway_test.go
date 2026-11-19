package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Offline, deterministic replacements for the legacy external-localhost E2E
// gateway tests. Every scenario drives the real Gin gateway router, auth and
// group middlewares, GatewayHandler, scheduler, Antigravity forwarder, and
// usage accounting with zero environment skips.

func TestE2E_GatewayModels(t *testing.T) {
	acc := newAntigravityAccount(401, "ag-models-e2e", 1, map[string]any{
		"claude-sonnet-4-5": "claude-sonnet-4-5",
		"gemini-3-flash":    "gemini-3-flash",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
	})

	rec := h.get(t, "/v1/models", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "list", resp.Object)
	require.NotEmpty(t, resp.Data)
}

func TestE2E_GatewayMessagesNonStream(t *testing.T) {
	acc := newAntigravityAccount(402, "ag-nonstream-e2e", 1, map[string]any{
		"claude-sonnet-4-5": "claude-sonnet-4-5",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("4", 15, 2, 0), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream": false,
		"messages": [{"role": "user", "content": "Say 4"}]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "message", gjson.GetBytes(rec.Body.Bytes(), "type").String())
	require.Equal(t, "assistant", gjson.GetBytes(rec.Body.Bytes(), "role").String())
	require.Equal(t, "4", gjson.GetBytes(rec.Body.Bytes(), "content.0.text").String())
	require.Equal(t, int64(15), gjson.GetBytes(rec.Body.Bytes(), "usage.input_tokens").Int())
	require.Equal(t, int64(2), gjson.GetBytes(rec.Body.Bytes(), "usage.output_tokens").Int())
	h.assertSlotsBalanced(t)
}

func TestE2E_GatewayMessagesStream(t *testing.T) {
	acc := newAntigravityAccount(403, "ag-stream-e2e", 1, map[string]any{
		"claude-sonnet-4-5": "claude-sonnet-4-5",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return sseHTTPResponse(
				`data: {"response":{"responseId":"resp_e2e_s1","candidates":[{"content":{"role":"model","parts":[{"text":"1, 2, 3"}]}}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":5}}}`,
				``,
				`data: {"response":{"responseId":"resp_e2e_s1","candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":6}}}`,
			), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream": true,
		"messages": [{"role": "user", "content": "Count from 1 to 3"}]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"))
	wire := rec.Body.String()
	require.Contains(t, wire, "event: message_start")
	require.Contains(t, wire, "event: content_block_delta")
	require.Contains(t, wire, "1, 2, 3")
	require.Contains(t, wire, "event: message_stop")
	h.assertSlotsBalanced(t)
}

func TestE2E_GatewayMessagesComplexToolSchema(t *testing.T) {
	acc := newAntigravityAccount(404, "ag-schema-e2e", 1, map[string]any{
		"claude-sonnet-4-5": "claude-sonnet-4-5",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return sseHTTPResponse(
				`data: {"response":{"responseId":"resp_schema","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_w1","name":"get_weather","args":{"location":"Tokyo","unit":"celsius"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":12}}}`,
			), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 256,
		"stream": false,
		"tools": [{
			"name": "get_weather",
			"description": "Get weather for a location",
			"input_schema": {
				"$schema": "https://json-schema.org/draft/2020-12/schema",
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"location": {"type": "string", "minLength": 1, "maxLength": 100, "pattern": "^[A-Za-z ]+$"},
					"unit": {"type": "string", "const": "celsius"},
					"extra": {"anyOf": [{"type": "string"}, {"type": "null"}]}
				},
				"required": ["location"]
			}
		}],
		"messages": [{"role": "user", "content": "What is the weather in Tokyo?"}]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Equal(t, "tool_use", gjson.GetBytes(rec.Body.Bytes(), "content.0.type").String())
	require.Equal(t, "get_weather", gjson.GetBytes(rec.Body.Bytes(), "content.0.name").String())

	reqs := h.requests()
	require.Len(t, reqs, 1)
	decl := gjson.GetBytes(reqs[0].Body, "request.tools.0.functionDeclarations.0")
	require.Equal(t, "get_weather", decl.Get("name").String())
	params := decl.Get("parameters")
	require.True(t, params.Exists(), "cleaned parameters must be present: %s", decl.Raw)
	require.False(t, params.Get("$schema").Exists(), "$schema must be stripped for v1internal")
	require.False(t, params.Get("additionalProperties").Exists(), "additionalProperties must be stripped")
	require.False(t, params.Get("properties.location.minLength").Exists(), "minLength must be stripped")
	require.False(t, params.Get("properties.location.pattern").Exists(), "pattern must be stripped")
	require.False(t, params.Get("properties.unit.const").Exists(), "const must be converted/stripped")
	require.Equal(t, "celsius", params.Get("properties.unit.enum.0").String(), "const should lower into single-value enum")
	require.False(t, params.Get("properties.extra.anyOf").Exists(), "nullable anyOf must be flattened")
	h.assertSlotsBalanced(t)
}

func TestE2E_GatewayMessagesThinkingUnsignedToolUse(t *testing.T) {
	acc := newAntigravityAccount(405, "ag-unsigned-tool-e2e", 1, map[string]any{
		"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("Sunny 22C in Tokyo", 35, 9, 0), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "claude-sonnet-4-5-thinking",
		"max_tokens": 4096,
		"stream": false,
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"tools": [{
			"name": "get_weather",
			"description": "Get weather",
			"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}
		}],
		"messages": [
			{"role": "user", "content": "What is the weather in Tokyo?"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me check."},
				{"type": "tool_use", "id": "toolu_unsigned_1", "name": "get_weather", "input": {"location": "Tokyo"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_unsigned_1", "content": "Sunny, 22C"}
			]}
		]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "Sunny 22C in Tokyo")

	reqs := h.requests()
	require.Len(t, reqs, 1)
	contents := gjson.GetBytes(reqs[0].Body, "request.contents").Array()
	require.GreaterOrEqual(t, len(contents), 3)

	var foundFuncCall, foundFuncResp bool
	for _, c := range contents {
		for _, p := range c.Get("parts").Array() {
			if p.Get("functionCall.name").String() == "get_weather" {
				foundFuncCall = true
			}
			if p.Get("functionResponse.name").String() == "get_weather" {
				foundFuncResp = true
			}
		}
	}
	require.True(t, foundFuncCall, "unsigned tool_use history must survive conversion as functionCall")
	require.True(t, foundFuncResp, "tool_result history must survive conversion as functionResponse")
	h.assertSlotsBalanced(t)
}

func TestE2E_GatewayMessagesThinkingWithoutSignature(t *testing.T) {
	acc := newAntigravityAccount(406, "ag-unsigned-think-e2e", 1, map[string]any{
		"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("6", 22, 4, 0), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "claude-sonnet-4-5-thinking",
		"max_tokens": 4096,
		"stream": false,
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"messages": [
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "2+2 is 4"},
				{"type": "text", "text": "4"}
			]},
			{"role": "user", "content": "And +2?"}
		]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "6")

	reqs := h.requests()
	require.Len(t, reqs, 1)
	// Unsigned thinking block must be downgraded/stripped rather than sent as an
	// invalid unsigned thought part.
	for _, c := range gjson.GetBytes(reqs[0].Body, "request.contents").Array() {
		for _, p := range c.Get("parts").Array() {
			if p.Get("thought").Bool() {
				require.NotEmpty(t, p.Get("thoughtSignature").String(),
					"any retained thought part must carry a valid thoughtSignature")
			}
		}
	}
	h.assertSlotsBalanced(t)
}

func TestE2E_GatewayMessagesGeminiModel(t *testing.T) {
	acc := newAntigravityAccount(407, "ag-gemini-e2e", 1, map[string]any{
		"gemini-2.5-flash": "gemini-2.5-flash",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("hello from gemini", 14, 6, 0), nil
		},
	})

	rec := h.post(t, "/v1/messages", nil, `{
		"model": "gemini-2.5-flash",
		"max_tokens": 100,
		"stream": false,
		"messages": [{"role": "user", "content": "Say hello"}]
	}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "hello from gemini")

	reqs := h.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "gemini-2.5-flash", gjson.GetBytes(reqs[0].Body, "model").String())
	h.assertSlotsBalanced(t)
}
