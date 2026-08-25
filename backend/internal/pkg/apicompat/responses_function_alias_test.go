package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAdaptResponsesFunctionToolAliasLowersAndRestores(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function", "name": "view_image", "description": "client declaration",
				"parameters": map[string]any{"anyOf": []any{map[string]any{"type": "object"}, map[string]any{"type": "null"}}},
			},
			map[string]any{"type": "function", "name": "shell_command"},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "view_image"}},
		"input": []any{
			map[string]any{"type": "function_call", "id": "fc_history", "call_id": "call_history", "name": "view_image", "arguments": `{"path":"C:/tmp/image","detail":"original","ignored":true}`},
			map[string]any{"type": "function_call_output", "call_id": "call_history", "output": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}}},
		},
	}
	mapping := ResponsesClientToolMapping{}
	providerDeclaration := map[string]any{
		"type": "function", "name": "read_file", "description": "images only",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{"target_file": map[string]any{"type": "string"}},
			"required": []any{"target_file"}, "additionalProperties": false,
		},
	}

	require.True(t, AdaptResponsesFunctionToolAlias(req, &mapping, "view_image", providerDeclaration, "path", "target_file"))
	require.Equal(t, "read_file", req["tools"].([]any)[0].(map[string]any)["name"])
	require.Equal(t, "view_image", mapping.FunctionAliases["read_file"].OriginalDeclaration["name"])
	require.Equal(t, "read_file", req["tool_choice"].(map[string]any)["name"])
	require.NotContains(t, req["tool_choice"].(map[string]any), "function")
	history := req["input"].([]any)
	require.Equal(t, "read_file", history[0].(map[string]any)["name"])
	require.JSONEq(t, `{"target_file":"C:/tmp/image"}`, history[0].(map[string]any)["arguments"].(string))
	require.Equal(t, "function_call_output", history[1].(map[string]any)["type"], "typed image output must remain untouched")

	payload := []byte(`{"response":{"output":[{"type":"function_call","id":"fc_result","call_id":"call_result","name":"read_file","arguments":"{\"target_file\":\"C:/tmp/result\",\"unexpected\":1}"},{"type":"function_call_output","call_id":"call_result","output":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "view_image", gjson.GetBytes(restored, "response.output.0.name").String())
	require.JSONEq(t, `{"path":"C:/tmp/result"}`, gjson.GetBytes(restored, "response.output.0.arguments").String())
	require.Equal(t, "fc_result", gjson.GetBytes(restored, "response.output.0.id").String())
	require.Equal(t, "call_result", gjson.GetBytes(restored, "response.output.0.call_id").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(restored, "response.output.1.type").String())
	require.Equal(t, "input_image", gjson.GetBytes(restored, "response.output.1.output.0.type").String())

	malformed := []byte(`{"output":[{"type":"function_call","name":"read_file","arguments":"{\"target_file\":\"x\"} trailing"}]}`)
	restored, changed, err = RestoreResponsesClientToolPayload(malformed, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "view_image", gjson.GetBytes(restored, "output.0.name").String())
	require.Equal(t, `{"target_file":"x"} trailing`, gjson.GetBytes(restored, "output.0.arguments").String())
}

func TestAdaptResponsesFunctionToolAliasCollisionsFailOpen(t *testing.T) {
	tests := []struct {
		name    string
		tools   []any
		mapping ResponsesClientToolMapping
	}{
		{
			name:  "provider name already declared",
			tools: []any{map[string]any{"type": "function", "name": "view_image"}, map[string]any{"type": "function", "name": "read_file"}},
		},
		{
			name:  "duplicate client name",
			tools: []any{map[string]any{"type": "function", "name": "view_image"}, map[string]any{"type": "function", "name": "view_image"}},
		},
		{
			name:  "non function client name",
			tools: []any{map[string]any{"type": "function", "name": "view_image"}, map[string]any{"type": "mcp", "name": "view_image"}},
		},
		{
			name:    "custom lowering owns client name",
			tools:   []any{map[string]any{"type": "function", "name": "view_image"}},
			mapping: ResponsesClientToolMapping{CustomTools: map[string]bool{"view_image": true}},
		},
	}
	provider := map[string]any{"type": "function", "name": "read_file"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := map[string]any{"tools": tt.tools}
			before, err := json.Marshal(req)
			require.NoError(t, err)
			mapping := tt.mapping
			require.False(t, AdaptResponsesFunctionToolAlias(req, &mapping, "view_image", provider, "path", "target_file"))
			after, err := json.Marshal(req)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after))
			require.Empty(t, mapping.FunctionAliases)
		})
	}
}

func TestResponsesFunctionToolAliasStreamBuffersInterleavedArguments(t *testing.T) {
	mapping := ResponsesClientToolMapping{FunctionAliases: map[string]ResponsesFunctionToolAlias{
		"read_file": {
			ClientName: "view_image", ProviderName: "read_file",
			ClientArgumentName: "path", ProviderArgumentName: "target_file",
		},
	}}
	restorer := NewResponsesClientToolStreamRestorer(mapping)
	events := []ResponsesStreamEvent{
		{Type: "response.output_item.added", SequenceNumber: 20, OutputIndex: 0, Item: &ResponsesOutput{Type: "function_call", ID: "fc_a", CallID: "call_a", Name: "read_file"}},
		{Type: "response.output_item.added", SequenceNumber: 21, OutputIndex: 1, Item: &ResponsesOutput{Type: "function_call", ID: "fc_b", CallID: "call_b", Name: "read_file"}},
		{Type: "response.function_call_arguments.delta", SequenceNumber: 22, OutputIndex: 0, ItemID: "fc_a", Delta: `{"target_`},
		{Type: "response.function_call_arguments.delta", SequenceNumber: 23, OutputIndex: 1, ItemID: "fc_b", Delta: `{"target_file":"B"}`},
		{Type: "response.function_call_arguments.delta", SequenceNumber: 24, OutputIndex: 0, ItemID: "fc_a", Delta: `file":"A"}`},
		{Type: "response.function_call_arguments.done", SequenceNumber: 25, OutputIndex: 1, ItemID: "fc_b", CallID: "call_b", Name: "read_file"},
		{Type: "response.function_call_arguments.done", SequenceNumber: 26, OutputIndex: 0, ItemID: "fc_a", CallID: "call_a", Name: "read_file"},
		{Type: "response.output_item.done", SequenceNumber: 27, OutputIndex: 0, Item: &ResponsesOutput{Type: "function_call", ID: "fc_a", CallID: "call_a", Name: "read_file", Arguments: `{"target_file":"A"}`}},
	}

	var restored []ResponsesStreamEvent
	for _, event := range events {
		restored = append(restored, restorer.Restore(event)...)
	}
	require.Len(t, restored, 7, "three provider argument deltas are suppressed and two restored delta/done pairs are emitted")
	for index, event := range restored {
		require.Equal(t, 20+index, event.SequenceNumber)
	}
	require.Equal(t, "view_image", restored[0].Item.Name)
	require.Equal(t, "view_image", restored[1].Item.Name)
	require.Equal(t, "response.function_call_arguments.delta", restored[2].Type)
	require.JSONEq(t, `{"path":"B"}`, restored[2].Delta)
	require.Equal(t, "view_image", restored[3].Name)
	require.JSONEq(t, `{"path":"B"}`, restored[3].Arguments)
	require.JSONEq(t, `{"path":"A"}`, restored[4].Delta)
	require.Equal(t, "view_image", restored[5].Name)
	require.Equal(t, "fc_a", restored[6].Item.ID)
	require.Equal(t, "call_a", restored[6].Item.CallID)
	require.Equal(t, "view_image", restored[6].Item.Name)
	require.JSONEq(t, `{"path":"A"}`, restored[6].Item.Arguments)
}

func TestResponsesFunctionToolAliasStreamPreservesMalformedArguments(t *testing.T) {
	mapping := ResponsesClientToolMapping{FunctionAliases: map[string]ResponsesFunctionToolAlias{
		"read_file": {
			ClientName: "view_image", ProviderName: "read_file",
			ClientArgumentName: "path", ProviderArgumentName: "target_file",
		},
	}}
	restorer := NewResponsesClientToolStreamRestorer(mapping)
	require.Len(t, restorer.Restore(ResponsesStreamEvent{
		Type: "response.output_item.added", SequenceNumber: 8, OutputIndex: 0,
		Item: &ResponsesOutput{Type: "function_call", ID: "fc_bad", CallID: "call_bad", Name: "read_file"},
	}), 1)
	require.Empty(t, restorer.Restore(ResponsesStreamEvent{
		Type: "response.function_call_arguments.delta", SequenceNumber: 9, OutputIndex: 0, ItemID: "fc_bad", Delta: "{not-json",
	}))
	restored := restorer.Restore(ResponsesStreamEvent{
		Type: "response.function_call_arguments.done", SequenceNumber: 10, OutputIndex: 0, ItemID: "fc_bad", CallID: "call_bad", Name: "read_file",
	})
	require.Len(t, restored, 2)
	require.Equal(t, "{not-json", restored[0].Delta)
	require.Equal(t, "view_image", restored[1].Name)
	require.Equal(t, "{not-json", restored[1].Arguments)
}
