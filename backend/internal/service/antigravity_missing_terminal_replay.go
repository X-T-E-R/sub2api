package service

// missingTerminalZeroOutput is finalized only at clean EOF, never on a line or a
// read error. A recognized candidate or usage-only response is required: SSE
// comments, [DONE], malformed data and arbitrary empty objects are not responses.
// Signatures alone are carriers, not output; STOP is deliberately excluded.
func (g *antigravityThoughtGeneration) missingTerminalZeroOutput() bool {
	return g != nil && (g.sawResponse || g.rawUsageMetadata != nil) && g.replayPrefixEligible()
}

func (g *antigravityThoughtGeneration) replayPrefixEligible() bool {
	return !g.readFailed && !g.hasUpstreamError &&
		(g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED") &&
		!g.hasContentCarrier && !g.hasThinking && !g.hasVisibleText &&
		!g.hasToolCall && !g.hasGrounding && !g.hasGroundingCarrier && !g.hasRefusal
}

func (c *antigravityThoughtContinuation) canReplayMissingTerminal() bool {
	return c != nil && c.retryMissingTerminal && !c.extraUsed && c.generation.missingTerminalZeroOutput()
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
