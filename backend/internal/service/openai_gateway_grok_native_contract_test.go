//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesSessionHeaderIsProtocolAware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sessionID = "codex-session-opaque"
	responsesBody := []byte(`{"model":"grok-4.5","client_metadata":{"session_id":"body-session","thread_id":"thread-1","turn_id":"turn-1"},"input":"hi"}`)

	responses := newGrokCacheTestContext(7101)
	responses.Request = responses.Request.Clone(context.Background())
	responses.Request.URL.Path = "/v1/responses"
	responses.Request.Header.Set(codexSessionIDHeader, sessionID)
	require.Equal(t, sessionID, codexResponsesSessionID(responses, responsesBody))
	require.Equal(t, sessionID, (&OpenAIGatewayService{}).ExtractSessionID(responses, responsesBody))
	require.Equal(t, sessionID, resolveGrokCacheIdentity(responses, responsesBody, "", "grok-4.5"), "accepted Codex session-id maps directly when prompt_cache_key is absent")

	messages := newGrokCacheTestContext(7101)
	messages.Request.URL.Path = "/v1/messages"
	messages.Request.Header.Set(codexSessionIDHeader, sessionID)
	require.Empty(t, codexResponsesSessionID(messages, responsesBody))
	require.Empty(t, (&OpenAIGatewayService{}).ExtractSessionID(messages, responsesBody), "Codex session-id must not become an Anthropic Messages session channel")

	nonCodexResponses := newGrokCacheTestContext(7101)
	nonCodexResponses.Request.URL.Path = "/v1/responses"
	nonCodexResponses.Request.Header.Set(codexSessionIDHeader, sessionID)
	require.Empty(t, codexResponsesSessionID(nonCodexResponses, []byte(`{"model":"grok-4.5","input":"hi"}`)))
}

func TestClaudeMetadataCompatibilityDoesNotBroadenBareSessionShape(t *testing.T) {
	bare := []byte(`{"model":"grok","metadata":{"user_id":"session_123e4567-e89b-12d3-a456-426614174000"}}`)
	require.Empty(t, extractClaudeCodeSessionIDFromPayload(bare))

	legacy := []byte(`{"metadata":{"user_id":"user_client_account__session_123e4567-e89b-12d3-a456-426614174000"}}`)
	require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", extractClaudeCodeSessionIDFromPayload(legacy))

	structured := []byte(`{"metadata":{"user_id":"{\"session_id\":\"legacy-structured-session\"}"}}`)
	require.Equal(t, "legacy-structured-session", extractClaudeCodeSessionIDFromPayload(structured))
}

func TestGrokNativePromptCacheKeyHasExactPriority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newGrokCacheTestContext(7201)
	c.Request.Header.Set(grokConversationIDHeader, "native-conversation")
	c.Request.Header.Set(grokSessionIDHeader, "native-session")
	c.Request.Header.Set(codexSessionIDHeader, "codex-session")
	body := []byte(`{"model":"grok-4.5","prompt_cache_key":"exact-body-cache-key","client_metadata":{"session_id":"codex-session"},"input":"hi"}`)

	require.Equal(t, "exact-body-cache-key", explicitGrokCacheSeed(c, body, "bridge-key"))
	require.Equal(t, "exact-body-cache-key", resolveGrokCacheIdentity(c, body, "bridge-key", "grok-4.5"))
	require.Equal(t, "exact-body-cache-key", (&OpenAIGatewayService{}).ExtractSessionID(c, body))
}

func TestPrepareGrokMessagesResponsesBodyUsesPostPatchFallbackIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newGrokCacheTestContext(7301)
	intentBody := []byte(`{
		"model":"grok-4.5",
		"instructions":"stable instructions",
		"tools":[{"type":"unsupported_tool","name":"must_not_affect_identity"}],
		"input":[{"role":"user","content":"hello"}]
	}`)

	prePatchIdentity := resolveGrokCacheIdentity(c, intentBody, "", "grok-4.5")
	patchedBody, err := patchGrokResponsesBody(intentBody, "grok-4.5")
	require.NoError(t, err)
	expectedPostPatchIdentity := resolveGrokCacheIdentity(c, patchedBody, "", "grok-4.5")
	require.NotEqual(t, prePatchIdentity, expectedPostPatchIdentity, "unsupported tools must not influence fallback identity")

	finalBody, identity, err := prepareGrokMessagesResponsesBody(c, intentBody, "", "grok-4.5", &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey})
	require.NoError(t, err)
	require.Equal(t, expectedPostPatchIdentity, identity)
	require.Equal(t, identity, gjson.GetBytes(finalBody, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(finalBody, "tools").Exists())
}

func TestApplyGrokNativeRequestHeadersPreservesOpaqueDistinctContracts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newGrokCacheTestContext(7401)
	native := map[string]string{
		grokConversationIDHeader: "conversation-side-call",
		grokSessionIDHeader:      "session-root",
		grokRequestIDHeader:      "request-logical-opaque",
		grokModelOverrideHeader:  "grok-build-model-override",
		grokAgentIDHeader:        "agent-opaque",
		grokTurnIndexHeader:      "17",
		grokDeploymentIDHeader:   "deployment-opaque",
		grokUserIDHeader:         "user-opaque",
	}
	for name, value := range native {
		c.Request.Header.Set(name, value)
	}
	headers := make(http.Header)
	applyGrokNativeRequestHeaders(headers, c, "bridge-fallback", nil)

	for name, value := range native {
		require.Equal(t, value, headers.Get(name), name)
	}
	require.NotEqual(t, headers.Get(grokConversationIDHeader), headers.Get(grokSessionIDHeader))

	rebuilt := make(http.Header)
	applyGrokNativeRequestHeaders(rebuilt, c, "different-fallback", nil)
	require.Equal(t, "request-logical-opaque", rebuilt.Get(grokRequestIDHeader))
}

func TestApplyGrokNativeRequestHeadersFillsOnlyMissingIdentityAndRejectsUnsafeValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newGrokCacheTestContext(7501)
	c.Request.Header[http.CanonicalHeaderKey(grokAgentIDHeader)] = []string{"unsafe\r\nvalue"}
	headers := make(http.Header)
	applyGrokNativeRequestHeaders(headers, c, "bridge-identity", nil)

	require.Equal(t, "bridge-identity", headers.Get(grokConversationIDHeader))
	require.Equal(t, "bridge-identity", headers.Get(grokSessionIDHeader))
	require.Empty(t, headers.Get(grokAgentIDHeader))
	for _, name := range []string{grokModelOverrideHeader, grokTurnIndexHeader, grokDeploymentIDHeader, grokUserIDHeader} {
		require.Empty(t, headers.Get(name), name)
	}
	_, err := uuid.Parse(headers.Get(grokRequestIDHeader))
	require.NoError(t, err)
}

func TestBuildGrokResponsesRequestPreservesCodexResponsesHeadersAndOpenAIProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "unused",
			"base_url": "https://api.x.ai/v1",
		},
	}
	c := newGrokCacheTestContext(7601)
	c.Request.URL.Path = "/v1/responses"
	c.Request.Header.Set(codexSessionIDHeader, "codex-session")
	c.Request.Header.Set(codexThreadIDHeader, "codex-thread")
	c.Request.Header.Set("X-Client-Request-Id", "codex-thread")
	body := []byte(`{"model":"grok-4.5","prompt_cache_key":"exact-cache","client_metadata":{"session_id":"codex-session","thread_id":"codex-thread","turn_id":"turn-1"}}`)

	req, err := buildGrokResponsesRequest(context.Background(), c, account, body, "token", "exact-cache", nil)
	require.NoError(t, err)
	require.Equal(t, "exact-cache", req.Header.Get(grokConversationIDHeader))
	require.Equal(t, "exact-cache", req.Header.Get(grokSessionIDHeader))
	require.Equal(t, "codex-session", req.Header.Get(codexSessionIDHeader))
	require.Equal(t, "codex-thread", req.Header.Get(codexThreadIDHeader))
	require.Equal(t, "codex-thread", req.Header.Get("X-Client-Request-Id"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
}
