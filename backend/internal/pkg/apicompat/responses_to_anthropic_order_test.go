package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAG003RestoredThinkingAndToolOrderKeepsRelativeBoundaries(t *testing.T) {
	stop := "tool_use"
	response := AnthropicToResponsesResponseWithOptions(&AnthropicResponse{
		Model:      "gemini-3.8-flash",
		StopReason: &stop,
		Content: []AnthropicContentBlock{
			{Type: "thinking", Thinking: "plan-A", Signature: "sig-A"},
			{Type: "tool_use", ID: "call_a", Name: "a", Signature: "sig-tool-A", Input: json.RawMessage(`{}`)},
			{Type: "thinking", Thinking: "plan-B", Signature: "sig-B"},
			{Type: "tool_use", ID: "call_b", Name: "b", Signature: "sig-tool-B", Input: json.RawMessage(`{}`)},
		},
	}, AnthropicToResponsesOptions{PreserveThinkingSignatures: true})
	require.Len(t, response.Output, 4)

	input := make([]ResponsesInputItem, 0, 6)
	for _, item := range response.Output {
		switch item.Type {
		case "reasoning":
			input = append(input, ResponsesInputItem{Type: "reasoning", EncryptedContent: item.EncryptedContent})
		case "function_call":
			input = append(input, ResponsesInputItem{Type: "function_call", CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, EncryptedContent: item.EncryptedContent})
		}
	}
	input = append(input,
		ResponsesInputItem{Type: "function_call_output", CallID: "call_a", Output: "A"},
		ResponsesInputItem{Type: "function_call_output", CallID: "call_b", Output: "B"},
	)
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	req, err := ResponsesToAnthropicRequestWithOptions(&ResponsesRequest{Model: "gemini-3.8-flash", Input: raw}, ResponsesToAnthropicOptions{PreserveThinkingSignatures: true})
	require.NoError(t, err)

	var blocks []AnthropicContentBlock
	for _, message := range req.Messages {
		if message.Role != "assistant" {
			continue
		}
		var messageBlocks []AnthropicContentBlock
		require.NoError(t, json.Unmarshal(message.Content, &messageBlocks))
		blocks = append(blocks, messageBlocks...)
	}
	require.Len(t, blocks, 4)
	require.Equal(t, []string{"thinking", "tool_use", "thinking", "tool_use"}, []string{blocks[0].Type, blocks[1].Type, blocks[2].Type, blocks[3].Type})
	require.Equal(t, "sig-A", blocks[0].Signature)
	require.Equal(t, "call_a", blocks[1].ID)
	require.Equal(t, "sig-tool-A", blocks[1].Signature)
	require.Equal(t, "sig-B", blocks[2].Signature)
	require.Equal(t, "call_b", blocks[3].ID)
	require.Equal(t, "sig-tool-B", blocks[3].Signature)
}
