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

func TestAG002VisibleTextSignatureRoundTripsThroughResponses(t *testing.T) {
	stop := "end_turn"
	resp := &AnthropicResponse{StopReason: &stop, Content: []AnthropicContentBlock{{Type: "text", Text: "answer", Signature: "sig-text"}}}
	converted := AnthropicToResponsesResponseWithOptions(resp, AnthropicToResponsesOptions{PreserveThinkingSignatures: true})
	require.Len(t, converted.Output, 1)
	require.Equal(t, "message", converted.Output[0].Type)
	require.Equal(t, "sig-text", converted.Output[0].Content[0].Signature)
	raw, err := json.Marshal([]ResponsesInputItem{{Type: "message", Role: "assistant", Content: mustJSON(t, converted.Output[0].Content)}})
	require.NoError(t, err)
	replayed, err := ResponsesToAnthropicRequestWithOptions(&ResponsesRequest{Input: raw}, ResponsesToAnthropicOptions{PreserveThinkingSignatures: true})
	require.NoError(t, err)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(replayed.Messages[0].Content, &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "text", blocks[0].Type)
	require.Equal(t, "answer", blocks[0].Text)
	require.Equal(t, "sig-text", blocks[0].Signature)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
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

func TestAG002StreamingTextCarrierResetsPerBlockAndGuardsGeneric(t *testing.T) {
	feed := func(state *AnthropicEventToResponsesState, signature string, index int) {
		idx := index
		AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_start", Index: &idx, ContentBlock: &AnthropicContentBlock{Type: "text", Text: "part", Signature: signature}}, state)
		AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_delta", Index: &idx, Delta: &AnthropicDelta{Type: "text_delta", Text: "part"}}, state)
		AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "content_block_stop", Index: &idx}, state)
	}
	for _, tc := range []struct {
		name string
		opts AnthropicToResponsesOptions
		want []string
	}{
		{"provider opt-in", AnthropicToResponsesOptions{PreserveThinkingSignatures: true}, []string{"sig-A", "", "sig-C"}},
		{"generic", AnthropicToResponsesOptions{}, []string{"", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := NewAnthropicEventToResponsesStateWithOptions(tc.opts)
			feed(state, "sig-A", 0)
			feed(state, "", 1)
			feed(state, "sig-C", 2)
			events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
			var completed *ResponsesStreamEvent
			for i := range events {
				if events[i].Type == "response.completed" {
					completed = &events[i]
				}
			}
			require.NotNil(t, completed)
			require.Len(t, completed.Response.Output, 1)
			require.Len(t, completed.Response.Output[0].Content, 3)
			got := make([]string, 0, 3)
			for _, part := range completed.Response.Output[0].Content {
				got = append(got, part.Signature)
			}
			require.Equal(t, tc.want, got)
		})
	}
}
