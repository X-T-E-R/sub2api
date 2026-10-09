package service

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
			{Type: "text", Text: "answer", Signature: "sig-text"},
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
		case "message":
			input = append(input, apicompat.ResponsesInputItem{Type: "message", Role: "assistant", Content: mustJSONService(t, item.Content)})
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

func mustJSONService(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func requireAntigravityReplayChain(t *testing.T, req *apicompat.AnthropicRequest) {
	t.Helper()
	var thinkingSignatures, textSignatures, toolSignatures []string
	var sawToolResult bool
	for _, message := range req.Messages {
		var blocks []apicompat.AnthropicContentBlock
		require.NoError(t, json.Unmarshal(message.Content, &blocks))
		for _, block := range blocks {
			switch block.Type {
			case "thinking":
				thinkingSignatures = append(thinkingSignatures, block.Signature)
			case "text":
				textSignatures = append(textSignatures, block.Signature)
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
	require.Equal(t, []string{"sig-text"}, textSignatures)
	require.Equal(t, []string{"sig-tool"}, toolSignatures)
	require.True(t, sawToolResult)
}

func TestAG001AntigravityResponsesNonStreamingToolSignatureRoundTrip(t *testing.T) {
	response := apicompat.AnthropicToResponsesResponseWithOptions(
		signedAntigravityResponse(),
		apicompat.AnthropicToResponsesOptions{PreserveThinkingSignatures: true},
	)
	require.Len(t, response.Output, 4)
	require.NotEmpty(t, response.Output[0].EncryptedContent)
	require.NotEmpty(t, response.Output[1].EncryptedContent)
	require.Equal(t, "message", response.Output[2].Type)
	require.Equal(t, "sig-text", response.Output[2].Content[0].Signature)
	require.Equal(t, "function_call", response.Output[3].Type)
	requireToolCarrierWire(t, response.Output[3].EncryptedContent)

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
	idx0, idx1, idx2, idx3 := 0, 1, 2, 3
	feed(&apicompat.AnthropicStreamEvent{Type: "message_start", Message: &apicompat.AnthropicResponse{ID: "msg_stream_signed"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx0, ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx0, Delta: &apicompat.AnthropicDelta{Type: "thinking_delta", Thinking: "plan"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx0, Delta: &apicompat.AnthropicDelta{Type: "signature_delta", Signature: "sig-thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx0})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx1, ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx1, Delta: &apicompat.AnthropicDelta{Type: "signature_delta", Signature: "sig-detached"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx1})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx2, ContentBlock: &apicompat.AnthropicContentBlock{Type: "text", Signature: "sig-text"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx2, Delta: &apicompat.AnthropicDelta{Type: "text_delta", Text: "answer"}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx2})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &idx3, ContentBlock: &apicompat.AnthropicContentBlock{
		Type: "tool_use", ID: "call_signed", Name: "lookup", Signature: "sig-tool", Input: json.RawMessage(`{}`),
	}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &idx3, Delta: &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: `{"city":"Tokyo"}`}})
	feed(&apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &idx3})
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
	require.Len(t, completed.Output, 4)
	require.Equal(t, "message", completed.Output[2].Type)
	require.Equal(t, "sig-text", completed.Output[2].Content[0].Signature)
	require.Equal(t, "function_call", completed.Output[3].Type)
	require.NotEmpty(t, completed.Output[3].EncryptedContent)
	requireToolCarrierWire(t, completed.Output[3].EncryptedContent)

	replay, err := apicompat.ResponsesToAnthropicRequestWithOptions(
		antigravityResponsesReplayRequest(t, completed.Output),
		apicompat.ResponsesToAnthropicOptions{PreserveThinkingSignatures: true},
	)
	require.NoError(t, err)
	requireAntigravityReplayChain(t, replay)
}

func assertAG001FinalGeminiRequest(t *testing.T, source *apicompat.AnthropicResponse) {
	t.Helper()
	response := apicompat.AnthropicToResponsesResponseWithOptions(
		source,
		apicompat.AnthropicToResponsesOptions{PreserveThinkingSignatures: true},
	)
	replay := antigravityResponsesReplayRequest(t, response.Output)
	replay.Model = "gemini-3.1-pro-high"
	body, err := json.Marshal(replay)
	require.NoError(t, err)

	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/responses", body)
	result, err := svc.ForwardAsResponses(context.Background(), c, newAntigravityCompatAccount(AccountTypeOAuth), body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, upstream.requestBodies, 1)

	request := upstream.requestBodies[0]
	contents := gjson.GetBytes(request, "request.contents").Array()
	require.Len(t, contents, 2, "final Gemini request must include model tool turn and user tool_result")
	require.Equal(t, "model", contents[0].Get("role").String())
	parts := contents[0].Get("parts").Array()
	require.Len(t, parts, 4)
	require.Equal(t, "sig-thinking", parts[0].Get("thoughtSignature").String())
	require.True(t, parts[0].Get("thought").Bool())
	require.Equal(t, "sig-detached", parts[1].Get("thoughtSignature").String())
	require.True(t, parts[1].Get("thought").Bool())
	require.Equal(t, "answer", parts[2].Get("text").String())
	require.Equal(t, "sig-text", parts[2].Get("thoughtSignature").String())
	require.False(t, parts[2].Get("thought").Bool())
	require.Equal(t, "sig-tool", parts[3].Get("thoughtSignature").String())
	require.Equal(t, "lookup", parts[3].Get("functionCall.name").String())
	require.Equal(t, "user", contents[1].Get("role").String())
	require.Equal(t, "lookup", contents[1].Get("parts.0.functionResponse.name").String())
}

func TestAG001AntigravityResponsesReplayReachesFinalGeminiRequest(t *testing.T) {
	assertAG001FinalGeminiRequest(t, signedAntigravityResponse())
}

func TestAG001AntigravityResponsesReplayNormalizesOpaqueToolID(t *testing.T) {
	source := signedAntigravityResponse()
	source.Content[2].ID = "native-id"
	assertAG001FinalGeminiRequest(t, source)
}
