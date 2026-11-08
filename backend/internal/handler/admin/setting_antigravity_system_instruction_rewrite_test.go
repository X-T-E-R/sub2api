package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravitySystemInstructionRewriteSettingsAPI(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	antigravity.SetSystemInstructionRewriteResolver(h.settingService.GetAntigravitySystemInstructionRewriteConfig)
	t.Cleanup(func() {
		antigravity.SetSystemInstructionRewriteResolver(nil)
	})

	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
		h.GetSettings(c)
		return r
	}

	// 1. Default GET returns enabled=false and the default 5 Magpie phrases.
	r := get()
	require.Equal(t, http.StatusOK, r.Code)
	require.False(t, gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_enabled").Bool())
	defaultPhrases := antigravity.DefaultSystemInstructionRewritePhrases()
	var gotPhrases []string
	gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_phrases").ForEach(func(_, v gjson.Result) bool {
		gotPhrases = append(gotPhrases, v.String())
		return true
	})
	require.Equal(t, defaultPhrases, gotPhrases)

	// 2. PUT enabling switch and customizing phrases normalizes, deduplicates, persists, and updates runtime resolver immediately.
	r = doUpdateSettings(t, h, map[string]any{
		"antigravity_system_instruction_rewrite_enabled": true,
		"antigravity_system_instruction_rewrite_phrases": []string{
			"  x-anthropic-billing-header:  ",
			"",
			"X-ANTHROPIC-BILLING-HEADER:",
			"İstanbul ✓ custom phrase\n  Second Custom Phrase  ",
		},
	}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.True(t, gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_enabled").Bool())

	expectedCustom := []string{
		"x-anthropic-billing-header:",
		"İstanbul ✓ custom phrase",
		"Second Custom Phrase",
	}
	var updatedPhrases []string
	gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_phrases").ForEach(func(_, v gjson.Result) bool {
		updatedPhrases = append(updatedPhrases, v.String())
		return true
	})
	require.Equal(t, expectedCustom, updatedPhrases)
	require.Equal(t, "true", repo.values[service.SettingKeyAntigravitySystemInstructionRewriteEnabled])
	require.JSONEq(t, `["x-anthropic-billing-header:","İstanbul ✓ custom phrase","Second Custom Phrase"]`, repo.values[service.SettingKeyAntigravitySystemInstructionRewritePhrases])

	rtEnabled, rtPhrases := antigravity.GetSystemInstructionRewriteConfigForContext(context.Background())
	require.True(t, rtEnabled)
	require.Equal(t, expectedCustom, rtPhrases)

	// 3. Partial PUT omitting rewrite fields preserves both enabled and phrase list.
	r = doUpdateSettings(t, h, map[string]any{"antigravity_user_agent_version": "2.9.1"}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.NotContains(t, repo.lastUpdates, service.SettingKeyAntigravitySystemInstructionRewriteEnabled)
	require.NotContains(t, repo.lastUpdates, service.SettingKeyAntigravitySystemInstructionRewritePhrases)
	require.True(t, gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_enabled").Bool())

	// 4. Turning off the toggle while omitting phrases preserves the custom phrase list.
	r = doUpdateSettings(t, h, map[string]any{
		"antigravity_system_instruction_rewrite_enabled": false,
	}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.False(t, gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_enabled").Bool())
	var preservedPhrases []string
	gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_phrases").ForEach(func(_, v gjson.Result) bool {
		preservedPhrases = append(preservedPhrases, v.String())
		return true
	})
	require.Equal(t, expectedCustom, preservedPhrases)

	// 5. Explicitly clearing the phrase list ([]string{}) stores "[]" and reads back empty array [] (not default 5).
	r = doUpdateSettings(t, h, map[string]any{
		"antigravity_system_instruction_rewrite_enabled": true,
		"antigravity_system_instruction_rewrite_phrases": []string{},
	}, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, "[]", repo.values[service.SettingKeyAntigravitySystemInstructionRewritePhrases])
	phrasesArray := gjson.Get(r.Body.String(), "data.antigravity_system_instruction_rewrite_phrases")
	require.True(t, phrasesArray.IsArray())
	require.Empty(t, phrasesArray.Array())

	rGetAfterClear := get()
	require.Equal(t, http.StatusOK, rGetAfterClear.Code)
	require.True(t, gjson.Get(rGetAfterClear.Body.String(), "data.antigravity_system_instruction_rewrite_enabled").Bool())
	require.Empty(t, gjson.Get(rGetAfterClear.Body.String(), "data.antigravity_system_instruction_rewrite_phrases").Array())
}

func TestAntigravitySystemInstructionRewriteAuditChange(t *testing.T) {
	before := &service.SystemSettings{
		AntigravitySystemInstructionRewriteEnabled: false,
		AntigravitySystemInstructionRewritePhrases: []string{"a"},
	}
	after := &service.SystemSettings{
		AntigravitySystemInstructionRewriteEnabled: true,
		AntigravitySystemInstructionRewritePhrases: []string{"a", "b"},
	}
	changed := diffSettings(before, after, nil, nil, UpdateSettingsRequest{})
	require.Contains(t, changed, "antigravity_system_instruction_rewrite_enabled")
	require.Contains(t, changed, "antigravity_system_instruction_rewrite_phrases")
	require.NotContains(t, diffSettings(after, after, nil, nil, UpdateSettingsRequest{}), "antigravity_system_instruction_rewrite_enabled")
	require.NotContains(t, diffSettings(after, after, nil, nil, UpdateSettingsRequest{}), "antigravity_system_instruction_rewrite_phrases")
}
