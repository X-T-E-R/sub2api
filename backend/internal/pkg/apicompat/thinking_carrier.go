package apicompat

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// anthropicThinkingEnvelopePrefix marks a provider-owned opaque reasoning
// carrier. It is intentionally not accepted as a generic OpenAI encrypted
// reasoning token; Antigravity opts into decoding this envelope explicitly.
const (
	anthropicThinkingEnvelopePrefix       = "anthropic-thinking-v1:"
	antigravityToolSignatureCarrierPrefix = "antigravity-tool-signature-v1:"
)

type antigravityToolSignatureCarrier struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
}

func encodeAntigravityToolSignature(block AnthropicContentBlock) string {
	payload, _ := json.Marshal(antigravityToolSignatureCarrier{
		Type:      "tool_use",
		ID:        block.ID,
		Name:      block.Name,
		Signature: block.Signature,
	})
	return antigravityToolSignatureCarrierPrefix + base64.RawStdEncoding.EncodeToString(payload)
}

func decodeAntigravityToolSignature(encrypted string) (antigravityToolSignatureCarrier, bool) {
	if !strings.HasPrefix(encrypted, antigravityToolSignatureCarrierPrefix) {
		return antigravityToolSignatureCarrier{}, false
	}
	encoded := strings.TrimPrefix(encrypted, antigravityToolSignatureCarrierPrefix)
	payload, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return antigravityToolSignatureCarrier{}, false
	}
	var carrier antigravityToolSignatureCarrier
	if json.Unmarshal(payload, &carrier) != nil || carrier.Type != "tool_use" || carrier.ID == "" || carrier.Signature == "" {
		return antigravityToolSignatureCarrier{}, false
	}
	return carrier, true
}

func encodeAnthropicThinking(block AnthropicContentBlock) string {
	payload, _ := json.Marshal(struct {
		Type      string `json:"type"`
		Thinking  string `json:"thinking"`
		Signature string `json:"signature,omitempty"`
		Data      string `json:"data,omitempty"`
	}{block.Type, block.Thinking, block.Signature, block.Data})
	return anthropicThinkingEnvelopePrefix + base64.RawStdEncoding.EncodeToString(payload)
}
