package service

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// GatewaySessionIDFromHeaders explicitly opts the generic Messages, Chat,
// Responses and native Gemini ingress into the existing session-scoped headers.
// Request/turn IDs and prompt-cache keys are not conversation identifiers.
// Keep this separate from usage-log correlation and the OpenAI gateway resolver.
func GatewaySessionIDFromHeaders(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, header := range clientSessionIDHeaders {
		if id := sanitizeSessionID(c.GetHeader(header)); id != "" {
			return id
		}
	}
	return ""
}

// gatewayMetadataSessionID preserves the structured Claude metadata contract.
// A bare user_id is accepted only when it explicitly says session_<UUID>;
// treating an arbitrary user/account ID as a session would merge conversations.
// Do not extend ParseMetadataUserID: identity rewriting also consumes that parser
// and needs its device/account components, which bare session IDs do not have.
func gatewayMetadataSessionID(raw string) string {
	if parsed := ParseMetadataUserID(raw); parsed != nil && parsed.SessionID != "" {
		return parsed.SessionID
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "session_") {
		return ""
	}
	value := strings.TrimPrefix(raw, "session_")
	if len(value) != 36 {
		return ""
	}
	id, err := uuid.Parse(value)
	if err != nil || !strings.EqualFold(value, id.String()) {
		return ""
	}
	return id.String()
}

func gatewayExplicitSessionIdentity(parsed *ParsedRequest) (source, seed string) {
	if parsed == nil {
		return "", ""
	}
	if id := gatewayMetadataSessionID(parsed.MetadataUserID); id != "" {
		return "metadata_user_id", id
	}
	if parsed.SessionContext != nil {
		if id := sanitizeSessionID(parsed.SessionContext.ClientSessionID); id != "" {
			return "client_session_header", id
		}
	}
	return "", ""
}

// HasExplicitSessionIdentity distinguishes a conversation anchor from a content
// guess so native Gemini's digest recovery cannot override an explicit session.
func (p *ParsedRequest) HasExplicitSessionIdentity() bool {
	_, seed := gatewayExplicitSessionIdentity(p)
	return seed != ""
}

// GenerateScopedSessionHash applies the same authenticated namespace to every
// generic gateway seed, including the native Gemini CLI compatibility seed.
// v2 deliberately does not read old unscoped bindings: those can belong to a
// different API key. Group isolation is retained both here and in the cache key.
func (s *GatewayService) GenerateScopedSessionHash(parsed *ParsedRequest, source, seed string) string {
	if parsed == nil || seed == "" {
		return ""
	}
	var keyID int64
	var platform string
	if parsed.SessionContext != nil {
		keyID = parsed.SessionContext.APIKeyID
		platform = parsed.SessionContext.Platform
	}
	// Quoting prevents separators in client-controlled IDs from aliasing fields.
	scoped := strconv.FormatInt(keyID, 10) + ":" + strconv.FormatInt(derefGroupID(parsed.GroupID), 10) +
		":" + strconv.Quote(platform) + ":" + strconv.Quote(source) + ":" + strconv.Quote(seed)
	return "v2:" + s.hashContent(scoped)
}
