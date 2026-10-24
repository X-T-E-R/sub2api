package service

import (
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Called only after clean EOF. A received candidate with no actual content may
// continue after STOP or an absent/UNSPECIFIED finish reason. Explicit failures,
// MAX_TOKENS, read errors, refusals and actual thinking keep their existing paths.
func (g *antigravityThoughtGeneration) zeroOutputSuccessful() bool {
	return g != nil && g.sawResponse && !g.readFailed &&
		(g.finishReason == antigravitySuccessfulFinishReason || g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED") &&
		!g.hasContentCarrier && !g.hasThinking && !g.hasUpstreamError &&
		!g.hasVisibleText && !g.hasToolCall && !g.hasGrounding && !g.hasRefusal
}

func (c *antigravityThoughtContinuation) zeroOutputContinuationUsed() bool {
	return c != nil && c.extraUsed && c.finishedGeneration.zeroOutputSuccessful()
}

// Thinking flags and signatures alone are protocol carriers, not actual output.
// Non-empty text/tool/modality content excludes replay even if not renderable.
func antigravityPartHasContentCarrier(part gjson.Result) bool {
	if !part.IsObject() {
		return part.Type != gjson.Null
	}
	hasContent := false
	part.ForEach(func(key, value gjson.Result) bool {
		if key.String() == "thought" || key.String() == "thoughtSignature" {
			return true // Protocol carriers alone are not thinking content.
		}
		switch value.Type {
		case gjson.Null:
		case gjson.String:
			hasContent = value.String() != ""
		case gjson.JSON:
			hasContent = len(value.Array()) > 0
			if value.IsObject() {
				hasContent = len(value.Map()) > 0
			}
		default:
			hasContent = true
		}
		return !hasContent
	})
	return hasContent
}

// Preserve the complete prepared upstream envelope. No model turn or empty user
// turn is fabricated; the only added content is one legal non-empty user turn.
func appendAntigravityUserContinue(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "request.contents").IsArray() {
		return nil, errors.New("missing Antigravity request contents")
	}
	return sjson.SetBytes(body, "request.contents.-1", map[string]any{
		"role": "user", "parts": []any{map[string]any{"text": "Continue."}},
	})
}
