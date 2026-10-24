package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// outcome is the single payload-free name for what the generation actually
// produced. It never infers a provider terminal from an absent one.
func (g *antigravityThoughtGeneration) outcome() string {
	if g == nil {
		return "no_response"
	}
	if g.readFailed {
		return "read_error"
	}
	if g.hasRefusal {
		return "safety"
	}
	if code := antigravity.TerminalFailureCode(g.finishReason); code != "" {
		return strings.TrimPrefix(code, "antigravity_")
	}
	if g.finishReason == "MAX_TOKENS" {
		return "max_tokens"
	}
	missing := g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED"
	if missing && !g.hasVisibleText && !g.hasToolCall && !g.hasGrounding {
		return "missing_terminal"
	}
	if g.hasToolCall {
		return "tool"
	}
	if g.hasVisibleText {
		return "visible_content"
	}
	if g.hasGrounding {
		return "grounding"
	}
	if g.hasThinking {
		return "thinking_only"
	}
	return "empty"
}

// Read errors, cancellations and transport failures keep their original
// semantics. They only stop the generation from being reported as a clean EOF.
func (c *antigravityThoughtContinuation) markReadFailure() {
	if c != nil && c.generation != nil {
		c.generation.readFailed = true
	}
}
