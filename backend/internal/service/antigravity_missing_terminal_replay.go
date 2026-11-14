package service

// missingTerminalZeroOutput is finalized only at clean EOF, never on a line or a
// read error. A recognized candidate or usage-only response is required: SSE
// comments, [DONE], malformed data and arbitrary empty objects are not responses.
// Signatures alone are carriers, not output; STOP is deliberately excluded.
func (g *antigravityThoughtGeneration) missingTerminalZeroOutput() bool {
	return g != nil && (g.sawResponse || g.rawUsageMetadata != nil) && g.replayPrefixEligible()
}

// malformedFunctionZeroOutput covers an upstream MALFORMED_FUNCTION_CALL that
// completed before emitting any thinking, visible text, tool call, grounding,
// refusal, content carrier, or signature carrier. If a signature carrier was
// already present in the terminal frame, ProcessLine has already closed and
// flushed it onto the SSE wire when finishReason arrived, so only a truly empty
// generation (len(g.tail) == 0) may replay verbatim inside the gateway.
func (g *antigravityThoughtGeneration) malformedFunctionZeroOutput() bool {
	return g != nil && (g.sawResponse || g.rawUsageMetadata != nil) &&
		!g.readFailed && !g.hasUpstreamError &&
		g.finishReason == "MALFORMED_FUNCTION_CALL" &&
		len(g.tail) == 0 &&
		!g.hasContentCarrier && !g.hasThinking && !g.hasVisibleText &&
		!g.hasToolCall && !g.hasGrounding && !g.hasGroundingCarrier && !g.hasRefusal
}

func (g *antigravityThoughtGeneration) replayPrefixEligible() bool {
	return !g.readFailed && !g.hasUpstreamError &&
		(g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED") &&
		!g.hasContentCarrier && !g.hasThinking && !g.hasVisibleText &&
		!g.hasToolCall && !g.hasGrounding && !g.hasGroundingCarrier && !g.hasRefusal
}

func (c *antigravityThoughtContinuation) canReplayMissingTerminal() bool {
	return c != nil && c.retryMissingTerminal && !c.extraUsed &&
		(c.generation.missingTerminalZeroOutput() || c.generation.malformedFunctionZeroOutput())
}

func (c *antigravityThoughtContinuation) missingTerminalReplayUsed() bool {
	return c != nil && c.missingTerminalReplayed
}

// Collected replay failures need an explicit protocol error, not HTTP 200 with
// only a null stop reason. The Go error remains the billable continuation error.
func (c *antigravityThoughtContinuation) collectedResponseFailure() *AntigravityNoUsableAnswerError {
	if c.missingTerminalReplayUsed() && c.terminalErr != nil {
		return &AntigravityNoUsableAnswerError{Stage: c.terminalErr.Stage, Code: "thought_continuation_error"}
	}
	return c.responseFailure()
}
