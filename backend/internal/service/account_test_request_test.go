package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestModelTestSameEffectivePromptAcrossProtocols(t *testing.T) {
	original := "  原词 exactly\n unchanged  "
	anthropic, err := createTestPayload("native", original)
	require.NoError(t, err)
	responses := createOpenAITestPayload("native", false, original)
	chat := createOpenAIChatCompletionsTestPayload("native", original)
	grok := grokTestResponsesPayload("native", original)
	gemini := createGeminiTestPayload("gemini-text", original)
	a, _ := json.Marshal(anthropic)
	r, _ := json.Marshal(responses)
	h, _ := json.Marshal(chat)
	x, _ := json.Marshal(grok)
	require.Equal(t, original, gjson.GetBytes(a, "messages.0.content.0.text").String())
	require.Equal(t, original, gjson.GetBytes(r, "input.0.content.0.text").String())
	require.Equal(t, original, gjson.GetBytes(h, "messages.0.content").String())
	require.Equal(t, original, gjson.GetBytes(x, "input").String())
	require.Equal(t, original, gjson.GetBytes(gemini, "contents.0.parts.0.text").String())
}

func TestModelTestPromptAppliesToNextTextRequestNotMedia(t *testing.T) {
	repo := &codexSessionAffinitySettingRepoStub{}
	settings := NewSettingService(repo, nil)
	svc := &AccountTestService{settingService: settings}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	c, _ := newAntigravityCompatContext("POST", "/test", nil)
	prompt, err := svc.prepareAccountTestRequest(c, account, "gpt-5.4", "", AccountTestModeDefault, AccountTestOptions{})
	require.NoError(t, err)
	require.Equal(t, DefaultModelTestPrompt, prompt)
	_, err = settings.SetModelTestPrompt(context.Background(), "new global words")
	require.NoError(t, err)
	c, _ = newAntigravityCompatContext("POST", "/test", nil)
	prompt, err = svc.prepareAccountTestRequest(c, account, "gpt-5.4", "", AccountTestModeDefault, AccountTestOptions{})
	require.NoError(t, err)
	require.Equal(t, "new global words", prompt)
	for _, input := range []string{"", "media scene"} {
		c, _ = newAntigravityCompatContext("POST", "/test", nil)
		prompt, err = svc.prepareAccountTestRequest(c, account, "gpt-image-2", input, AccountTestModeDefault, AccountTestOptions{})
		require.NoError(t, err)
		require.Equal(t, input, prompt)
	}
	c, _ = newAntigravityCompatContext("POST", "/test", nil)
	_, err = svc.prepareAccountTestRequest(c, account, "gpt-5.4", strings.Repeat("a", 8193), AccountTestModeDefault, AccountTestOptions{})
	require.Error(t, err)
}

func nativeEffortTestAccount(source string) *Account {
	return &Account{Extra: map[string]any{UpstreamModelMetadataExtraKey: map[string]any{"source": "upstream", "models": map[string]any{"client-alias": map[string]any{"id": "client-alias", "supported_reasoning_levels": []string{"low", "high"}, "default_reasoning_level": "low", "reasoning_levels_source": source}}}}}
}

func TestModelTestNativeEffortUsesRequestedCapabilityAndNativeFields(t *testing.T) {
	cases := []struct{ protocol, path, want string }{{"responses", "reasoning.effort", "high"}, {"chat", "reasoning_effort", "high"}, {"anthropic", "output_config.effort", "high"}, {"gemini", "generationConfig.thinkingConfig.thinkingLevel", "HIGH"}}
	for _, tt := range cases {
		t.Run(tt.protocol, func(t *testing.T) {
			c, _ := newAntigravityCompatContext("POST", "/test", nil)
			c.Set(accountTestRequestedKey, "client-alias")
			c.Set(accountTestEffortKey, "high")
			payload := map[string]any{"model": "actual-wire-ID"}
			err := (&AccountTestService{}).applyAccountTestEffort(c, nativeEffortTestAccount("upstream"), payload, tt.protocol)
			require.NoError(t, err)
			body, _ := json.Marshal(payload)
			require.Equal(t, tt.want, gjson.GetBytes(body, tt.path).String())
			require.Equal(t, "actual-wire-ID", payload["model"])
		})
	}
	c, _ := newAntigravityCompatContext("POST", "/test", nil)
	c.Set(accountTestRequestedKey, "client-alias")
	payload := map[string]any{"model": "wire"}
	require.NoError(t, (&AccountTestService{}).applyAccountTestEffort(c, nativeEffortTestAccount("upstream"), payload, "responses"))
	require.NotContains(t, payload, "reasoning")
	v, _ := c.Get(accountTestEffectiveEffortKey)
	require.Equal(t, "low", v)
}

func TestModelTestNativeEffortRejectsUnprovenAndUnknownLevels(t *testing.T) {
	for _, source := range []string{"models.dev", "catalog_suffix", "upstream"} {
		c, _ := newAntigravityCompatContext("POST", "/test", nil)
		c.Set(accountTestRequestedKey, "client-alias")
		effort := "high"
		if source == "upstream" {
			effort = "max"
		}
		c.Set(accountTestEffortKey, effort)
		payload := map[string]any{"model": "wire"}
		require.Error(t, (&AccountTestService{}).applyAccountTestEffort(c, nativeEffortTestAccount(source), payload, "responses"))
		require.NotContains(t, payload, "reasoning")
	}
}
