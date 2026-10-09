package antigravity

import (
	"encoding/json"
	"testing"
)

func TestAG002SignedVisibleTextReplayKeepsTargetPart(t *testing.T) {
	resp := NewNonStreamingProcessor().Process(&GeminiResponse{
		Candidates: []GeminiCandidate{{Content: &GeminiContent{Role: "model", Parts: []GeminiPart{{Text: "answer", ThoughtSignature: "sig-text"}}}, FinishReason: "STOP"}},
	}, "resp_signed_text", "gemini-3.8-flash")
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Signature != "sig-text" {
		t.Fatalf("AG-002 source carrier changed: %+v", resp.Content)
	}

	content, err := json.Marshal(resp.Content)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := TransformClaudeToGeminiWithOptions(&ClaudeRequest{
		Model: "gemini-3.8-flash",
		Messages: []ClaudeMessage{
			{Role: "assistant", Content: content},
			{Role: "user", Content: json.RawMessage(`"next"`)},
		},
	}, "project", "gemini-3.8-flash", DefaultTransformOptions())
	if err != nil {
		t.Fatal(err)
	}
	var request V1InternalRequest
	if err := json.Unmarshal(replay, &request); err != nil {
		t.Fatal(err)
	}
	parts := request.Request.Contents[0].Parts
	if len(parts) != 1 || parts[0].Text != "answer" || parts[0].Thought || parts[0].ThoughtSignature != "sig-text" {
		t.Fatalf("AG-002 visible signature moved carrier: %+v", parts)
	}
}

func TestAG002SignedVisibleTextStreamingUsesTextCarrier(t *testing.T) {
	body, err := json.Marshal(V1InternalResponse{Response: GeminiResponse{
		Candidates: []GeminiCandidate{{Content: &GeminiContent{Role: "model", Parts: []GeminiPart{{Text: "answer", ThoughtSignature: "sig-text"}}}, FinishReason: "STOP"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewStreamingProcessor("gemini-3.8-flash")
	events := decodeSSEData(processor.ProcessLine("data: " + string(body)))
	var textCarrier, thoughtCarrier bool
	for _, event := range events {
		if event["type"] != "content_block_start" {
			continue
		}
		block, _ := event["content_block"].(map[string]any)
		if block["type"] == "text" && block["signature"] == "sig-text" {
			textCarrier = true
		}
		if block["type"] == "thinking" && block["signature"] == "sig-text" {
			thoughtCarrier = true
		}
	}
	if !textCarrier || thoughtCarrier {
		t.Fatalf("AG-002 stream carrier target text=%v thought=%v events=%v", textCarrier, thoughtCarrier, events)
	}
}
