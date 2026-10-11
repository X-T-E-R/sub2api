package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// legacyAccountTestProbeText is the probe text every test payload used before the
// global model test prompt existed. Payload builders that are called without a
// prompt (CN provider adapters, scheduled usage probes) keep it verbatim so their
// requests are unchanged; callers that do know the effective prompt pass it in.
const legacyAccountTestProbeText = "hi"

// accountTestNativeNonStreamMode labels a probe that reads a complete non-SSE
// upstream body instead of a real stream. It is the wire value of the
// stream_mode metadata the admin test UI renders, so it must stay stable.
const accountTestNativeNonStreamMode = "native_non_stream"

// accountTestPayloadPrompt resolves the text a test payload must carry: the
// prompt the caller passed when it is not blank, otherwise the legacy probe text.
// Resolution lives here so every protocol builder picks the same value.
func accountTestPayloadPrompt(prompt []string) string {
	if len(prompt) > 0 && strings.TrimSpace(prompt[0]) != "" {
		return prompt[0]
	}
	return legacyAccountTestProbeText
}

// grokTestResponsesPayload builds the xAI Responses probe body. It mirrors
// buildGrokQuotaProbeBody (model/input/stream) but carries the effective model
// test prompt instead of the fixed quota probe input.
func grokTestResponsesPayload(model, prompt string) map[string]any {
	model = strings.TrimSpace(model)
	if model == "" {
		model = grokQuotaDefaultModel
	}
	return map[string]any{
		"model":  model,
		"input":  accountTestPayloadPrompt([]string{prompt}),
		"stream": true,
	}
}

// bedrockNativeResponseText extracts the ordinary output text of a Bedrock
// InvokeModel response body.
//
// Bedrock answers with a complete Anthropic message, so an HTTP 200 is not by
// itself a passing probe: a body that never completed (no stop_reason) or that
// carries only reasoning blocks must fail the test instead of being reported as
// a placeholder success. The returned error message is user-facing and is
// reported verbatim.
func bedrockNativeResponseText(body []byte) (string, error) {
	var result struct {
		StopReason *string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if result.StopReason == nil || strings.TrimSpace(*result.StopReason) == "" {
		return "", errors.New("bedrock response is incomplete: no stop_reason")
	}
	var (
		texts       []string
		sawThinking bool
	)
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			// Keep the upstream text verbatim; only blank blocks are dropped.
			if strings.TrimSpace(block.Text) != "" {
				texts = append(texts, block.Text)
			}
		case "thinking", "redacted_thinking":
			sawThinking = true
		}
	}
	if len(texts) == 0 {
		if sawThinking {
			return "", errors.New("bedrock response contained only thinking content, no text")
		}
		return "", errors.New("bedrock response contained no text content")
	}
	return strings.Join(texts, ""), nil
}
