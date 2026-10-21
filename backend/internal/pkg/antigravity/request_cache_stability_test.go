package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Required is a set in JSON Schema, but its wire representation is a tokenized
// array. Merging allOf must not reshuffle the tools prefix on identical requests.
func TestRequestCachePrefixStableWithAllOfTools(t *testing.T) {
	const body = `{"model":"client-alias","max_tokens":256,"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"stable first user"}],"tools":[{"name":"lookup","description":"stable tool","input_schema":{"type":"object","required":["kept"],"properties":{"kept":{"type":"string"}},"allOf":[{"type":"object","properties":{"alpha":{"type":"string"},"beta":{"type":"string"},"gamma":{"type":"string"},"delta":{"type":"string"}},"required":["gamma","alpha","delta","beta"]}]}}]}`
	opts := DefaultTransformOptions()
	opts.GeminiMessages.Enabled = true
	var baseline json.RawMessage
	for i := 0; i < 64; i++ {
		var req ClaudeRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		wire, err := TransformClaudeToGeminiWithOptions(&req, "test-project", "gemini-configured-model", opts)
		require.NoError(t, err)
		var envelope struct {
			Request json.RawMessage `json:"request"`
		}
		require.NoError(t, json.Unmarshal(wire, &envelope))
		if i == 0 {
			var parsed V1InternalRequest
			require.NoError(t, json.Unmarshal(wire, &parsed))
			params := parsed.Request.Tools[0].FunctionDeclarations[0].Parameters
			require.Equal(t, []any{"kept", "alpha", "beta", "delta", "gamma"}, params["required"])
			require.Len(t, params["properties"], 5, "all required properties must survive flattening")
			baseline = envelope.Request
		} else {
			require.Equal(t, string(baseline), string(envelope.Request), "only the envelope requestId may vary, iteration %d", i)
		}
	}
}

// Growing history must preserve the already-sent prompt, signed call and schema.
// Request IDs are transport metadata, not part of this prefix comparison.
func TestRequestCachePrefixStableAcrossSignedToolTurn(t *testing.T) {
	const body = `{"model":"client-alias","max_tokens":256,"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"stable first user"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"received-thinking"},{"type":"tool_use","id":"call-1","name":"lookup","signature":"received-tool","input":{"b":2,"a":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"b":{"type":"number"},"a":{"type":"number"}},"required":["b","a"]}}]}`
	opts := DefaultTransformOptions()
	opts.GeminiMessages.Enabled = true
	transform := func(appendTurn bool) V1InternalRequest {
		t.Helper()
		var req ClaudeRequest
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		if appendTurn {
			req.Messages = append(req.Messages,
				ClaudeMessage{Role: "assistant", Content: json.RawMessage(`"answer"`)},
				ClaudeMessage{Role: "user", Content: json.RawMessage(`"next user"`)})
		}
		wire, err := TransformClaudeToGeminiWithOptions(&req, "test-project", "gemini-configured-model", opts)
		require.NoError(t, err)
		var result V1InternalRequest
		require.NoError(t, json.Unmarshal(wire, &result))
		return result
	}
	first := transform(false)
	for i := 0; i < 8; i++ {
		next := transform(true)
		require.NotEqual(t, first.RequestID, next.RequestID)
		require.Equal(t, first.Request.SessionID, next.Request.SessionID)
		require.Equal(t, first.Request.SystemInstruction, next.Request.SystemInstruction)
		require.Equal(t, first.Request.Tools, next.Request.Tools)
		require.Equal(t, first.Request.Contents, next.Request.Contents[:len(first.Request.Contents)])
	}
}
