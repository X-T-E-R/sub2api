package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
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
	require.NoError(t, settingSvc.LoadAntigravitySystemInstructionRewriteSettings(context.Background()))

	// 1. Default state: disabled=false, built-in semantic rules returned by GetAntigravitySystemInstructionRewriteConfig.
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
	userPrompt := "User message mentioning x-anthropic-billing-header: and built on Anthropic's Claude Agent SDK\n`You are Codex, an agent based on GPT-5.4`\n```text\nY\u200bou are Codex, a coding agent based on GPT-5\n```"
	wantUser := "User message mentioning x-anthropic-billing-header: and built on Anthropic's Claude Agent SDK\n`You are Codex, an agent built on GPT-5.4`\n```text\nYou are Codex, a coding agent built on GPT-5\n```"

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

	// 2. When switch is OFF (default), outbound systemInstruction has NO U+200B across all 4 protocols,
	// and Chat Completions preserves its original default projection (developer role stays in user contents rather than hoisting).
	offBodies := runFourProtocols("claude-sonnet-4-5")
	for protocol, wire := range offBodies {
		sysText, userText := extractSystemAndUserText(wire)
		require.Contains(t, sysText, "x-anthropic-billing-header: cc_version=2.1.86", "protocol=%s", protocol)
		require.NotContains(t, sysText, "\u200b", "protocol=%s should not insert U+200B when disabled", protocol)
		require.Contains(t, userText, userPrompt, "protocol=%s user prompt must remain intact", protocol)
		if protocol == "chat_completions" {
			require.NotContains(t, sysText, "Hoisted developer: You are Codex, a coding agent based on GPT-5.", "when AGY rewrite is OFF, ChatCompletions preserves default non-hoisted developer projection")
			require.Contains(t, userText, "Hoisted developer: You are Codex, a coding agent based on GPT-5.")
		}
	}

	// 3. Enable switch with built-in semantic rules via UpdateSettingsOmitting -> takes effect on very next request across all 4 protocols (both claude-* and gemini-* models).
	require.NoError(t, settingSvc.UpdateSettingsOmitting(context.Background(), &SystemSettings{
		AntigravitySystemInstructionRewriteEnabled: true,
	}, OmittedSettingKeys{
		SettingKeyAntigravitySystemInstructionRewritePhrases: {},
	}))

	for _, model := range []string{"claude-sonnet-4-5", "gemini-2.5-pro"} {
		onBodies := runFourProtocols(model)
		for protocol, wire := range onBodies {
			sysText, userText := extractSystemAndUserText(wire)
			require.NotContains(t, sysText, "cc_version=", "protocol=%s model=%s", protocol, model)
			require.NotContains(t, sysText, "\u200b")
			require.Contains(t, sysText, "İstanbul ✓ You are a file search specialist, built on the Claude Agent SDK by Anthropic.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "You are Codex, an agent built on GPT-5.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "You are Codex, a coding agent built on GPT-5.4.", "protocol=%s model=%s", protocol, model)
			require.Contains(t, sysText, "You are Hermes Agent, an intelligent AI assistant built by Nous Research.", "protocol=%s model=%s", protocol, model)
			if protocol == "chat_completions" {
				require.Contains(t, sysText, "Hoisted developer: You are Codex, a coding agent built on GPT-5.", "when AGY rewrite is ON, ChatCompletions developer message is hoisted into systemInstruction and rewritten")
				require.NotContains(t, userText, "Hoisted developer:")
			}
			// Ordinary mentions must stay untouched.
			require.Contains(t, sysText, "Ordinary mentions: running within the Claude Agent SDK; You are Codex, based on GPT-5; Hermes Agent, built by Nous Research.", "protocol=%s model=%s", protocol, model)
			// Only @user sentences change, even inside backticks and fences.
			require.Contains(t, userText, wantUser, "protocol=%s model=%s", protocol, model)
			require.NotContains(t, userText, "\u200b", "protocol=%s model=%s", protocol, model)
			require.Equal(t, wire, (&AntigravityGatewayService{settingService: settingSvc}).applyAntigravitySystemInstructionRewrite(context.Background(), wire))
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
		sysText, userText := extractSystemAndUserText(wire)
		require.Contains(t, sysText, "x-anthropic-billing-header: cc_version=2.1.86", "protocol=%s", protocol)
		require.Contains(t, sysText, "based on GPT-5", "protocol=%s", protocol)
		require.Contains(t, userText, userPrompt, "protocol=%s", protocol)
	}
}

type failingOrBlockingRewriteSettingRepo struct {
	antigravityUASettingsRepo
	rewriteGetValueCalls atomic.Int64
	failErr              error
}

func (r *failingOrBlockingRewriteSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyAntigravitySystemInstructionRewriteEnabled || key == SettingKeyAntigravitySystemInstructionRewritePhrases {
		r.rewriteGetValueCalls.Add(1)
		if r.failErr != nil {
			return "", r.failErr
		}
	}
	return r.antigravityUASettingsRepo.GetValue(ctx, key)
}

func TestAntigravitySystemInstructionRewriteRequestPathNeverTouchesSlowOrFailingRepo(t *testing.T) {
	line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}}`
	repo := &failingOrBlockingRewriteSettingRepo{
		antigravityUASettingsRepo: antigravityUASettingsRepo{
			values: map[string]string{
				SettingKeyAntigravitySystemInstructionRewriteEnabled: "true",
				SettingKeyAntigravitySystemInstructionRewritePhrases: `["x-anthropic-billing-header:"]`,
			},
		},
		failErr: errors.New("pg connection timeout"),
	}

	settingSvc := NewSettingService(repo, &config.Config{})

	// 1. Startup load failure logs/returns error, keeps snapshot safely disabled, and does not panic.
	err := settingSvc.LoadAntigravitySystemInstructionRewriteSettings(context.Background())
	require.Error(t, err)
	enabled, phrases := settingSvc.GetAntigravitySystemInstructionRewriteConfig(context.Background())
	require.False(t, enabled)
	require.Equal(t, antigravity.DefaultSystemInstructionRewritePhrases(), phrases)

	// 2. When repository recovers and startup/refresh succeeds, snapshot is populated.
	repo.failErr = nil
	require.NoError(t, settingSvc.LoadAntigravitySystemInstructionRewriteSettings(context.Background()))
	enabled, phrases = settingSvc.GetAntigravitySystemInstructionRewriteConfig(context.Background())
	require.True(t, enabled)
	require.Equal(t, []string{`x-anthropic-billing-header: => ""`}, phrases)

	// 3. Simulate a database outage during live traffic: request-path reads and AGY forwards
	// must read only the in-memory atomic snapshot with ZERO repository GetValue calls for the rewrite settings.
	repo.failErr = errors.New("database is down")
	callsBefore := repo.rewriteGetValueCalls.Load()

	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
	svc := &AntigravityGatewayService{
		settingService: settingSvc,
		tokenProvider:  NewAntigravityTokenProvider(nil, &antigravityCompatTokenCache{token: "agy-test-token"}, nil),
		httpUpstream:   upstream,
	}
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"stream": true,
		"max_tokens": 64,
		"system": "x-anthropic-billing-header: cc_version=2.1.86",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	account := &Account{
		ID:          42,
		Name:        "agy-snapshot-test",
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
	_, err = svc.Forward(context.Background(), c, account, body, false)
	require.NoError(t, err)
	require.Equal(t, callsBefore, repo.rewriteGetValueCalls.Load(), "request path must never query SettingRepository for Antigravity systemInstruction rewrite config")
	require.Len(t, upstream.requestBodies, 1)
	var sysParts []string
	gjson.GetBytes(upstream.requestBodies[0], "request.systemInstruction.parts").ForEach(func(_, part gjson.Result) bool {
		sysParts = append(sysParts, part.Get("text").String())
		return true
	})
	require.NotContains(t, joinWithNewline(sysParts), "cc_version=2.1.86")
	for _, part := range sysParts {
		require.NotEmpty(t, strings.TrimSpace(part))
	}
	_, compiled := settingSvc.GetAntigravitySystemInstructionRewriter()
	require.Same(t, compiled, svc.getClaudeTransformOptions(context.Background()).SystemInstructionRewriter)
	require.Same(t, compiled, svc.getClaudeTransformOptions(context.Background()).SystemInstructionRewriter)
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
