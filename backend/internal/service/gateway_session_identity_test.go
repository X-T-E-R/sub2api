//go:build unit

package service

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayMetadataSessionID_BareSessionBoundary(t *testing.T) {
	const id = "123e4567-e89b-12d3-a456-426614174000"
	for _, raw := range []string{"session_" + id, "  session_" + strings.ToUpper(id) + "  "} {
		require.Equal(t, id, gatewayMetadataSessionID(raw))
		require.Nil(t, ParseMetadataUserID(raw), "bare sessions must not enter Claude device/account identity rewriting")
	}
	for _, raw := range []string{
		"", id, "static-user-42", "session_customer", "user_session_" + id,
		"session_123e4567e89b12d3a456426614174000", "session_{" + id + "}",
		"session_123e4567-e89b-12d3-a456-42661417400z", "session_" + id + "_extra",
		`{"session_id":"` + id + `"}`, // structured Claude metadata still requires device_id
	} {
		require.Empty(t, gatewayMetadataSessionID(raw), raw)
	}
}

func TestGenerateSessionHash_AllSourcesRetainScope(t *testing.T) {
	svc := &GatewayService{}
	for _, source := range []string{"bare_metadata", "structured_metadata", "header", "cacheable", "messages", "gemini_cli"} {
		t.Run(source, func(t *testing.T) {
			group := int64(11)
			parsed := mustParseSessionHashRequest(t, `{"messages":[{"role":"user","content":"hello"}]}`, &SessionContext{APIKeyID: 21, Platform: PlatformAnthropic})
			parsed.GroupID = &group
			hash := func() string { return svc.GenerateSessionHash(parsed) }
			switch source {
			case "bare_metadata":
				parsed.MetadataUserID = "session_123e4567-e89b-12d3-a456-426614174000"
			case "structured_metadata":
				parsed.MetadataUserID = `{"device_id":"device","session_id":"conversation"}`
			case "header":
				parsed.SessionContext.ClientSessionID = "conversation"
			case "cacheable":
				parsed = mustParseSessionHashRequest(t, `{"system":[{"type":"text","text":"public prefix","cache_control":{"type":"ephemeral"}}]}`, parsed.SessionContext)
				parsed.GroupID = &group
			case "gemini_cli":
				hash = func() string { return svc.GenerateScopedSessionHash(parsed, "gemini_cli", "native-seed") }
			}
			base := hash()
			require.True(t, strings.HasPrefix(base, "v2:"))
			parsed.SessionContext.APIKeyID++
			require.NotEqual(t, base, hash(), "API key isolation")
			parsed.SessionContext.APIKeyID--
			group++
			require.NotEqual(t, base, hash(), "group isolation")
			group--
			parsed.SessionContext.Platform = PlatformAntigravity
			require.NotEqual(t, base, hash(), "target platform isolation")
			parsed.SessionContext.Platform = PlatformAnthropic
			require.Equal(t, base, hash())
		})
	}
}

func TestGenerateSessionHash_ExplicitIdentityPriorityAndSessionSeparation(t *testing.T) {
	svc := &GatewayService{}
	ctx := &SessionContext{APIKeyID: 21, Platform: PlatformAntigravity, ClientSessionID: "header-session"}
	first := mustParseSessionHashRequest(t, `{"metadata":{"user_id":"session_123e4567-e89b-12d3-a456-426614174000"},"messages":[{"role":"user","content":[{"type":"text","text":"S1","cache_control":{"type":"ephemeral"}}]}]}`, ctx)
	base := svc.GenerateSessionHash(first)
	for _, text := range []string{"S2", "S3", "S4", "S4"} {
		next := mustParseSessionHashRequest(t, anthropicSessionBody("same system", []any{msg("user", []any{map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}}})}, first.MetadataUserID), ctx)
		require.Equal(t, base, svc.GenerateSessionHash(next))
	}
	ctx.ClientSessionID = "different header"
	require.Equal(t, base, svc.GenerateSessionHash(first), "structured/bare metadata outranks headers")
	first.MetadataUserID = "session_223e4567-e89b-12d3-a456-426614174000"
	require.NotEqual(t, base, svc.GenerateSessionHash(first), "independent sessions sharing a prompt stay separate")
	first.MetadataUserID = "static-user-42"
	headerHash := svc.GenerateSessionHash(first)
	first.MetadataUserID = ""
	require.Equal(t, headerHash, svc.GenerateSessionHash(first), "arbitrary user ID is not an anchor")
	ctx.ClientSessionID = "different session again"
	require.NotEqual(t, headerHash, svc.GenerateSessionHash(first))
}

func TestGatewaySessionIDFromHeaders_SessionSignalsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, header := range clientSessionIDHeaders {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		c.Request.Header.Set(header, "conversation")
		require.Equal(t, "conversation", GatewaySessionIDFromHeaders(c), header)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	for _, header := range []string{"X-Request-Id", "X-Turn-Id", "X-Message-Id"} {
		c.Request.Header.Set(header, "rotating-id")
	}
	require.Empty(t, GatewaySessionIDFromHeaders(c))
	c.Request.Header.Set("session_id", "bad\x00id")
	require.Empty(t, GatewaySessionIDFromHeaders(c))
	c.Request.Header.Set("session_id", "first")
	c.Request.Header.Set("X-Claude-Code-Session-Id", "second")
	require.Equal(t, "first", GatewaySessionIDFromHeaders(c), "preserve existing header precedence")
}

func TestShortSessionHash_VersionedNamespaceKeepsDigestVisible(t *testing.T) {
	for _, value := range []string{"v2:123456789abc", "gemini:v2:123456789abc", "123456789abc"} {
		require.Equal(t, "12345678", shortSessionHash(value))
	}
	require.Empty(t, shortSessionHash(""))
	require.Equal(t, "short", shortSessionHash("short"))
}
