package antigravity

import "strings"

const (
	terminalFailureSafetyCode            = "antigravity_safety"
	terminalFailureMalformedFunctionCode = "antigravity_malformed_function_call"
	terminalFailureMalformedResponseCode = "antigravity_malformed_response"
	terminalFailureUpstreamCode          = "antigravity_upstream_terminal_error"
)

// TerminalFailureCode maps an explicit Gemini terminal reason to the stable error
// code exposed by the Anthropic-compatible boundary. Empty and successful terminal
// reasons deliberately return no failure code; callers own the no-finish policy.
func TerminalFailureCode(reason string) string {
	switch strings.TrimSpace(reason) {
	case "", "STOP", "MAX_TOKENS", "FINISH_REASON_UNSPECIFIED":
		return ""
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
		return terminalFailureSafetyCode
	case "MALFORMED_FUNCTION_CALL":
		return terminalFailureMalformedFunctionCode
	case "MALFORMED_RESPONSE":
		return terminalFailureMalformedResponseCode
	default:
		return terminalFailureUpstreamCode
	}
}

// TerminalFailureMessage returns a static, payload-free description for a stable
// terminal failure code. It intentionally never includes upstream content or a
// finish reason, since those can contain user prompts, signatures, or tool data.
func TerminalFailureMessage(code string) string {
	switch code {
	case terminalFailureSafetyCode:
		return "The upstream response was blocked by a safety policy."
	case terminalFailureMalformedFunctionCode:
		return "The upstream returned a malformed function call."
	case terminalFailureMalformedResponseCode:
		return "The upstream returned a malformed response."
	case terminalFailureUpstreamCode:
		return "The upstream response terminated with an unexpected terminal reason."
	default:
		return "The upstream response could not be completed."
	}
}

// terminalFailureReason extracts an explicit terminal reason from a response.
// Gemini may return a prompt block without candidates; that shape has no
// candidate finishReason, but is still a safety terminal. Explicit prompt or
// candidate safety blocks take precedence over a candidate finishReason, while
// ordinary STOP/MAX_TOKENS keep their existing semantics.
func terminalFailureReason(geminiResp *GeminiResponse) string {
	if geminiResp == nil {
		return ""
	}

	if promptFeedbackBlocked(geminiResp.PromptFeedback) {
		return "SAFETY"
	}

	if len(geminiResp.Candidates) > 0 {
		candidate := geminiResp.Candidates[0]
		if candidateSafetyBlocked(candidate.SafetyRatings) {
			return "SAFETY"
		}

		reason := strings.TrimSpace(candidate.FinishReason)
		if reason != "" {
			return reason
		}
	}

	return ""
}

func candidateSafetyBlocked(ratings []GeminiSafetyRating) bool {
	for _, rating := range ratings {
		if rating.Blocked {
			return true
		}
	}
	return false
}

func promptFeedbackBlocked(feedback *GeminiPromptFeedback) bool {
	if feedback == nil {
		return false
	}
	blockReason := strings.TrimSpace(feedback.BlockReason)
	return blockReason != "" && blockReason != "BLOCK_REASON_UNSPECIFIED"
}
