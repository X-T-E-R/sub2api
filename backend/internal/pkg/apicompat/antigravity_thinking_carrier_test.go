package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAntigravityThinkingCarrierIsExplicitAndRoundTrips(t *testing.T) {
	stop := "end_turn"
	resp := &AnthropicResponse{
		ID:         "msg_carrier",
		Model:      "gemini-3.8-flash",
		StopReason: &stop,
		Content:    []AnthropicContentBlock{{Type: "thinking", Thinking: "plan", Signature: "sig-real"}},
	}

	generic := AnthropicToResponsesResponse(resp)
	require.Len(t, generic.Output, 1)
	require.Empty(t, generic.Output[0].EncryptedContent, "generic conversion must not infer a provider carrier")

	withCarrier := AnthropicToResponsesResponseWithOptions(resp, AnthropicToResponsesOptions{PreserveThinkingSignatures: true})
	require.Len(t, withCarrier.Output, 1)
	require.True(t, strings.HasPrefix(withCarrier.Output[0].EncryptedContent, anthropicThinkingEnvelopePrefix))

	input, err := json.Marshal([]ResponsesInputItem{{
		Type:             "reasoning",
		EncryptedContent: withCarrier.Output[0].EncryptedContent,
	}})
	require.NoError(t, err)
	req := &ResponsesRequest{Model: "gemini-3.8-flash", Input: input}
	converted, err := ResponsesToAnthropicRequestWithOptions(req, ResponsesToAnthropicOptions{PreserveThinkingSignatures: true})
	require.NoError(t, err)
	require.Len(t, converted.Messages, 1)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(converted.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "thinking", blocks[0].Type)
	require.Equal(t, "plan", blocks[0].Thinking)
	require.Equal(t, "sig-real", blocks[0].Signature)

	// An arbitrary OpenAI encrypted_content token remains dropped even on the
	// opt-in path; only the explicit Antigravity envelope is accepted.
	rawInput, err := json.Marshal([]ResponsesInputItem{{Type: "reasoning", EncryptedContent: "gAAAA-openai"}})
	require.NoError(t, err)
	rawReq := &ResponsesRequest{Model: "gemini-3.8-flash", Input: rawInput}
	rawConverted, err := ResponsesToAnthropicRequestWithOptions(rawReq, ResponsesToAnthropicOptions{PreserveThinkingSignatures: true})
	require.NoError(t, err)
	require.Empty(t, rawConverted.Messages)
}

func TestAntigravityThinkingCarrierPreservesStreamingSignatureDelta(t *testing.T) {
	state := NewAnthropicEventToResponsesStateWithOptions(AnthropicToResponsesOptions{PreserveThinkingSignatures: true})
	idx := 0
	feed := func(event *AnthropicStreamEvent) []ResponsesStreamEvent {
		return AnthropicEventToResponsesEvents(event, state)
	}
	feed(&AnthropicStreamEvent{Type: "message_start", Message: &AnthropicResponse{ID: "msg_stream"}})
	feed(&AnthropicStreamEvent{Type: "content_block_start", Index: &idx, ContentBlock: &AnthropicContentBlock{Type: "thinking"}})
	feed(&AnthropicStreamEvent{Type: "content_block_delta", Index: &idx, Delta: &AnthropicDelta{Type: "thinking_delta", Thinking: "plan"}})
	feed(&AnthropicStreamEvent{Type: "content_block_delta", Index: &idx, Delta: &AnthropicDelta{Type: "signature_delta", Signature: "sig-stream"}})
	feed(&AnthropicStreamEvent{Type: "content_block_stop", Index: &idx})
	events := feed(&AnthropicStreamEvent{Type: "message_stop"})

	var completed *ResponsesStreamEvent
	for i := range events {
		if events[i].Type == "response.completed" {
			completed = &events[i]
		}
	}
	require.NotNil(t, completed)
	require.NotNil(t, completed.Response)
	require.Len(t, completed.Response.Output, 1)
	require.True(t, strings.HasPrefix(completed.Response.Output[0].EncryptedContent, anthropicThinkingEnvelopePrefix))
}
