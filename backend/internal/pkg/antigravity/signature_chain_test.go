package antigravity

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
)

func signatureChainResponse() *GeminiResponse {
	return &GeminiResponse{
		ResponseID: "resp_signature_chain",
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Role: "model", Parts: []GeminiPart{
				{Text: "plan", Thought: true},
				{Text: "", ThoughtSignature: "sig-after-thought"},
				{FunctionCall: &GeminiFunctionCall{
					ID:   "call_signature_chain",
					Name: "lookup",
					Args: map[string]any{"query": "Tokyo"},
				}, ThoughtSignature: "sig-tool"},
			}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &GeminiUsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 7, ThoughtsTokenCount: 3},
	}
}

func decodeSSEData(payload []byte) []map[string]any {
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(string(payload)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func TestSignatureOnlyPartPreservesCarrierAcrossStreamingAndNonStreaming(t *testing.T) {
	resp := signatureChainResponse()

	nonStreaming := NewNonStreamingProcessor().Process(resp, resp.ResponseID, "gemini-3.8-flash")
	if len(nonStreaming.Content) != 3 {
		t.Fatalf("non-stream content blocks = %d, want 3: %+v", len(nonStreaming.Content), nonStreaming.Content)
	}
	if got := nonStreaming.Content[0]; got.Type != "thinking" || got.Thinking != "plan" {
		t.Fatalf("first non-stream block = %+v, want thinking plan", got)
	}
	if got := nonStreaming.Content[1]; got.Type != "thinking" || got.Signature != "sig-after-thought" || got.Thinking != "" {
		t.Fatalf("signature-only carrier = %+v, want empty thinking with sig-after-thought", got)
	}
	if got := nonStreaming.Content[2]; got.Type != "tool_use" || got.Signature != "sig-tool" || got.Name != "lookup" {
		t.Fatalf("tool carrier = %+v, want lookup with sig-tool", got)
	}

	body, err := json.Marshal(V1InternalResponse{Response: *resp, ResponseID: resp.ResponseID})
	if err != nil {
		t.Fatal(err)
	}
	stream := NewStreamingProcessor("gemini-3.8-flash")
	streamEvents := stream.ProcessLine("data: " + string(body))
	finalStream, _ := stream.Finish()
	streamEvents = append(streamEvents, finalStream...)
	decoded := decodeSSEData(streamEvents)
	var sawSignatureCarrier, sawToolCarrier bool
	for _, event := range decoded {
		if event["type"] != "content_block_start" {
			continue
		}
		block, _ := event["content_block"].(map[string]any)
		if block["type"] == "thinking" && block["signature"] == "" {
			// The provider emits an explicit empty thinking carrier before its
			// signature_delta; this is distinct from the initial thought block.
			for _, candidate := range decoded {
				if candidate["type"] == "content_block_delta" {
					delta, _ := candidate["delta"].(map[string]any)
					if delta["type"] == "signature_delta" && delta["signature"] == "sig-after-thought" {
						sawSignatureCarrier = true
					}
				}
			}
		}
		if block["type"] == "tool_use" && block["signature"] == "sig-tool" {
			sawToolCarrier = true
		}
	}
	if !sawSignatureCarrier || !sawToolCarrier {
		t.Fatalf("stream lost signature carriers: signature=%v tool=%v events=%v", sawSignatureCarrier, sawToolCarrier, decoded)
	}

	// Replay the non-stream response through the existing Claude→Gemini request
	// adapter. The real signatures must stay on their own thinking/tool carriers;
	// they must not be collapsed into the preceding thought text.
	assistantContent, err := json.Marshal(nonStreaming.Content)
	if err != nil {
		t.Fatal(err)
	}
	replay := &ClaudeRequest{Model: "gemini-3.8-flash", Messages: []ClaudeMessage{
		{Role: "user", Content: json.RawMessage(`"next"`)},
		{Role: "assistant", Content: assistantContent},
	}}
	replayBody, err := TransformClaudeToGeminiWithOptions(replay, "project", "gemini-3.8-flash", DefaultTransformOptions())
	if err != nil {
		t.Fatal(err)
	}
	var request V1InternalRequest
	if err := json.Unmarshal(replayBody, &request); err != nil {
		t.Fatal(err)
	}
	parts := request.Request.Contents[1].Parts
	if len(parts) != 3 {
		t.Fatalf("replayed model parts = %d, want 3: %+v", len(parts), parts)
	}
	if !parts[0].Thought || parts[0].ThoughtSignature != DummyThoughtSignature {
		t.Fatalf("initial thought carrier = %+v, want dummy signature for unsigned thought", parts[0])
	}
	if !parts[1].Thought || parts[1].Text != "" || parts[1].ThoughtSignature != "sig-after-thought" {
		t.Fatalf("replayed signature carrier = %+v, want empty thought carrier", parts[1])
	}
	if parts[2].FunctionCall == nil || parts[2].ThoughtSignature != "sig-tool" {
		t.Fatalf("replayed tool carrier = %+v, want sig-tool", parts[2])
	}
}
