package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravitySystemInstructionRewriteAcrossAllFourProtocolsAndHotReload(t *testing.T) {
	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}}`
	repo := &antigravityUASettingsRepo{
		values: map[string]string{},
	}
	settingSvc := NewSettingService(repo, &config.Config{})
	antigravity.SetSystemInstructionRewriteResolver(settingSvc.GetAntigravitySystemInstructionRewriteConfig)
	t.Cleanup(func() {
		antigravity.SetSystemInstructionRewriteResolver(nil)
	})

	// 1. Default state: disabled=false, default 5 phrases returned by GetAntigravitySystemInstructionRewriteConfig.
	enabled, phrases := settingSvc.GetAntigravitySystemInstructionRewriteConfig(context.Background())
	require.False(t, enabled)
	require.Equal(t, antigravity.DefaultSystemInstructionRewritePhrases(), phrases)

	allSettings, err := settingSvc.GetAllSettings(context.Background())
	require.NoError(t, err)
	require.False(t, allSettings.AntigravitySystemInstructionRewriteEnabled)
	require.Equal(t, antigravity.DefaultSystemInstructionRewritePhrases(), allSettings.AntigravitySystemInstructionRewritePhrases)

	systemSeed := "x-anthropic-billing-header: cc_version=2.1.86\n" +
		"İstanbul ✓ You are a file search specialist, built on Anthropic's Claude Agent SDK.\n" +
		"You are Codex, an agent based on GPT-5.\n" +
		"You are Codex, a coding agent based on GPT-5.4.\n" +
		"You are Hermes Agent, an intelligent AI assistant created by Nous Research.\n" +
		"Ordinary mentions: running within the Claude Agent SDK; You are Codex, based on GPT-5; Hermes Agent, built by Nous Research."
	userPrompt := "User message mentioning x-anthropic-billing-header: and built on Anthropic's Claude Agent SDK and You are Codex, an agent based on GPT-5"

	buildProtocolBody := func(protocol string, model string) []byte {
		switch protocol {
		case "messages":
			return []byte(`{
				"model": "` + model + `",
				"stream": true,
				"max_tokens": 64,
				"system": ` + jsonQuote(systemSeed) + `,
				"messages": [
					{"role": "system", "content": "Hoisted system: You are Codex, an agent based on GPT-5."},
					{"role": "user", "content": ` + jsonQuote(userPrompt) + `}
				]
			}`)
		case "chat_completions":
			return []byte(`{
				"model": "` + model + `",
				"stream": true,
				"messages": [
					{"role": "system", "content": ` + jsonQuote(systemSeed) + `},
					{"role": "developer", "content": "Hoisted developer: You are Codex, a coding agent based on GPT-5."},
					{"role": "user", "content": ` + jsonQuote(userPrompt) + `}
				]
			}`)
		case "responses":
			return []byte(`{
				"model": "` + model + `",
				"stream": true,
				"instructions": ` + jsonQuote(systemSeed) + `,
				"input": [
					{"role": "developer", "content": "Hoisted developer: Hermes Agent, an intelligent AI assistant created by Nous Research."},
					{"role": "user", "content": ` + jsonQuote(userPrompt) + `}
				]
			}`)
		case "gemini":
			return []byte(`{
				"systemInstruction": {
					"parts": [
						{"text": ` + jsonQuote(systemSeed) + `}
					]
				},
				"contents": [
					{
						"role": "user",
						"parts": [{"text": ` + jsonQuote(userPrompt) + `}]
					}
				]
			}`)
		default:
			t.Fatalf("unknown protocol %s", protocol)
			return nil
		}
	}

	extractSystemAndUserText := func(wire []byte) (string, string) {
		var sysParts []string
		gjson.GetBytes(wire, "request.systemInstruction.parts").ForEach(func(_, part gjson.Result) bool {
			sysParts = append(sysParts, part.Get("text").String())
			return true
		})
		var userParts []string
		gjson.GetBytes(wire, "request.contents").ForEach(func(_, content gjson.Result) bool {
			content.Get("parts").ForEach(func(_, part gjson.Result) bool {
				userParts = append(userParts, part.Get("text").String())
				return true
			})
			return true
		})
		return joinWithNewline(sysParts), joinWithNewline(userParts)
	}

	runFourProtocols := func(targetModel string) map[string][]byte {
		out := make(map[string][]byte, 4)
		for _, protocol := range []string{"messages", "chat_completions", "responses", "gemini"} {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
			svc := &AntigravityGatewayService{
				settingService: settingSvc,
				tokenProvider:  NewAntigravityTokenProvider(nil, &antigravityCompatTokenCache{token: "agy-test-token"}, nil),
				httpUpstream:   upstream,
			}
			body := buildProtocolBody(protocol, targetModel)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
			account := &Account{
				ID:          42,
				Name:        "agy-rewrite-test",
				Platform:    PlatformAntigravity,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 2,
				Credentials: map[string]any{
					"access_token": "agy-test-token",
					"project_id":   "agy-test-project",
				},
			}
			var err error
			switch protocol {
			case "messages":
				_, err = svc.Forward(context.Background(), c, account, body, false)
			case "chat_completions":
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			case "responses":
				_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
			case "gemini":
				_, err = svc.ForwardGemini(context.Background(), c, account, targetModel, "streamGenerateContent", true, body, false)
			}
			require.NoError(t, err, "protocol=%s model=%s", protocol, targetModel)
			require.Len(t, upstream.requestBodies, 1, "protocol=%s model=%s", protocol, targetModel)
			out[protocol] = upstream.requestBodies[0]
		}
		return out
	}

	// 2. When switch is OFF (default), outbound systemInstruction has NO U+200B across all 4 protocols.
	offBodies := runFourProtocols("claude-sonnet-4-5")
	for protocol, wire := range offBodies {
		sysText, userText := extractSystemAndUserText(wire)
		require.Contains(t, sysText, "x-anthropic-billing-header: cc_version=2.1.86", "protocol=%s", protocol)
		require.NotContains(t, sysText, "\u200b", "protocol=%s should not insert U+200B when disabled", protocol)
		require.Contains(t, userText, userPrompt, "protocol=%s user prompt must remain intact", protocol)
		require.NotContains(t, userText, "\u200b", "protocol=%s user prompt must not contain U+200B", protocol)
	}

	// 3. Enable switch with default 5 phrases via UpdateSettingsOmitting -> takes effect on very next request across all 4 protocols (both claude-* and gemini-* models).
	require.NoError(t, settingSvc.UpdateSettingsOmitting(context.Background(), &SystemSettings{
		AntigravitySystemInstructionRewriteEnabled: true,
	}, OmittedSettingKeys{
		SettingKeyAntigravitySystemInstructionRewritePhrases: {},
	}))

	for _, model := range []string{"claude-sonnet-4-5", "gemini-2.5-pro"} {
		onBodies := runFourProtocols(model)
		for protocol, wire := range onBodies {
			sysText, userText := extractSystemAndUserText(wire)
			require.Contains(t, sysText, "x\u200b-anthropic-billing-header: cc_version=2.1.86", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "İstanbul ✓ You are a file search specialist, b\u200built on Anthropic's Claude Agent SDK.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "Y\u200bou are Codex, an agent based on GPT-5.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "Y\u200bou are Codex, a coding agent based on GPT-5.4.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "You are H\u200bermes Agent, an intelligent AI assistant created by Nous Research.", "protocol=%s model=%s", protocol, model)
			// Ordinary mentions must stay untouched.
			require.Contains(t, sysText, "Ordinary mentions: running within the Claude Agent SDK; You are Codex, based on GPT-5; Hermes Agent, built by Nous Research.", "protocol=%s model=%s", protocol, model)
			// User message content must stay untouched.
			require.Contains(t, userText, userPrompt, "protocol=%s model=%s", protocol, model)
			require.NotContains(t, userText, "\u200b", "protocol=%s model=%s user content must never be rewritten", protocol, model)
			// Raw JSON must not contain double-escaped \\u200b literal text.
			require.NotContains(t, sysText, `\u200b`, "protocol=%s model=%s", protocol, model)
		}
	}

	// 4. Save an explicit empty phrase list [] while keeping switch enabled -> rewrites nothing and reads back [].
	require.NoError(t, settingSvc.UpdateSettings(context.Background(), &SystemSettings{
		AntigravitySystemInstructionRewriteEnabled:    true,
		AntigravitySystemInstructionRewritePhrases:    []string{},
		AntigravitySystemInstructionRewritePhrasesSet: true,
	}))
	enabledAfterClear, phrasesAfterClear := settingSvc.GetAntigravitySystemInstructionRewriteConfig(context.Background())
	require.True(t, enabledAfterClear)
	require.Empty(t, phrasesAfterClear)

	clearedBodies := runFourProtocols("claude-sonnet-4-5")
	for protocol, wire := range clearedBodies {
		sysText, _ := extractSystemAndUserText(wire)
		require.NotContains(t, sysText, "\u200b", "protocol=%s with empty phrase list must not rewrite anything", protocol)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func joinWithNewline(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}
