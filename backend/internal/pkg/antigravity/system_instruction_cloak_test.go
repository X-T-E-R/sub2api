package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDefaultSystemInstructionRewritePhrases(t *testing.T) {
	rules := DefaultSystemInstructionRewritePhrases()
	require.Len(t, rules, 24)
	require.NoError(t, ValidateSystemInstructionRewritePhrases(rules))
	rewriter := NewCompiledSystemInstructionRewriter(rules)
	for _, line := range rules {
		target, replacement, user := splitRewriteRule(line)
		if strings.HasPrefix(target, "re:") {
			continue
		}
		input := target
		if replacement == "" {
			input += " cc_version=dynamic\r\n"
		}
		require.Equal(t, replacement, rewriter.Rewrite(input), line)
		require.Equal(t, replacement, rewriter.Rewrite(rewriter.Rewrite(input)), line)
		if user {
			require.Equal(t, replacement, rewriter.RewriteUser(input))
		} else {
			require.Equal(t, input, rewriter.RewriteUser(input))
		}
	}
	rules[0] = "mutated"
	require.Equal(t, `x-anthropic-billing-header: => ""`, DefaultSystemInstructionRewritePhrases()[0])
}

func TestNormalizeAndParseSystemInstructionRewritePhrases(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json"} {
		require.Equal(t, DefaultSystemInstructionRewritePhrases(), ParseStoredSystemInstructionRewritePhrases(raw))
	}
	require.Equal(t, []string{}, ParseStoredSystemInstructionRewritePhrases("[]"))
	require.Equal(t, "[]", FormatStoredSystemInstructionRewritePhrases(nil))
	rules := NormalizeSystemInstructionRewritePhrases([]string{
		"  X-Anthropic-Billing-Header:  \n\n x-\u200banthropic-billing-header:",
		"BUILT ON ANTHROPIC'S CLAUDE AGENT SDK",
		"You are Codex, an agent based on GPT-5",
		"You are Codex, a coding agent based on GPT-5",
		"Hermes Agent, an intelligent AI assistant created by Nous Research",
		"RFC 2119 keywords", "RFC 2119 keywords:",
		"İstanbul ✓ custom phrase", "Unknown Header:",
		"# ignored", "@system A\u2060gent\tidentity => clean identity",
		`@user re:\D+ => $literal`, `@user re:\d+ => number`,
	})
	require.Equal(t, []string{
		DefaultSystemInstructionRewritePhrases()[0],
		DefaultSystemInstructionRewritePhrases()[2],
		DefaultSystemInstructionRewritePhrases()[3],
		DefaultSystemInstructionRewritePhrases()[4],
		DefaultSystemInstructionRewritePhrases()[5],
		"RFC 2119 keywords: => RFC 2119 key words:",
		`İstanbul ✓ custom phrase => ""`, `Unknown Header: => ""`,
		"Agent identity => clean identity",
		`@user re:\D+ => $literal`, `@user re:\d+ => number`,
	}, rules)
	require.Equal(t, rules, NormalizeSystemInstructionRewritePhrases(rules))
	require.Equal(t, rules, ParseStoredSystemInstructionRewritePhrases(FormatStoredSystemInstructionRewritePhrases(rules)))

	for _, invalid := range []string{`re:[ => bad`, `=> missing`, `re: => missing`, `re:a* => empty match`, "@user => missing target", "@system => missing target"} {
		require.Error(t, ValidateSystemInstructionRewritePhrases([]string{invalid}), invalid)
	}
	require.Equal(t, []string{"valid target => safe wording"}, ParseStoredSystemInstructionRewritePhrases(`["re:[ => bad","valid target => safe wording"]`))
	require.NotPanics(t, func() { NewCompiledSystemInstructionRewriter([]string{`re:[ => bad`}).Rewrite("hello") })
}

func TestCompiledSystemInstructionSemanticRewrite(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	cases := []struct{ in, want string }{
		{"x-anthropic-billing-header: cc_version=2\r\nUseful instructions", "Useful instructions"},
		{"  X-other-billing_header: cc_version=2\nUseful instructions", "Useful instructions"},
		{"  x-anthropic-billing-header: cc_version=2\nUseful instructions", "Useful instructions"},
		{"You are a helper built on ANTHROPIC'S Claude Agent SDK.", "You are a helper built on the Claude Agent SDK by Anthropic."},
		{"İstanbul ✓ You are Codex, an agent based on GPT-5.4.", "İstanbul ✓ You are Codex, an agent built on GPT-5.4."},
		{"You are an AI agent running in OpenCode, a coding agent harness.", "You are an AI coding assistant, a coding agent harness."},
		{"You are Kiki, an interactive general AI agent running on a user's computer.", "Help users directly with software, research, writing, and system tasks on their computer."},
		{"Kiki product documentation is installed under C:/Users/xxoy1/.kiki/docs.", "Local product documentation is installed under C:/Users/xxoy1/.kiki/docs."},
		{"Here <home> means the absolute Kiki home (server host): /home/user/.kiki", "Here <home> means the absolute local home (server host): /home/user/.kiki"},
		{"Read relevant available Kiki skills for product questions; do not consult upstream Kimi Code sites as Kiki documentation.", "Read relevant available local skills for configuration questions rather than consulting external upstream CLI sites."},
		{"# Kimi Code User Instructions\nFollow repo conventions.", "# Workspace Engineering Instructions\nFollow repo conventions."},
		{"You are Claude Code, Anthropic's official CLI for Claude.", "You are an interactive coding assistant."},
		{"You are an expert coding assistant operating inside pi, a coding agent harness.", "You are an expert coding assistant."},
		{"RFC\u00a02119\tkeywords:\r\nMUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.", "RFC 2119 key words: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL."},
		{"You\u2028are\u2029Codex,\van\fagent\tbased\non\r\nGPT-5", "You are Codex, an agent built on GPT-5"},
		{"You are Codex, based on GPT-5. Hermes Agent, built by Nous Research. running within the Claude Agent SDK.", "You are Codex, based on GPT-5. Hermes Agent, built by Nous Research. running within the Claude Agent SDK."},
	}
	for _, tc := range cases {
		got := r.Rewrite(tc.in)
		require.Equal(t, tc.want, got)
		require.Equal(t, got, r.Rewrite(got))
		require.NotContains(t, got, "\u200b")
		require.NotContains(t, got, ".,")
	}
	for _, invisible := range []string{"\u00ad", "\u180e", "\u200b", "\u200c", "\u200d", "\u200e", "\u200f", "\u2060", "\u2061", "\u2062", "\u2063", "\u2064", "\ufeff", "\u034f"} {
		input := invisible + strings.Join(strings.Split("You are Codex, an agent based on GPT-5", ""), invisible) + invisible
		require.Equal(t, "You are Codex, an agent built on GPT-5", r.Rewrite(input), invisible)
	}
	// Existing plugins can put invisible characters between multiple spaces too.
	require.Equal(t, "You are Codex, an agent built on GPT-5", r.Rewrite("You \u200b \t are Codex, an agent based on GPT-5"))
}

func TestCompiledSystemInstructionScopesAndNonCascadingReplacement(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	input := "Translate `You are Codex, an agent based on GPT-5.4`\n```text\nYou are Codex, a coding agent based on GPT-5\n```\nbuilt on Anthropic's Claude Agent SDK"
	want := "Translate `You are Codex, an agent built on GPT-5.4`\n```text\nYou are Codex, a coding agent built on GPT-5\n```\nbuilt on Anthropic's Claude Agent SDK"
	require.Equal(t, want, r.RewriteUser(input))
	require.Equal(t, want, r.RewriteUser(want))
	r = NewCompiledSystemInstructionRewriter([]string{"alpha => beta", "beta => gamma", "alpha extended => precise", `re:price [0-9]+ => $10`})
	require.Equal(t, "beta gamma precise $10", r.Rewrite("alpha beta alpha extended price 100"))
	require.Equal(t, "alpha beta", r.RewriteUser("alpha beta"))
	require.Equal(t, "unchanged", NewCompiledSystemInstructionRewriter(nil).Rewrite("unchanged"))
	require.Equal(t, "safe wording", NewCompiledSystemInstructionRewriter([]string{"@system custom target => safe wording"}).Rewrite("CUSTOM TARGET"))
	require.Equal(t, "safe wording", NewCompiledSystemInstructionRewriter([]string{"@USER\u00a0custom target => safe wording"}).RewriteUser("CUSTOM TARGET"))
	require.Equal(t, "Keep this.\n", NewCompiledSystemInstructionRewriter([]string{"Unknown Header:"}).Rewrite("Unknown Header: dynamic value\nKeep this.\n"))
}

func TestRewriteGeminiContentPruningAndProtectedParts(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	codex := "You are Codex, an agent based on GPT-5"
	protected := []GeminiPart{
		{Text: codex, Thought: true}, {Text: codex, ThoughtSignature: "signed"},
		{Text: codex, InlineData: &GeminiInlineData{Data: "base64"}},
		{Text: codex, FunctionCall: &GeminiFunctionCall{Name: "tool"}},
		{Text: codex, FunctionResponse: &GeminiFunctionResponse{Name: "tool", Response: map[string]any{"text": codex}}},
	}
	content := &GeminiContent{Parts: append([]GeminiPart{{Text: "x-anthropic-billing-header: cc_version=1"}, {Text: codex}}, protected...)}
	RewriteGeminiSystemInstructionContent(&content, true, r)
	require.Len(t, content.Parts, 6)
	require.Equal(t, r.Rewrite(codex), content.Parts[0].Text)
	require.Equal(t, protected, content.Parts[1:])
	contents := []GeminiContent{
		{Role: "user", Parts: append([]GeminiPart{{Text: codex}}, protected...)},
		{Role: "model", Parts: []GeminiPart{{Text: codex}}},
		{Parts: []GeminiPart{{Text: codex}}},
	}
	RewriteGeminiUserContents(contents, true, r)
	require.Equal(t, r.Rewrite(codex), contents[0].Parts[0].Text)
	require.Equal(t, protected, contents[0].Parts[1:])
	require.Equal(t, codex, contents[1].Parts[0].Text)
	require.Equal(t, r.Rewrite(codex), contents[2].Parts[0].Text)
	content = &GeminiContent{Parts: []GeminiPart{{Text: "x-anthropic-billing-header: cc_version=1"}}}
	RewriteGeminiSystemInstructionContent(&content, true, r)
	require.Nil(t, content)
}

func TestRewriteV1InternalWirePruningScopesAndProtectedBytes(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	for _, root := range []string{"", "request"} {
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			input := `{"` + key + `":{"parts":[{"text":"x-anthropic-billing-header: cc_version=1"},{"text":" \t "}]},"contents":[` +
				`{"role":"user","parts":[{"text":"You are Codex, an agent based on GPT-5"},` +
				`{"text":"You are Codex, an agent based on GPT-5","thoughtSignature":"signed"},` +
				`{"text":"You are Codex, an agent based on GPT-5","thought":true},` +
				`{"functionResponse":{"name":"tool","response":{"text":"You are Codex, an agent based on GPT-5"}}}]},` +
				`{"role":"model","parts":[{"text":"You are Codex, an agent based on GPT-5"}]},` +
				`{"parts":[{"text":"You are Codex, a coding agent based on GPT-5"}]}]}`
			prefix := ""
			if root != "" {
				input = `{"request":` + input + `}`
				prefix = root + "."
			}
			wire := []byte(input)
			out := RewriteV1InternalSystemInstructionWire(wire, true, r)
			require.False(t, gjson.GetBytes(out, prefix+key).Exists())
			require.Equal(t, "You are Codex, an agent built on GPT-5", gjson.GetBytes(out, prefix+"contents.0.parts.0.text").String())
			require.Equal(t, "You are Codex, a coding agent built on GPT-5", gjson.GetBytes(out, prefix+"contents.2.parts.0.text").String())
			for _, path := range []string{"contents.0.parts.1", "contents.0.parts.2", "contents.0.parts.3", "contents.1"} {
				require.Equal(t, gjson.GetBytes(wire, prefix+path).Raw, gjson.GetBytes(out, prefix+path).Raw)
			}
			require.Equal(t, out, RewriteV1InternalSystemInstructionWire(out, true, r))
			require.Equal(t, wire, RewriteV1InternalSystemInstructionWire(wire, false, r))
			require.Equal(t, wire, RewriteV1InternalSystemInstructionWire(wire, true, NewCompiledSystemInstructionRewriter(nil)))
		}
	}
	unchanged := []byte(`{ "contents": [ {"role":"user","parts":[{"text":"ordinary Anthropic mention"}]} ] }`)
	require.Same(t, &unchanged[0], &RewriteV1InternalSystemInstructionWire(unchanged, true, r)[0])
}

func TestUserRewriteDeletionAndRawTypedParity(t *testing.T) {
	r := NewCompiledSystemInstructionRewriter([]string{`@user discard => ""`, `@user old identity => new wording`})
	contents := []GeminiContent{
		{Role: "user", Parts: []GeminiPart{{Text: "discard"}}},
		{Role: "user", Parts: []GeminiPart{{Text: "discard"}, {Text: "old identity"}}},
	}
	RewriteGeminiUserContents(contents, true, r)
	require.Equal(t, []GeminiPart{{Text: geminiEmptyUserMessage}}, contents[0].Parts)
	require.Equal(t, []GeminiPart{{Text: "new wording"}}, contents[1].Parts)
	wire := []byte(`{"systemInstruction":{"parts":[{"text":"discard","thought":false}]},"contents":[{"role":"user","parts":[{"text":"discard","thought":false}]},{"role":"user","parts":[{"text":"discard"},{"text":"old identity","thought":false}]}]}`)
	out := RewriteV1InternalSystemInstructionWire(wire, true, r)
	require.False(t, gjson.GetBytes(out, "systemInstruction").Exists())
	require.Equal(t, geminiEmptyUserMessage, gjson.GetBytes(out, "contents.0.parts.0.text").String())
	require.Len(t, gjson.GetBytes(out, "contents.1.parts").Array(), 1)
	require.Equal(t, "new wording", gjson.GetBytes(out, "contents.1.parts.0.text").String())
	require.False(t, gjson.GetBytes(out, "contents.1.parts.0.thought").Bool())
	require.Equal(t, out, RewriteV1InternalSystemInstructionWire(out, true, r))
}

func TestTransformClaudeToGeminiSystemInstructionRewrite(t *testing.T) {
	req := &ClaudeRequest{Model: "claude-sonnet-4-5", MaxTokens: 1024,
		System: json.RawMessage(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=2"},{"type":"text","text":"built on Anthropic's Claude Agent SDK"}]`),
		Messages: []ClaudeMessage{
			{Role: "system", Content: json.RawMessage(`"You are Codex, an agent based on GPT-5"`)},
			{Role: "user", Content: json.RawMessage(`"You are Codex, an agent based on GPT-5.4; built on Anthropic's Claude Agent SDK"`)},
		},
	}
	opts := TransformOptions{SystemInstructionRewriteEnabled: true, SystemInstructionRewriter: NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())}
	wire, err := TransformClaudeToGeminiWithOptions(req, "project", req.Model, opts)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(wire, "request.systemInstruction.parts").Array(), 2)
	require.Equal(t, "built on the Claude Agent SDK by Anthropic", gjson.GetBytes(wire, "request.systemInstruction.parts.0.text").String())
	require.Equal(t, "You are Codex, an agent built on GPT-5", gjson.GetBytes(wire, "request.systemInstruction.parts.1.text").String())
	require.Equal(t, "You are Codex, an agent built on GPT-5.4; built on Anthropic's Claude Agent SDK", gjson.GetBytes(wire, "request.contents.0.parts.0.text").String())
	// Billing-only standalone system blocks must disappear when identity injection is off.
	req.System = json.RawMessage(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=2"}]`)
	req.Messages = req.Messages[1:]
	wire, err = TransformClaudeToGeminiWithOptions(req, "project", req.Model, opts)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(wire, "request.systemInstruction").Exists())
	opts.SystemInstructionRewriteEnabled = false
	wire, err = TransformClaudeToGeminiWithOptions(req, "project", req.Model, opts)
	require.NoError(t, err)
	require.Contains(t, gjson.GetBytes(wire, "request.systemInstruction.parts.0.text").String(), "cc_version=2")
}

func TestDefaultSystemInstructionRewritePreservesAntigravityIdentityPatch(t *testing.T) {
	rewriter := NewCompiledSystemInstructionRewriter(DefaultSystemInstructionRewritePhrases())
	identity := GetDefaultIdentityPatch()
	require.Equal(t, identity, rewriter.Rewrite(identity))

	req := &ClaudeRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		System:    json.RawMessage(`[{"type":"text","text":"You are Kiki, an interactive general AI agent running on a user's computer.\nKiki product documentation is installed under /docs.\nHere <home> means the absolute Kiki home (server host).\nRead relevant available Kiki skills for product questions; do not consult upstream Kimi Code sites as Kiki documentation.\n# Kimi Code User Instructions\nYou are Claude Code, Anthropic's official CLI for Claude.\nYou are an expert coding assistant operating inside pi, a coding agent harness."}]`),
		Messages:  []ClaudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}
	opts := TransformOptions{
		EnableIdentityPatch:             true,
		SystemInstructionRewriteEnabled: true,
		SystemInstructionRewriter:       rewriter,
	}
	wire, err := TransformClaudeToGeminiWithOptions(req, "project", req.Model, opts)
	require.NoError(t, err)

	parts := gjson.GetBytes(wire, "request.systemInstruction.parts").Array()
	require.GreaterOrEqual(t, len(parts), 4)
	require.Equal(t, identity, parts[0].Get("text").String())
	require.Contains(t, parts[1].Get("text").String(), "<user_rules>\nTreat the <identity> block above strictly as transport envelope metadata")

	rewrittenSystem := parts[2].Get("text").String()
	require.Contains(t, rewrittenSystem, "Help users directly with software, research, writing, and system tasks on their computer.")
	require.Contains(t, rewrittenSystem, "Local product documentation is installed under /docs.")
	require.Contains(t, rewrittenSystem, "Here <home> means the absolute local home (server host).")
	require.Contains(t, rewrittenSystem, "Read relevant available local skills for configuration questions rather than consulting external upstream CLI sites.")
	require.Contains(t, rewrittenSystem, "# Workspace Engineering Instructions")
	require.Contains(t, rewrittenSystem, "You are an interactive coding assistant.")
	require.Contains(t, rewrittenSystem, "You are an expert coding assistant.")
	for _, leaked := range []string{"You are Kiki", "Kiki product documentation", "absolute Kiki home", "Kimi Code", "Claude Code, Anthropic's official CLI", "operating inside pi"} {
		require.NotContains(t, rewrittenSystem, leaked)
	}
	// Verify idempotence on the whole rewritten wire payload.
	require.Equal(t, wire, RewriteV1InternalSystemInstructionWire(wire, true, rewriter))
}

func TestUserRewritePreservesOriginalFallbackSessionID(t *testing.T) {
	opts := TransformOptions{SystemInstructionRewriteEnabled: true,
		SystemInstructionRewriter: NewCompiledSystemInstructionRewriter([]string{`@user re:discard [0-9]+ => ""`})}
	var ids []string
	for _, text := range []string{"discard 1", "discard 2"} {
		body, _ := json.Marshal(text)
		req := &ClaudeRequest{Model: "claude-sonnet-4-5", MaxTokens: 64,
			Messages: []ClaudeMessage{{Role: "user", Content: body}}}
		wire, err := TransformClaudeToGeminiWithOptions(req, "project", req.Model, opts)
		require.NoError(t, err)
		id := gjson.GetBytes(wire, "request.sessionId").String()
		require.Equal(t, generateStableSessionID([]GeminiContent{{Role: "user", Parts: []GeminiPart{{Text: text}}}}), id)
		require.Equal(t, geminiEmptyUserMessage, gjson.GetBytes(wire, "request.contents.0.parts.0.text").String())
		ids = append(ids, id)
	}
	require.NotEqual(t, ids[0], ids[1])
}
