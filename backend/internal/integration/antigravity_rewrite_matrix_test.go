package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newAntigravityAccount(id int64, name string, priority int, mapping map[string]any) *service.Account {
	return &service.Account{
		ID:          id,
		Name:        name,
		Platform:    service.PlatformAntigravity,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    priority,
		Concurrency: 4,
		Credentials: map[string]any{
			"access_token":  "oauth-token-" + name,
			"project_id":    "proj-" + name,
			"model_mapping": mapping,
		},
		Extra: map[string]any{},
	}
}

func TestAntigravityMessages_EffortTemplateExpansionMatrix(t *testing.T) {
	cases := []struct {
		name           string
		outputEffort   string
		wantModel      string
		wantNoThinkLvl bool
	}{
		{name: "low", outputEffort: "low", wantModel: "gemini-4-pro-low", wantNoThinkLvl: true},
		{name: "medium", outputEffort: "medium", wantModel: "gemini-4-pro-medium", wantNoThinkLvl: true},
		{name: "high", outputEffort: "high", wantModel: "gemini-4-pro-high", wantNoThinkLvl: true},
		{name: "xhigh", outputEffort: "xhigh", wantModel: "gemini-4-pro-xhigh", wantNoThinkLvl: true},
		{name: "default_when_omitted", outputEffort: "", wantModel: "gemini-4-pro-medium", wantNoThinkLvl: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newAntigravityAccount(101, "ag-effort", 1, map[string]any{
				"gemini-4-pro": "gemini-4-pro-{effort:medium}",
			})
			h := newGatewayHarness(t, gatewayHarnessConfig{
				Accounts: []*service.Account{acc},
				Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
					return antigravityTextSSE("effort-ok", 20, 7, 4), nil
				},
			})

			body := `{"model":"gemini-4-pro","max_tokens":128,"stream":false,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hello"}]}`
			if tc.outputEffort != "" {
				body = `{"model":"gemini-4-pro","max_tokens":128,"stream":false,"thinking":{"type":"adaptive"},"output_config":{"effort":"` + tc.outputEffort + `"},"messages":[{"role":"user","content":"hello"}]}`
			}

			rec := h.post(t, "/v1/messages", nil, body)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Contains(t, rec.Body.String(), "effort-ok")

			reqs := h.requests()
			require.Len(t, reqs, 1)
			require.Equal(t, tc.wantModel, gjson.GetBytes(reqs[0].Body, "model").String())
			require.Equal(t, "proj-ag-effort", gjson.GetBytes(reqs[0].Body, "project").String())
			if tc.wantNoThinkLvl {
				require.False(t, gjson.GetBytes(reqs[0].Body, "request.generationConfig.thinkingConfig.thinkingLevel").Exists(),
					"effort already resolved by template mapping must not also set thinkingLevel")
			}

			logs := h.usageRecords()
			require.Len(t, logs, 1)
			requireUpstreamModel(t, logs[0], tc.wantModel)
			require.Equal(t, 16, logs[0].InputTokens)
			require.Equal(t, 4, logs[0].CacheReadTokens)
			require.Equal(t, 7, logs[0].OutputTokens)
			h.assertSlotsBalanced(t)
		})
	}
}

func TestAntigravityMessages_ThinkingBudgetAndEffortMappingMatrix(t *testing.T) {
	cases := []struct {
		name              string
		mapping           map[string]any
		body              string
		wantModel         string
		wantBudget        int64
		wantThinkingLevel string
		wantMaxOut        int64
	}{
		{
			name:              "enabled_budget_auto_adjusts_max_tokens",
			mapping:           map[string]any{"claude-sonnet-4-6": "claude-sonnet-4-6"},
			body:              `{"model":"claude-sonnet-4-6","max_tokens":2048,"stream":false,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"plan"}]}`,
			wantModel:         "claude-sonnet-4-6",
			wantBudget:        4096,
			wantThinkingLevel: "",
			wantMaxOut:        5096, // budget_tokens + MaxTokensBudgetPadding(1000)
		},
		{
			name:              "adaptive_opus_high_tier_uses_24576_budget",
			mapping:           map[string]any{"claude-opus-4-6": "claude-opus-4-6"},
			body:              `{"model":"claude-opus-4-6","max_tokens":32000,"stream":false,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"deep"}]}`,
			wantModel:         "claude-opus-4-6",
			wantBudget:        antigravity.ClaudeAdaptiveHighThinkingBudgetTokens,
			wantThinkingLevel: "",
			wantMaxOut:        32000,
		},
		{
			name:              "gemini_25_flash_budget_capped_at_24576",
			mapping:           map[string]any{"fast-flash": "gemini-2.5-flash"},
			body:              `{"model":"fast-flash","max_tokens":32000,"stream":false,"thinking":{"type":"enabled","budget_tokens":40000},"messages":[{"role":"user","content":"cap"}]}`,
			wantModel:         "gemini-2.5-flash",
			wantBudget:        antigravity.Gemini25FlashThinkingBudgetLimit,
			wantThinkingLevel: "",
			wantMaxOut:        32000,
		},
		{
			name:              "uncatalogued_literal_model_writes_explicit_thinking_level",
			mapping:           map[string]any{"custom-hydrogen": "gemini-4-hydrogen"},
			body:              `{"model":"custom-hydrogen","max_tokens":4096,"stream":false,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"level"}]}`,
			wantModel:         "gemini-4-hydrogen",
			wantBudget:        0,
			wantThinkingLevel: "high",
			wantMaxOut:        4096,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newAntigravityAccount(102, "ag-think", 1, tc.mapping)
			h := newGatewayHarness(t, gatewayHarnessConfig{
				Accounts: []*service.Account{acc},
				Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
					return antigravityTextSSE("ok", 10, 5, 0), nil
				},
			})

			rec := h.post(t, "/v1/messages", nil, tc.body)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			reqs := h.requests()
			require.Len(t, reqs, 1)
			out := reqs[0].Body
			require.Equal(t, tc.wantModel, gjson.GetBytes(out, "model").String())
			require.True(t, gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts").Bool())
			require.Equal(t, tc.wantMaxOut, gjson.GetBytes(out, "request.generationConfig.maxOutputTokens").Int())
			if tc.wantThinkingLevel != "" {
				require.Equal(t, tc.wantThinkingLevel, gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String())
				require.False(t, gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Exists())
			} else {
				require.Equal(t, tc.wantBudget, gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int())
			}
			h.assertSlotsBalanced(t)
		})
	}
}

func TestAntigravityMessages_MultimodalInlineDataAndToolResultMedia(t *testing.T) {
	t.Run("gemini_3plus_converts_image_video_audio_in_user_and_tool_result", func(t *testing.T) {
		acc := newAntigravityAccount(103, "ag-mm", 1, map[string]any{
			"mm-model": "gemini-4-hydrogen",
		})
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Accounts: []*service.Account{acc},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return antigravityTextSSE("seen-all-media", 50, 12, 0), nil
			},
		})

		body := `{
			"model": "mm-model",
			"max_tokens": 256,
			"stream": false,
			"messages": [
				{"role": "user", "content": [
					{"type": "text", "text": "inspect media"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}
				]},
				{"role": "assistant", "content": [
					{"type": "tool_use", "id": "call-mm-1", "name": "ReadMediaFile", "input": {"path": "clip.mp4"}}
				]},
				{"role": "user", "content": [
					{"type": "tool_result", "tool_use_id": "call-mm-1", "content": [
						{"type": "text", "text": "captured media"},
						{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "+///AA=="}},
						{"type": "audio", "source": {"type": "base64", "media_type": "audio/wav", "data": "UklGRg=="}}
					]},
					{"type": "audio", "source": {"type": "base64", "media_type": "audio/mpeg", "data": "SUQz"}},
					{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "AAAAIGZ0eXA="}}
				]}
			]
		}`

		rec := h.post(t, "/v1/messages", nil, body)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		reqs := h.requests()
		require.Len(t, reqs, 1)
		out := reqs[0].Body

		// Turn 0: user text + image inlineData.
		require.Equal(t, "image/png", gjson.GetBytes(out, "request.contents.0.parts.1.inlineData.mimeType").String())
		require.Equal(t, "iVBORw0KGgo=", gjson.GetBytes(out, "request.contents.0.parts.1.inlineData.data").String())

		// Turn 1: model functionCall gets dummy thoughtSignature on Gemini targets.
		require.Equal(t, "ReadMediaFile", gjson.GetBytes(out, "request.contents.1.parts.0.functionCall.name").String())
		require.Equal(t, antigravity.DummyThoughtSignature, gjson.GetBytes(out, "request.contents.1.parts.0.thoughtSignature").String())

		// Turn 2: functionResponse carries video + audio inlineData in parts, followed by top-level audio + video inlineData.
		require.Equal(t, "captured media", gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.response.result").String())
		require.Equal(t, "video/mp4", gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.parts.0.inlineData.mimeType").String())
		require.Equal(t, "audio/wav", gjson.GetBytes(out, "request.contents.2.parts.0.functionResponse.parts.1.inlineData.mimeType").String())
		require.Equal(t, "audio/mpeg", gjson.GetBytes(out, "request.contents.2.parts.1.inlineData.mimeType").String())
		require.Equal(t, "video/mp4", gjson.GetBytes(out, "request.contents.2.parts.2.inlineData.mimeType").String())
		h.assertSlotsBalanced(t)
	})

	t.Run("gemini_25_rejects_tool_result_video_before_upstream", func(t *testing.T) {
		acc := newAntigravityAccount(104, "ag-mm-25", 1, map[string]any{
			"legacy-model": "gemini-2.5-flash",
		})
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Accounts: []*service.Account{acc},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				t.Fatal("upstream must not be called when Gemini 2.5 tool_result media validation rejects request")
				return nil, nil
			},
		})

		body := `{
			"model": "legacy-model",
			"max_tokens": 128,
			"stream": false,
			"messages": [
				{"role": "assistant", "content": [{"type": "tool_use", "id": "call-1", "name": "ReadMediaFile", "input": {}}]},
				{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call-1", "content": [
					{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "+///AA=="}}
				]}]}
			]
		}`

		rec := h.post(t, "/v1/messages", nil, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "video tool results require a Gemini 3+ final model")
		require.Empty(t, h.requests())
		h.assertSlotsBalanced(t)
	})
}

func TestAntigravityMessages_HeterogeneousToolResultAndUserTextTurnInvariants(t *testing.T) {
	acc := newAntigravityAccount(105, "ag-tool-mix", 1, map[string]any{
		"tool-model": "gemini-4-hydrogen",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("combined-ok", 30, 8, 0), nil
		},
	})

	body := `{
		"model": "tool-model",
		"max_tokens": 256,
		"stream": false,
		"tools": [
			{"name": "lookup", "description": "lookup data", "input_schema": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"], "additionalProperties": false}}
		],
		"messages": [
			{"role": "user", "content": "fetch a and b"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "calling both"},
				{"type": "tool_use", "id": "call-a", "name": "lookup", "input": {"id": "a"}, "signature": "sig-a"},
				{"type": "tool_use", "id": "call-b", "name": "lookup", "input": {"id": "b"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call-a", "content": "alpha"},
				{"type": "tool_result", "tool_use_id": "call-b", "is_error": true, "content": "timeout"},
				{"type": "text", "text": "summarize both outputs and retry failed one"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "bGFzdA=="}}
			]}
		]
	}`

	rec := h.post(t, "/v1/messages", nil, body)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	reqs := h.requests()
	require.Len(t, reqs, 1)
	out := reqs[0].Body

	contents := gjson.GetBytes(out, "request.contents").Array()
	require.Len(t, contents, 3)
	require.Equal(t, "user", contents[0].Get("role").String())
	require.Equal(t, "model", contents[1].Get("role").String())
	require.Equal(t, "user", contents[2].Get("role").String())

	// Model turn retains real signature on call-a and injects dummy signature on unsigned call-b.
	require.Equal(t, "sig-a", contents[1].Get("parts.1.thoughtSignature").String())
	require.Equal(t, antigravity.DummyThoughtSignature, contents[1].Get("parts.2.thoughtSignature").String())

	// Heterogeneous user turn preserves both functionResponses (error keyed under "error"), trailing user text, and image inlineData.
	parts := contents[2].Get("parts").Array()
	require.Len(t, parts, 4)
	require.Equal(t, "call-a", parts[0].Get("functionResponse.id").String())
	require.Equal(t, "lookup", parts[0].Get("functionResponse.name").String())
	require.Equal(t, "alpha", parts[0].Get("functionResponse.response.result").String())
	require.Equal(t, "call-b", parts[1].Get("functionResponse.id").String())
	require.Equal(t, "timeout", parts[1].Get("functionResponse.response.error").String())
	require.Equal(t, "summarize both outputs and retry failed one", parts[2].Get("text").String())
	require.Equal(t, "image/jpeg", parts[3].Get("inlineData.mimeType").String())

	// Schema cleaning stripped additionalProperties.
	require.False(t, gjson.GetBytes(out, "request.tools.0.functionDeclarations.0.parameters.additionalProperties").Exists())
	h.assertSlotsBalanced(t)
}

func TestAntigravityMessages_TwoStageMalformedFunctionCallRecovery(t *testing.T) {
	acc := newAntigravityAccount(106, "ag-malformed", 1, map[string]any{
		"tool-model": "gemini-4-hydrogen",
	})

	malformedPartial := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"planning","thoughtSignature":"sig-1"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	responses := []*http.Response{
		sseHTTPResponse(malformedPartial),     // Req 1: VALIDATED -> MALFORMED_FUNCTION_CALL (count -> 1)
		sseHTTPResponse(malformedPartial),     // Req 2: Stage 1 AUTO -> MALFORMED_FUNCTION_CALL (count -> 2)
		antigravityTextSSE("recovered", 12, 4, 0), // Req 3: Stage 2 AUTO + single-tool recovery note -> STOP (clears state)
		antigravityTextSSE("next-clean", 12, 4, 0), // Req 4: back to VALIDATED without recovery note
	}

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(attempt int, _ capturedUpstreamRequest) (*http.Response, error) {
			return responses[attempt], nil
		},
	})

	headers := map[string]string{"X-Session-Id": "sess-malformed-e2e"}
	reqBody := `{
		"model": "tool-model",
		"max_tokens": 128,
		"stream": true,
		"system": "stable system prompt",
		"tools": [{"name": "lookup", "input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}}],
		"messages": [{"role": "user", "content": "find docs"}]
	}`

	// 1. Initial call -> VALIDATED, upstream returns MALFORMED_FUNCTION_CALL.
	rec1 := h.post(t, "/v1/messages", headers, reqBody)
	require.Equal(t, http.StatusOK, rec1.Code)
	require.Contains(t, rec1.Body.String(), "antigravity_malformed_function_call")

	// 2. Retry 1 -> Stage 1 AUTO with byte-identical cache prefix (tools, systemInstruction, contents).
	rec2 := h.post(t, "/v1/messages", headers, reqBody)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Contains(t, rec2.Body.String(), "antigravity_malformed_function_call")

	// 3. Retry 2 -> Stage 2 AUTO + single-tool system recovery note appended to last user turn.
	rec3 := h.post(t, "/v1/messages", headers, reqBody)
	require.Equal(t, http.StatusOK, rec3.Code)
	require.Contains(t, rec3.Body.String(), "recovered")
	require.Contains(t, rec3.Body.String(), "event: message_stop")

	// 4. Subsequent call -> cache cleared after success, returns to VALIDATED with no recovery note.
	rec4 := h.post(t, "/v1/messages", headers, reqBody)
	require.Equal(t, http.StatusOK, rec4.Code)
	require.Contains(t, rec4.Body.String(), "next-clean")

	reqs := h.requests()
	require.Len(t, reqs, 4)

	const modePath = "request.toolConfig.functionCallingConfig.mode"
	require.Equal(t, "VALIDATED", gjson.GetBytes(reqs[0].Body, modePath).String())
	require.Equal(t, "AUTO", gjson.GetBytes(reqs[1].Body, modePath).String())
	require.Equal(t, "AUTO", gjson.GetBytes(reqs[2].Body, modePath).String())
	require.Equal(t, "VALIDATED", gjson.GetBytes(reqs[3].Body, modePath).String())

	// Stage 1 keeps cache prefix identical to initial call.
	for _, path := range []string{"request.tools", "request.systemInstruction", "request.contents"} {
		require.Equal(t, gjson.GetBytes(reqs[0].Body, path).Raw, gjson.GetBytes(reqs[1].Body, path).Raw, "Stage 1 must keep %s byte-identical", path)
	}
	require.NotContains(t, string(reqs[1].Body), "[System recovery note:")

	// Stage 2 appends the single-tool recovery note to the user turn.
	require.Contains(t, gjson.GetBytes(reqs[2].Body, "request.contents.0.parts.1.text").String(), "[System recovery note:")
	require.Contains(t, gjson.GetBytes(reqs[2].Body, "request.contents.0.parts.1.text").String(), "at most ONE tool call")

	// Request 4 has no recovery note.
	require.NotContains(t, string(reqs[3].Body), "[System recovery note:")
	h.assertSlotsBalanced(t)
}

func TestAntigravityMessages_SoftSafetySubcodeAndSessionStreakGuard(t *testing.T) {
	acc := newAntigravityAccount(107, "ag-safety", 1, map[string]any{
		"safe-model": "gemini-4-hydrogen",
	})

	softSafetyStream := func() *http.Response {
		return sseHTTPResponse(
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"This request was blocked by Gemini's filters."}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}}`,
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":" See https://policies.google.com/terms/generative-"}]}}]}}`,
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ai/use-policy"}]}}]}}`,
		)
	}

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Config: &config.Config{
			RunMode: config.RunModeSimple,
			Gateway: config.GatewayConfig{
				StreamPreheaderKeepaliveEnabled: true,
				StreamKeepaliveInterval:         1,
			},
		},
		Respond: func(attempt int, _ capturedUpstreamRequest) (*http.Response, error) {
			if attempt < 3 {
				return softSafetyStream(), nil
			}
			return antigravityTextSSE("other-session-ok", 10, 4, 0), nil
		},
	})

	reqBody := `{"model":"safe-model","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"blocked topic"}]}`
	flaggedHeaders := map[string]string{"X-Session-Id": "sess-flagged-1"}

	// 3 consecutive soft safety stream cuts emit the explicit subcode and trip the session cooldown.
	for i := 0; i < 3; i++ {
		rec := h.post(t, "/v1/messages", flaggedHeaders, reqBody)
		require.Equal(t, http.StatusOK, rec.Code)
		wire := rec.Body.String()
		require.Contains(t, wire, `"code":"gateway_stream_terminal_error"`)
		require.Contains(t, wire, `"upstream_code":"antigravity_soft_safety_stream_cut"`)
		require.Contains(t, wire, `"retryable":false`)
		require.NotContains(t, wire, "event: message_stop")
	}
	require.Len(t, h.requests(), 3)

	// 4th request on the SAME session is aborted pre-upstream with HTTP 400 and zero upstream call.
	recBlocked := h.post(t, "/v1/messages", flaggedHeaders, reqBody)
	require.Equal(t, http.StatusBadRequest, recBlocked.Code, "body=%s", recBlocked.Body.String())
	require.Equal(t, "antigravity_safety_streak_aborted", gjson.GetBytes(recBlocked.Body.Bytes(), "error.code").String())
	require.False(t, gjson.GetBytes(recBlocked.Body.Bytes(), "error.retryable").Bool())
	require.Len(t, h.requests(), 3, "tripped safety streak guard must abort before calling upstream")

	// A different session on the same API key remains isolated and succeeds.
	recOther := h.post(t, "/v1/messages", map[string]string{"X-Session-Id": "sess-isolated-2"}, reqBody)
	require.Equal(t, http.StatusOK, recOther.Code, "body=%s", recOther.Body.String())
	require.Contains(t, recOther.Body.String(), "other-session-ok")
	require.Len(t, h.requests(), 4)
	h.assertSlotsBalanced(t)
}

func TestAntigravityMessages_MultiAccountFailoverPreservesCleanRequestPerAccount(t *testing.T) {
	acc1 := newAntigravityAccount(201, "ag-primary", 1, map[string]any{
		"shared-alias": "gemini-4-hydrogen-{effort:medium}",
	})
	acc2 := newAntigravityAccount(202, "ag-backup", 2, map[string]any{
		"shared-alias": "gemini-4-neon-{effort:medium}",
	})

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc1, acc2},
		Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
			if req.AccountID == acc1.ID {
				// 429 RATE_LIMIT_EXCEEDED with retryDelay >= 7s triggers immediate model rate limit + account switch.
				return jsonHTTPResponse(http.StatusTooManyRequests, `{
					"error": {
						"code": 429,
						"message": "You have exhausted your capacity on this model. Your quota will reset after 30s.",
						"status": "RESOURCE_EXHAUSTED",
						"details": [
							{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED", "metadata": {"model": "gemini-4-hydrogen-high"}},
							{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "30s"}
						]
					}
				}`), nil
			}
			return antigravityTextSSE("served-by-backup", 25, 9, 5), nil
		},
	})

	body := `{
		"model": "shared-alias",
		"max_tokens": 256,
		"stream": false,
		"thinking": {"type": "adaptive"},
		"output_config": {"effort": "high"},
		"messages": [
			{"role": "user", "content": "step 1"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "prior thought", "signature": "acct1-foreign-sig"},
				{"type": "text", "text": "step 1 reply"}
			]},
			{"role": "user", "content": "step 2"}
		]
	}`

	rec := h.post(t, "/v1/messages", nil, body)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "served-by-backup")

	reqs := h.requests()
	require.Len(t, reqs, 2, "must fail over cleanly from account 1 to account 2")

	// Attempt 1 (account 1): expanded against account 1's template, original signature intact.
	require.Equal(t, acc1.ID, reqs[0].AccountID)
	require.Equal(t, "gemini-4-hydrogen-high", gjson.GetBytes(reqs[0].Body, "model").String())
	require.Equal(t, "proj-ag-primary", gjson.GetBytes(reqs[0].Body, "project").String())
	require.Equal(t, "acct1-foreign-sig", gjson.GetBytes(reqs[0].Body, "request.contents.1.parts.0.thoughtSignature").String())

	// Attempt 2 (account 2): expanded against account 2's template (not corrupted by account 1!),
	// and cross-account thought signature proactively stripped/replaced with dummy sentinel.
	require.Equal(t, acc2.ID, reqs[1].AccountID)
	require.Equal(t, "gemini-4-neon-high", gjson.GetBytes(reqs[1].Body, "model").String())
	require.Equal(t, "proj-ag-backup", gjson.GetBytes(reqs[1].Body, "project").String())
	require.Equal(t, antigravity.DummyThoughtSignature, gjson.GetBytes(reqs[1].Body, "request.contents.1.parts.0.thoughtSignature").String())

	// Usage recorded once against account 2 with account 2's mapped upstream model.
	logs := h.usageRecords()
	require.Len(t, logs, 1)
	require.Equal(t, acc2.ID, logs[0].AccountID)
	requireUpstreamModel(t, logs[0], "gemini-4-neon-high")
	require.Equal(t, 20, logs[0].InputTokens)
	require.Equal(t, 5, logs[0].CacheReadTokens)
	require.Equal(t, 9, logs[0].OutputTokens)
	h.assertSlotsBalanced(t)
}

func TestAntigravityMessages_ModelsAndBasicStreamNonStreamOffline(t *testing.T) {
	acc := newAntigravityAccount(108, "ag-basic", 1, map[string]any{
		"claude-sonnet-4-5": "claude-sonnet-4-5",
		"gemini-3-flash":    "gemini-3-flash",
	})
	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return antigravityTextSSE("hello-offline", 14, 6, 2), nil
		},
	})

	// GET /v1/models
	modelsRec := h.get(t, "/v1/models", nil)
	require.Equal(t, http.StatusOK, modelsRec.Code, "body=%s", modelsRec.Body.String())
	require.Equal(t, "list", gjson.GetBytes(modelsRec.Body.Bytes(), "object").String())

	// POST /v1/messages non-stream and stream
	for _, stream := range []bool{false, true} {
		name := "non_stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			streamVal := "false"
			if stream {
				streamVal = "true"
			}
			rec := h.post(t, "/v1/messages", nil, `{"model":"gemini-3-flash","max_tokens":64,"stream":`+streamVal+`,"messages":[{"role":"user","content":"say hello"}]}`)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			if stream {
				require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"))
				require.Contains(t, rec.Body.String(), "hello-offline")
				require.Contains(t, rec.Body.String(), "event: message_stop")
			} else {
				require.Equal(t, "message", gjson.GetBytes(rec.Body.Bytes(), "type").String())
				require.Equal(t, "hello-offline", gjson.GetBytes(rec.Body.Bytes(), "content.0.text").String())
			}
		})
	}
	h.assertSlotsBalanced(t)
}
