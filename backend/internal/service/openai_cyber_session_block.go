package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type CyberSessionBlockKind string

const (
	CyberSessionBlockKindExplicit   CyberSessionBlockKind = "explicit"
	CyberSessionBlockKindTranscript CyberSessionBlockKind = "transcript"
)

// CyberSessionBlockReceipt is safe to persist in Ops JSON. Digest is an
// irreversible tenant-isolated hash; no raw identifier or request content is
// retained. Stored means the exact key existed after a successful write or
// lookup. ExpiryUnixMs is omitted when Redis has no usable positive TTL.
type CyberSessionBlockReceipt struct {
	Digest       string                `json:"digest,omitempty"`
	Kind         CyberSessionBlockKind `json:"kind,omitempty"`
	Source       string                `json:"source,omitempty"`
	Count        int                   `json:"count"`
	Truncated    bool                  `json:"truncated"`
	Stored       bool                  `json:"stored"`
	ExpiryUnixMs int64                 `json:"expiry,omitempty"`
}

func (r CyberSessionBlockReceipt) Matched() bool {
	return r.Stored && r.Digest != ""
}

// CyberSessionBlockPlan is the content-free write side of one exclusive
// matcher mode. Explicit identity and transcript ancestry are never combined.
type CyberSessionBlockPlan struct {
	Digest    string
	Kind      CyberSessionBlockKind
	Source    string
	ScopeKey  string
	Count     int
	Truncated bool
}

// CyberSessionBlockStore stores v2 explicit and transcript keys in distinct
// namespaces. Scope is only a transcript lookup optimization.
type CyberSessionBlockStore interface {
	SetCyberSessionBlocked(ctx context.Context, kind CyberSessionBlockKind, scopeKey, digest string, ttl time.Duration) error
	IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error)
	FindCyberSessionBlocked(ctx context.Context, kind CyberSessionBlockKind, digests []string) (string, time.Duration, error)
}

type cyberSemanticSession struct {
	id     string
	source string
}

var cyberSemanticSessionHeaders = []struct {
	name   string
	source string
}{
	{"session-id", "header:session-id"},
	{"session_id", "header:session_id"},
	{"conversation_id", "header:conversation_id"},
	{openCodeSessionIDHeader, "header:x-session-id"},
	{openCodeNativeSessionHeader, "header:x-opencode-session"},
	{codeBuddyConversationHeader, "header:x-conversation-id"},
	{claudeCodeSessionHeader, "header:x-claude-code-session-id"},
}

// resolveCyberSemanticSession deliberately differs from sticky scheduling.
// prompt_cache_key, X-Session-Affinity, previous_response_id, request/turn IDs,
// and content-derived affinity are not trustworthy conversation identity.
func resolveCyberSemanticSession(c *gin.Context, body []byte) cyberSemanticSession {
	if c == nil || c.Request == nil {
		return cyberSemanticSession{}
	}
	payload := openAIRequestPayloadView(body)
	if isOpenAIResponsesRequestPath(c) {
		// Responses-native body identity is authoritative over compatibility
		// headers. It represents the semantic conversation, unlike cache keys,
		// response ids, thread ids, and rotating turn ids.
		if id := sanitizeSessionID(payload.Get("client_metadata.session_id").String()); id != "" {
			return cyberSemanticSession{id: id, source: "metadata:codex-session-id"}
		}
		for _, path := range []string{"client_metadata.x-codex-turn-metadata", "client_metadata.X-Codex-Turn-Metadata"} {
			if raw := strings.TrimSpace(payload.Get(path).String()); gjson.Valid(raw) {
				if id := sanitizeSessionID(gjson.Get(raw, "session_id").String()); id != "" {
					return cyberSemanticSession{id: id, source: "metadata:codex-turn-session-id"}
				}
			}
		}
		if raw := validInboundHeaderValue(c, "X-Codex-Turn-Metadata"); gjson.Valid(raw) {
			if id := sanitizeSessionID(gjson.Get(raw, "session_id").String()); id != "" {
				return cyberSemanticSession{id: id, source: "header:x-codex-turn-metadata.session_id"}
			}
		}
	}
	for _, candidate := range cyberSemanticSessionHeaders {
		if id := sanitizeSessionID(c.GetHeader(candidate.name)); id != "" {
			return cyberSemanticSession{id: id, source: candidate.source}
		}
	}
	if isGrokRequestContext(c) {
		for _, candidate := range []struct {
			name   string
			source string
		}{
			{grokConversationIDHeader, "header:x-grok-conv-id"},
			{grokSessionIDHeader, "header:x-grok-session-id"},
		} {
			if id := sanitizeSessionID(validInboundHeaderValue(c, candidate.name)); id != "" {
				return cyberSemanticSession{id: id, source: candidate.source}
			}
		}
	}
	if isAnthropicMessagesRequestPath(c) {
		if id := sanitizeSessionID(extractClaudeCodeSessionIDFromPayload(body)); id != "" {
			return cyberSemanticSession{id: id, source: "metadata:claude-session-id"}
		}
	}
	return cyberSemanticSession{}
}

func isAnthropicMessagesRequestPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(path, "/messages")
}

func isOpenAIResponsesRequestPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact")
}

func CyberSessionExplicitBlockKey(apiKeyID int64, c *gin.Context, body []byte) string {
	return hashCyberSessionExplicitKey(apiKeyID, resolveCyberSemanticSession(c, body).id)
}

// CyberSessionTranscriptBlockKeys contains only the full refused transcript
// digest. Prefix digests are lookup candidates, never writes.
func CyberSessionTranscriptBlockKeys(apiKeyID int64, body []byte) []string {
	derived := deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body)
	if len(derived.lookupKeys) == 0 {
		return nil
	}
	return []string{derived.lookupKeys[len(derived.lookupKeys)-1]}
}

func CyberSessionTranscriptLookupKeys(apiKeyID int64, body []byte) []string {
	return deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body).lookupKeys
}

func CyberSessionScopeKey(apiKeyID int64, clientIP, userAgent string) string {
	if apiKeyID <= 0 {
		return ""
	}
	h := sha256.New()
	writeCyberHashField(h, "sub2api.cyber-policy.scope.v2")
	writeCyberHashField(h, strconv.FormatInt(apiKeyID, 10))
	writeCyberHashField(h, strings.TrimSpace(clientIP))
	writeCyberHashField(h, NormalizeSessionUserAgent(userAgent))
	return hex.EncodeToString(h.Sum(nil))
}

func hashCyberSessionExplicitKey(apiKeyID int64, raw string) string {
	if apiKeyID <= 0 || raw == "" {
		return ""
	}
	h := sha256.New()
	writeCyberHashField(h, "sub2api.cyber-policy.explicit.v2")
	writeCyberHashField(h, strconv.FormatInt(apiKeyID, 10))
	writeCyberHashField(h, raw)
	return hex.EncodeToString(h.Sum(nil))
}

func ResolveCyberSessionBlockWritePlan(apiKeyID int64, c *gin.Context, body []byte, clientIP, userAgent string) CyberSessionBlockPlan {
	if session := resolveCyberSemanticSession(c, body); session.id != "" {
		return CyberSessionBlockPlan{
			Digest: hashCyberSessionExplicitKey(apiKeyID, session.id),
			Kind:   CyberSessionBlockKindExplicit,
			Source: session.source,
			Count:  1,
		}
	}
	derived := deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body)
	if len(derived.lookupKeys) == 0 {
		return CyberSessionBlockPlan{}
	}
	return CyberSessionBlockPlan{
		Digest:    derived.lookupKeys[len(derived.lookupKeys)-1],
		Kind:      CyberSessionBlockKindTranscript,
		Source:    "transcript",
		ScopeKey:  CyberSessionScopeKey(apiKeyID, clientIP, userAgent),
		Count:     1,
		Truncated: derived.lookupKeysTruncated,
	}
}

func (s *OpenAIGatewayService) cyberSessionBlockStore() CyberSessionBlockStore {
	if s == nil || s.cache == nil {
		return nil
	}
	store, _ := s.cache.(CyberSessionBlockStore)
	return store
}

func (s *OpenAIGatewayService) CyberSessionBlockRuntime(ctx context.Context) (bool, time.Duration) {
	if s == nil || s.settingService == nil {
		return false, time.Hour
	}
	return s.settingService.GetCyberSessionBlockRuntime(ctx)
}

func (s *OpenAIGatewayService) MarkCyberSessionBlocked(ctx context.Context, plan CyberSessionBlockPlan) CyberSessionBlockReceipt {
	receipt := CyberSessionBlockReceipt{
		Digest:    plan.Digest,
		Kind:      plan.Kind,
		Source:    plan.Source,
		Count:     plan.Count,
		Truncated: plan.Truncated,
	}
	if s == nil || plan.Digest == "" {
		return receipt
	}
	if plan.Kind != CyberSessionBlockKindExplicit && plan.Kind != CyberSessionBlockKindTranscript {
		return receipt
	}
	enabled, ttl := s.CyberSessionBlockRuntime(ctx)
	if !enabled || ttl <= 0 {
		return receipt
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return receipt
	}
	if err := store.SetCyberSessionBlocked(ctx, plan.Kind, plan.ScopeKey, plan.Digest, ttl); err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block write failed: err=%v", err)
		return receipt
	}
	receipt.Stored = true
	receipt.ExpiryUnixMs = time.Now().Add(ttl).UnixMilli()
	return receipt
}

// FindCyberSessionBlockedForRequest uses exactly one matcher mode. Explicit
// identity never falls back to transcript or scope when its exact lookup misses.
func (s *OpenAIGatewayService) FindCyberSessionBlockedForRequest(ctx context.Context, apiKeyID int64, c *gin.Context, body []byte, clientIP, userAgent string) CyberSessionBlockReceipt {
	enabled, _ := s.CyberSessionBlockRuntime(ctx)
	if !enabled {
		return CyberSessionBlockReceipt{}
	}
	store := s.cyberSessionBlockStore()
	if store == nil {
		return CyberSessionBlockReceipt{}
	}
	if session := resolveCyberSemanticSession(c, body); session.id != "" {
		digest := hashCyberSessionExplicitKey(apiKeyID, session.id)
		return findCyberSessionReceipt(ctx, store, CyberSessionBlockKindExplicit, session.source, []string{digest}, 1, false)
	}

	derived := deriveOpenAICyberTranscriptBlockKeys(apiKeyID, body)
	receipt := CyberSessionBlockReceipt{
		Kind:      CyberSessionBlockKindTranscript,
		Source:    "transcript",
		Count:     len(derived.lookupKeys),
		Truncated: derived.lookupKeysTruncated,
	}
	if len(derived.lookupKeys) == 0 {
		return receipt
	}
	scopeKey := CyberSessionScopeKey(apiKeyID, clientIP, userAgent)
	active, err := store.IsCyberSessionScopeActive(ctx, scopeKey)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session scope read failed: err=%v", err)
		return receipt
	}
	if !active {
		return receipt
	}
	// A truncated miss is intentionally fail-open: only the newest 256 exact
	// cumulative keys are queried and there is no overflow sentinel.
	matched := findCyberSessionReceipt(ctx, store, CyberSessionBlockKindTranscript, "transcript", derived.lookupKeys, len(derived.lookupKeys), derived.lookupKeysTruncated)
	if matched.Truncated && !matched.Matched() {
		logger.LegacyPrintf("service.openai_gateway", "cyber transcript lookup truncated miss: count=%d", matched.Count)
	}
	return matched
}

func findCyberSessionReceipt(ctx context.Context, store CyberSessionBlockStore, kind CyberSessionBlockKind, source string, digests []string, count int, truncated bool) CyberSessionBlockReceipt {
	receipt := CyberSessionBlockReceipt{Kind: kind, Source: source, Count: count, Truncated: truncated}
	if len(digests) == 0 || digests[0] == "" {
		return receipt
	}
	digest, ttl, err := store.FindCyberSessionBlocked(ctx, kind, digests)
	if err != nil {
		logger.LegacyPrintf("service.openai_gateway", "cyber session block read failed: err=%v", err)
		return receipt
	}
	if digest == "" || ttl <= 0 {
		return receipt
	}
	receipt.Digest = digest
	receipt.Stored = true
	if ttl > 0 {
		receipt.ExpiryUnixMs = time.Now().Add(ttl).UnixMilli()
	}
	return receipt
}
