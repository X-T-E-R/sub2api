package service

// Collected content may precede a terminal-only frame and a final usage-only
// frame. Choosing lastWithParts must not discard either authoritative snapshot.
func (c *antigravityThoughtContinuation) reconcileCollectedTerminal(response map[string]any) {
	if c == nil {
		return
	}
	g := c.effectiveGeneration()
	if g == nil {
		return
	}
	if g.rawUsageMetadata != nil {
		usage := make(map[string]any, len(g.rawUsageMetadata))
		for key, value := range g.rawUsageMetadata {
			usage[key] = value
		}
		response["usageMetadata"] = usage
	}
	candidates, _ := response["candidates"].([]any)
	if len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok && g.finishReason != "" {
			candidate["finishReason"] = g.finishReason
		}
	}
}
