package apicompat

import (
	"encoding/base64"
	"encoding/json"
)

// anthropicThinkingEnvelopePrefix marks a provider-owned opaque reasoning
// carrier. It is intentionally not accepted as a generic OpenAI encrypted
// reasoning token; Antigravity opts into decoding this envelope explicitly.
const anthropicThinkingEnvelopePrefix = "anthropic-thinking-v1:"

func encodeAnthropicThinking(block AnthropicContentBlock) string {
	payload, _ := json.Marshal(struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature,omitempty"`
		Data      string `json:"data,omitempty"`
	}{block.Type, block.Thinking, block.Signature, block.Data})
	return anthropicThinkingEnvelopePrefix + base64.RawStdEncoding.EncodeToString(payload)
}
