package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Offline in-process gateway user-journey tests exercising real authentication,
// group assignment enforcement, endpoint routing, and usage recording across
// Anthropic-compatible and OpenAI-compatible gateway surfaces without requiring
// external servers or environment skips.

func TestE2E_GatewayAuthAndGroupEnforcementFlow(t *testing.T) {
	t.Run("missing_and_invalid_api_key_rejected_with_401", func(t *testing.T) {
		acc := newAntigravityAccount(411, "ag-auth-1", 1, map[string]any{
			"gemini-3-flash": "gemini-3-flash",
		})
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Accounts: []*service.Account{acc},
		})

		reqNoAuth := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
		reqNoAuth.Header.Set("Content-Type", "application/json")
		recNoAuth := httptest.NewRecorder()
		h.router.ServeHTTP(recNoAuth, reqNoAuth)
		require.Equal(t, http.StatusUnauthorized, recNoAuth.Code)

		reqBadKey := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
		reqBadKey.Header.Set("Content-Type", "application/json")
		reqBadKey.Header.Set("Authorization", "Bearer sk-invalid-key")
		recBadKey := httptest.NewRecorder()
		h.router.ServeHTTP(recBadKey, reqBadKey)
		require.Equal(t, http.StatusUnauthorized, recBadKey.Code)

		require.Empty(t, h.requests(), "unauthenticated requests must never reach upstream")
		require.Empty(t, h.usageRecords())
		h.assertSlotsBalanced(t)
	})

	t.Run("unassigned_group_api_key_rejected_with_403", func(t *testing.T) {
		acc := newAntigravityAccount(414, "ag-auth-nogroup", 1, map[string]any{
			"gemini-3-flash": "gemini-3-flash",
		})
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Accounts:        []*service.Account{acc},
			UnassignedGroup: true,
		})

		rec := h.post(t, "/v1/messages", nil, `{"model":"gemini-3-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
		require.Empty(t, h.requests(), "unassigned-group API key must be rejected before reaching upstream")
		require.Empty(t, h.usageRecords())
		h.assertSlotsBalanced(t)
	})

	t.Run("valid_key_full_user_journey_models_messages_and_usage", func(t *testing.T) {
		acc := newAntigravityAccount(412, "ag-journey", 1, map[string]any{
			"gemini-3-flash": "gemini-3-flash",
		})
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Accounts: []*service.Account{acc},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return antigravityTextSSE("journey ok", 18, 7, 3), nil
			},
		})

		// Step 1: discover models.
		modelsRec := h.get(t, "/v1/models", nil)
		require.Equal(t, http.StatusOK, modelsRec.Code)
		require.Contains(t, modelsRec.Body.String(), `"object":"list"`)

		// Step 2: invoke /v1/messages with x-api-key header (Anthropic SDK style).
		msgReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
			"model": "gemini-3-flash",
			"max_tokens": 64,
			"stream": false,
			"messages": [{"role": "user", "content": "ping"}]
		}`))
		msgReq.Header.Set("Content-Type", "application/json")
		msgReq.Header.Set("x-api-key", h.apiKey.Key)
		msgRec := httptest.NewRecorder()
		h.router.ServeHTTP(msgRec, msgReq)
		require.Equal(t, http.StatusOK, msgRec.Code, "body=%s", msgRec.Body.String())
		require.Contains(t, msgRec.Body.String(), "journey ok")

		// Step 3: verify usage log recorded accurately for the authenticated user & API key.
		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, h.apiKey.ID, logs[0].APIKeyID)
		require.Equal(t, h.apiKey.UserID, logs[0].UserID)
		require.Equal(t, acc.ID, logs[0].AccountID)
		require.Equal(t, 15, logs[0].InputTokens)
		require.Equal(t, 3, logs[0].CacheReadTokens)
		require.Equal(t, 7, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})

	t.Run("openai_user_journey_bearer_auth_and_usage", func(t *testing.T) {
		acc := newOpenAIAPIKeyAccount(413, "oa-journey", 1, "https://api.openai.com/v1", "sk-oa-journey", map[string]any{
			"gpt-5.4": "gpt-5.4",
		}, true)
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    openAIGroup(413),
			Accounts: []*service.Account{acc},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return jsonHTTPResponse(http.StatusOK, `{
					"id": "resp_oa_journey",
					"object": "response",
					"status": "completed",
					"model": "gpt-5.4",
					"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"openai ok"}]}],
					"usage": {"input_tokens": 25, "output_tokens": 11}
				}`), nil
			},
		})

		rec := h.post(t, "/v1/responses", nil, `{"model":"gpt-5.4","stream":false,"input":"hello"}`)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "openai ok")

		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, acc.ID, logs[0].AccountID)
		require.Equal(t, 25, logs[0].InputTokens)
		require.Equal(t, 11, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})
}
