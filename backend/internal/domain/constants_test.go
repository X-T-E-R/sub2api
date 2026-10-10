package domain

import "testing"

func TestDefaultAntigravityModelMapping_CanonicalTargetsAndShortFamilies(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5":                 "claude-fable-5",
		"claude-opus-4-8":                "claude-opus-4-8",
		"claude-sonnet-4-5":              "claude-sonnet-4-5",
		"gemini-2.5-flash-image":         "gemini-2.5-flash-image",
		"gemini-3.1-flash-image":         "gemini-3.1-flash-image",
		AntigravityGemini31ProAgentModel: AntigravityGemini31ProAgentModel,
		"gemini-3.1-pro":                 "gemini-3.1-pro-{effort:low}",
		"gemini-3-pro":                   "gemini-3-pro-{effort:high}",
		"gemini-3.5-flash":               "gemini-3.5-flash-{effort:low}",
		"gemini-3.6-flash":               "gemini-3.6-flash-{effort}",
		"gemini-3.6-flash-tiered":        "gemini-3.6-flash-tiered",
		"gemini-3.8-flash":               "gemini-3.8-flash-{effort}",
	}
	for source, want := range cases {
		if got := DefaultAntigravityModelMapping[source]; got != want {
			t.Fatalf("%s: got %q want %q", source, got, want)
		}
	}
}

func TestDefaultAntigravityModelMapping_NoLegacyAliasesOrRedundantEffortEntries(t *testing.T) {
	for _, source := range []string{"claude-opus-4-5-thinking", "claude-sonnet-4-5-thinking", "claude-sonnet-4-5-20250929", "claude-haiku-4-5", "gemini-3-pro-preview", "gemini-3.1-pro-preview", "gemini-3.1-pro-high", "gemini-3.1-flash-image-preview", "gemini-3-pro-image"} {
		if _, exists := DefaultAntigravityModelMapping[source]; exists {
			t.Fatalf("legacy alias %s must not be seeded", source)
		}
	}
	for _, base := range []string{"gemini-3-pro", "gemini-3.1-pro", "gemini-3.5-flash", "gemini-3.6-flash", "gemini-3.8-flash"} {
		for _, level := range []string{"low", "medium", "high"} {
			if _, exists := DefaultAntigravityModelMapping[base+"-"+level]; exists {
				t.Fatalf("redundant effort key %s-%s", base, level)
			}
		}
	}
}

func TestDefaultBedrockModelMapping_ContainsNewClaudeModels(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"claude-fable-5": "anthropic.claude-fable-5", "claude-opus-4-8": "us.anthropic.claude-opus-4-8-v1"}
	for from, want := range cases {
		got, ok := DefaultBedrockModelMapping[from]
		if !ok {
			t.Fatalf("expected Bedrock mapping for %q", from)
		}
		if got != want {
			t.Fatalf("%q: got %q want %q", from, got, want)
		}
	}
}
