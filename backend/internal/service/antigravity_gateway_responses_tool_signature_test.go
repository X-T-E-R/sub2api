package service

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const antigravityResponsesCarrierModel = "gemini-3.8-flash"

func signedAntigravityResponse() *apicompat.AnthropicResponse {
	stop := "tool_use"
	return &apicompat.AnthropicResponse{
		ID:         "msg_signed_chain",
		Model:      antigravityResponsesCarrierModel,
		StopReason: &stop,
		Content: []apicompat.AnthropicContentBlock{
			{Type: "thinking", Thinking: "plan", Signature: "sig-thinking"},
			{Type: "thinking", Signature: "sig-detached"},
			{Type: "tool_use", ID: "call_signed", Name: "lookup", Signature: "sig-tool", Input: json.RawMessage(`{"city":"Tokyo"}`)},
		},
	}
}

func antigravityResponsesReplayRequest(t *testing.T, outputs []apicompat.ResponsesOutput) *apicompat.ResponsesRequest {
	t.Helper()
	input := make([]apicompat.ResponsesInputItem, 0, len(outputs)+1)
	var callID string
	for _, item := range outputs {
		switch item.Type {
		case "reasoning":
			input = append(input, apicompat.ResponsesInputItem{
				Type:             "reasoning",
				EncryptedContent: item.EncryptedContent,
			})
		case "function_call":
			callID = item.CallID
			input = append(input, apicompat.ResponsesInputItem{
				Type:             "function_call",
				CallID:           item.CallID,
				Name:             item.Name,
				Arguments:        item.Arguments,
				EncryptedContent: item.EncryptedContent,
			})
		}
	}
	input = append(input, apicompat.ResponsesInputItem{
		Type:   "function_call_output",
		CallID: callID,
		Output: "tool result",
	})
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	return &apicompat.ResponsesRequest{Model: antigravityResponsesCarrierModel, Input: raw}
}

func requireAntigravityReplayChain(t *testing.T, req *apicompat.AnthropicRequest) {
	t.Helper()
	var thinkingSignatures, toolSignatures []string
	var sawToolResult bool
	for _, message := range req.Messages {
		var blocks []apicompat.AnthropicContentBlock
		require.NoError(t, json.Unmarshal(message.Content, &blocks))
		for _, block := range blocks {
			switch block.Type {
			case "thinking":
				thinkingSignatures = append(thinkingSignatures, block.Signature)
			case "tool_use":
				toolSignatures = append(toolSignatures, block.Signature)
				require.Equal(t, "call_signed", block.ID)
			case "tool_result":
				sawToolResult = true
				require.Equal(t, "call_signed", block.ToolUseID)
			}
		}
	}
	require.Equal(t, []string{"sig-thinking", "sig-detached"}, thinkingSignatures)
	require.Equal(t, []string{"sig-tool"}, toolSignatures)
	require.True(t, sawToolResult)
}

func TestAG001AntigravityResponsesNonStreamingToolSignatureRoundTrip(t *testing.T) {
	response := apicompat.AnthropicToResponsesResponseWithOptions(
		signedAntigravityResponse(),
		apicompat.AnthropicToResponsesOptions{PreserveThinkingSignatures: true},
	)
	require.Len(t, response.Output, 3)
	require.NotEmpty(t, response.Output[0].EncryptedContent)
	require.NotEmpty(t, response.Output[1].EncryptedContent)
	require.Equal(t, "function_call", response.Output[2].Type)
	requireToolCarrierWire(t, response.Output[2].EncryptedContent)

	replay, err := apicompat.ResponsesToAnthropicRequestWithOptions(
		antigravityResponsesReplayRequest(t, response.Output),
		apicompat.ResponsesToAnthropicOptions{PreserveThinkingSignatures: true},
	)
	require.NoError(t, err)
	requireAntigravityReplayChain(t, replay)
}

func requireToolCarrierWire(t *testing.T, encrypted string) {
	t.Helper()
	const prefix = "antigravity-tool-signature-v1:"
	require.True(t, strings.HasPrefix(encrypted, prefix))
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encrypted, prefix))
	require.NoError(t, err)
	var carrier struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Signature string `json:"signature"`
	}
	require.NoError(t, json.Unmarshal(payload, &carrier))
	require.Equal(t, "tool_use", carrier.Type)
	require.Equal(t, "call_signed", carrier.ID)
	require.Equal(t, "lookup", carrier.Name)
	require.Equal(t, "sig-tool", carrier.Signature)
}

func TestAG001AntigravityResponsesStreamingToolSignatureRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/responses", nil)
	writer := newAntigravityClientWriter(c.Writer, c.Writer, "test")
	adapter := newAntigravityResponsesStreamAdapter(antigravityResponsesCarrierModel)
	feed := func(event *apicompat.AnthropicStreamEvent) {
		adapter.Emit(event, writer)
	}
	idx0, idx1, idx2 := 0, 1, 2
	feed(&apicompat.AnthropicStreamEvent{Type: "message_start", Message: &apicompat.AnthropicResponse{ID: "msg_stream_signed"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx0, ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx0, Delta: &apicompat.AnthropicDelta{Type: "thinking_delta", Thinking: "plan"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx0, Delta: &apicompat.AnthropicDelta{Type: "signature_delta", Signature: "sig-thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx0})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx1, ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx1, Delta: &apicompat.AnthropicDelta{Type: "signature_delta", Signature: "sig-detached"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx1})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx2, ContentBlock: &apicompat.AnthropicContentBlock{
		Type: "tool_use", ID: "call_signed", Name: "lookup", Signature: "sig-tool", Input: json.RawMessage(`{}`),
	}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx2, Delta: &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: `{"city":"Tokyo"}`}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx2})
	feed(&apicompat.AnthropicStreamEvent{Type: "message_stop"})

	var completed *apicompat.ResponsesResponse
	scanner := bufio.NewScanner(strings.NewReader(recorder.Body.String()))
	var eventType string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") || eventType != "response.completed" {
			continue
		}
		var event apicompat.ResponsesStreamEvent
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil && event.Response != nil {
			completed = event.Response
		}
	}
	require.NotNil(t, completed)
	require.Len(t, completed.Output, 3)
	require.Equal(t, "function_call", completed.Output[2].Type)
	require.NotEmpty(t, completed.Output[2].EncryptedContent)
	requireToolCarrierWire(t, completed.Output[2].EncryptedContent)

	replay, err := apicompat.ResponsesToAnthropicRequestWithOptions(
		antigravityResponsesReplayRequest(t, completed.Output),
		apicompat.ResponsesToAnthropicOptions{PreserveThinkingSignatures: true},
	)
	require.NoError(t, err)
	requireAntigravityReplayChain(t, replay)
}
