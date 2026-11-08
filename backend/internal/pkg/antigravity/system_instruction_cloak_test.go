package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultSystemInstructionRewritePhrasesMatchMagpie627fd1a(t *testing.T) {
	want := []string{
		"x-anthropic-billing-header:",
		"built on anthropic's claude agent sdk",
		"you are codex, an agent based on gpt-5",
		"you are codex, a coding agent based on gpt-5",
		"hermes agent, an intelligent ai assistant created by nous research",
	}
	got := DefaultSystemInstructionRewritePhrases()
	require.Equal(t, want, got)

	// Mutating returned slice must not mutate internal default list.
	got[0] = "mutated"
	require.Equal(t, want, DefaultSystemInstructionRewritePhrases())
}

func TestNormalizeAndParseSystemInstructionRewritePhrases(t *testing.T) {
	// Unconfigured / empty raw string falls back to default 5 phrases.
	require.Equal(t, DefaultSystemInstructionRewritePhrases(), ParseStoredSystemInstructionRewritePhrases(""))
	require.Equal(t, DefaultSystemInstructionRewritePhrases(), ParseStoredSystemInstructionRewritePhrases("   "))

	// Explicitly cleared JSON array "[]" stays empty (0 phrases).
	require.Empty(t, ParseStoredSystemInstructionRewritePhrases("[]"))
	require.Equal(t, "[]", FormatStoredSystemInstructionRewritePhrases([]string{}))

	// Normalization trims whitespace, splits newlines, strips embedded U+200B,
	// ignores blank lines, and deduplicates ASCII case-insensitively while preserving order.
	normalized := NormalizeSystemInstructionRewritePhrases([]string{
		"  X-Anthropic-Billing-Header:  \n\n  x-\u200banthropic-billing-header: ",
		"Built on Anthropic's Claude Agent SDK",
		"   ",
		"BUILT ON ANTHROPIC'S CLAUDE AGENT SDK",
		"İstanbul ✓ custom phrase",
	})
	require.Equal(t, []string{
		"X-Anthropic-Billing-Header:",
		"Built on Anthropic's Claude Agent SDK",
		"İstanbul ✓ custom phrase",
	}, normalized)
}

func TestRewriteSystemInstructionText(t *testing.T) {
	defaultPhrases := DefaultSystemInstructionRewritePhrases()

	tests := []struct {
		name    string
		phrases []string
		in      string
		want    string
	}{
		{
			name:    "default 5 phrases case-insensitive exact match",
			phrases: defaultPhrases,
			in: "x-anthropic-billing-header: cc_version=2.1.86\n" +
				"You are a file search specialist, built on Anthropic's Claude Agent SDK.\n" +
				"You are Codex, an agent based on GPT-5.\n" +
				"You are Codex, a coding agent based on GPT-5.4.\n" +
				"You are Hermes Agent, an intelligent AI assistant created by Nous Research.",
			want: "x\u200b-anthropic-billing-header: cc_version=2.1.86\n" +
				"You are a file search specialist, b\u200built on Anthropic's Claude Agent SDK.\n" +
				"Y\u200bou are Codex, an agent based on GPT-5.\n" +
				"Y\u200bou are Codex, a coding agent based on GPT-5.4.\n" +
				"You are H\u200bermes Agent, an intelligent AI assistant created by Nous Research.",
		},
		{
			name:    "ordinary mentions outside exact phrases remain untouched",
			phrases: defaultPhrases,
			in: "You are a general-purpose agent running within the Claude Agent SDK.\n" +
				"You are Codex, based on GPT-5.\n" +
				"You are Hermes Agent, built by Nous Research.",
			want: "You are a general-purpose agent running within the Claude Agent SDK.\n" +
				"You are Codex, based on GPT-5.\n" +
				"You are Hermes Agent, built by Nous Research.",
		},
		{
			name:    "preserves UTF-8 offsets when preceding text has multi-byte runes whose lowercase length differs",
			phrases: defaultPhrases,
			in:      "İstanbul ✓ You are Codex, an agent based on GPT-5.",
			want:    "İstanbul ✓ Y\u200bou are Codex, an agent based on GPT-5.",
		},
		{
			name:    "custom phrase starting with multi-byte Unicode rune inserts U+200B after full first rune",
			phrases: []string{"智能助手 Hermes Agent", "İstanbul Agent"},
			in:      "前缀：智能助手 hermes agent 与 İstanbul agent。",
			want:    "前缀：智\u200b能助手 hermes agent 与 İ\u200bstanbul agent。",
		},
		{
			name:    "overlapping single-byte phrase matches are all rewritten in one pass and strictly idempotent",
			phrases: []string{"aaa"},
			in:      "aaaa and AAAAA",
			want:    "a\u200ba\u200baa and A\u200bA\u200bA\u200bAA",
		},
		{
			name:    "overlapping multi-byte rune phrase matches are all rewritten in one pass and strictly idempotent",
			phrases: []string{"智智智"},
			in:      "智智智智",
			want:    "智\u200b智\u200b智智",
		},
		{
			name:    "empty phrase list leaves text unchanged",
			phrases: []string{},
			in:      "x-anthropic-billing-header: cc_version=2.1.86",
			want:    "x-anthropic-billing-header: cc_version=2.1.86",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteSystemInstructionText(tt.in, tt.phrases)
			require.Equal(t, tt.want, got)
			// Idempotence: applying a second time produces the exact same string.
			require.Equal(t, got, RewriteSystemInstructionText(got, tt.phrases))
		})
	}
}

func TestTransformClaudeToGeminiSystemInstructionRewrite(t *testing.T) {
	claudeReq := &ClaudeRequest{
		Model:     "claude-sonnet-4-5",
		System:    json.RawMessage(`"x-anthropic-billing-header: cc_version=2.1.86\nYou are a helper built on Anthropic's Claude Agent SDK."`),
		MaxTokens: 1024,
		Messages: []ClaudeMessage{
			{
				Role:    "system",
				Content: json.RawMessage(`"You are Codex, an agent based on GPT-5."`),
			},
			{
				Role:    "user",
				Content: json.RawMessage(`"Please explain x-anthropic-billing-header: and built on Anthropic's Claude Agent SDK"`),
			},
		},
	}

	// Default disabled: systemInstruction and user message remain unmodified.
	wireOff, err := TransformClaudeToGemini(claudeReq, "proj-1", "claude-sonnet-4-5")
	require.NoError(t, err)
	require.Contains(t, string(wireOff), "x-anthropic-billing-header: cc_version=2.1.86")
	require.NotContains(t, string(wireOff), "\u200b")
	require.NotContains(t, string(wireOff), `\u200b`)

	// Enabled via TransformOptions with default 5 phrases: only systemInstruction (including hoisted system message) is rewritten;
	// user message content stays untouched.
	opts := DefaultTransformOptions()
	opts.SystemInstructionRewriteEnabled = true
	opts.SystemInstructionRewritePhrases = DefaultSystemInstructionRewritePhrases()
	wireOn, err := TransformClaudeToGeminiWithOptions(claudeReq, "proj-1", "claude-sonnet-4-5", opts)
	require.NoError(t, err)

	var v1 V1InternalRequest
	require.NoError(t, json.Unmarshal(wireOn, &v1))
	require.NotNil(t, v1.Request.SystemInstruction)

	var sysTexts []string
	for _, p := range v1.Request.SystemInstruction.Parts {
		sysTexts = append(sysTexts, p.Text)
	}
	joinedSys := ""
	for _, s := range sysTexts {
		joinedSys += s + "\n"
	}
	require.Contains(t, joinedSys, "x\u200b-anthropic-billing-header: cc_version=2.1.86")
	require.Contains(t, joinedSys, "b\u200built on Anthropic's Claude Agent SDK.")
	require.Contains(t, joinedSys, "Y\u200bou are Codex, an agent based on GPT-5.")

	// User turn must remain untouched.
	require.Len(t, v1.Request.Contents, 1)
	require.Equal(t, "Please explain x-anthropic-billing-header: and built on Anthropic's Claude Agent SDK", v1.Request.Contents[0].Parts[0].Text)
}
