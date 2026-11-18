package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	antigravitySafetyStreakAbortedCode = "antigravity_safety_streak_aborted"
	antigravitySafetyStreakGinKey      = "_antigravity_safety_streak_observer"

	antigravitySoftSafetyStreakThreshold = 3
	antigravityHardSafetyStreakThreshold = 2

	antigravitySafetyStreakRecordTTL   = 15 * time.Minute
	antigravitySafetyStreakCooldownTTL = 10 * time.Minute

	antigravitySoftSafetyFilterMarkerA = "This request was blocked by Gemini's filters."
	antigravitySoftSafetyFilterMarkerB = "generative-ai/use-policy"

	antigravitySafetyStreakMaxMemoryEntries = 8192
)

// ErrAntigravitySafetyStreakNotFound indicates no streak record exists in cache.
var ErrAntigravitySafetyStreakNotFound = errors.New("antigravity safety streak not found")

// AntigravitySafetyStreakCache is an optional extension on GatewayCache backed by Redis
// (`agy:safety_streak:<hash>`). When GatewayCache is nil or Redis is unavailable,
// AntigravityGatewayService automatically falls back to its in-memory concurrent store.
type AntigravitySafetyStreakCache interface {
	GetAntigravitySafetyStreak(ctx context.Context, sessionHash string) ([]byte, error)
	SetAntigravitySafetyStreak(ctx context.Context, sessionHash string, payload []byte, ttl time.Duration) error
	DeleteAntigravitySafetyStreak(ctx context.Context, sessionHash string) error
}

type antigravitySafetyStreakState struct {
	SoftStreak    int       `json:"soft_streak"`
	HardStreak    int       `json:"hard_streak"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (st antigravitySafetyStreakState) isTrippedAt(now time.Time) bool {
	if st.CooldownUntil.IsZero() {
		return false
	}
	return now.Before(st.CooldownUntil)
}

func (st antigravitySafetyStreakState) isExpiredCooldownAt(now time.Time) bool {
	return !st.CooldownUntil.IsZero() && !now.Before(st.CooldownUntil)
}

type antigravityMemorySafetyStreakEntry struct {
	state     antigravitySafetyStreakState
	expiresAt time.Time
}

type antigravityMemorySafetyStreakStore struct {
	mu      sync.Mutex
	entries map[string]antigravityMemorySafetyStreakEntry
}

func newAntigravityMemorySafetyStreakStore() *antigravityMemorySafetyStreakStore {
	return &antigravityMemorySafetyStreakStore{
		entries: make(map[string]antigravityMemorySafetyStreakEntry),
	}
}

func (m *antigravityMemorySafetyStreakStore) get(sessionHash string, now time.Time) (antigravitySafetyStreakState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[sessionHash]
	if !ok {
		return antigravitySafetyStreakState{}, false
	}
	if !now.Before(entry.expiresAt) {
		delete(m.entries, sessionHash)
		return antigravitySafetyStreakState{}, false
	}
	return entry.state, true
}

func (m *antigravityMemorySafetyStreakStore) set(sessionHash string, state antigravitySafetyStreakState, ttl time.Duration, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) >= antigravitySafetyStreakMaxMemoryEntries {
		for k, v := range m.entries {
			if !now.Before(v.expiresAt) {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= antigravitySafetyStreakMaxMemoryEntries {
			for k := range m.entries {
				delete(m.entries, k)
				break
			}
		}
	}
	m.entries[sessionHash] = antigravityMemorySafetyStreakEntry{
		state:     state,
		expiresAt: now.Add(ttl),
	}
}

func (m *antigravityMemorySafetyStreakStore) del(sessionHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, sessionHash)
}

func (s *AntigravityGatewayService) memorySafetyStreakStore() *antigravityMemorySafetyStreakStore {
	s.safetyStreakFallbackOnce.Do(func() {
		if s.safetyStreakFallback == nil {
			s.safetyStreakFallback = newAntigravityMemorySafetyStreakStore()
		}
	})
	return s.safetyStreakFallback
}

// antigravitySafetyStreakSessionHash derives a cross-account, per-session hash from
// authenticated APIKey + explicit conversation identity (metadata.user_id or session header).
// Excludes account.ID so account failover within one subagent shares streak state, while
// parent main and subagent sessions stay strictly isolated. Stateless or internal warmup
// requests without api_key or explicit session seed return "".
func antigravitySafetyStreakSessionHash(c *gin.Context, request *antigravity.ClaudeRequest) string {
	if c == nil || request == nil {
		return ""
	}
	value, _ := c.Get("api_key")
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 {
		return ""
	}
	parsed := &ParsedRequest{
		SessionContext: &SessionContext{
			ClientSessionID: GatewaySessionIDFromHeaders(c),
		},
	}
	if request.Metadata != nil {
		parsed.MetadataUserID = request.Metadata.UserID
	}
	source, seed := gatewayExplicitSessionIdentity(parsed)
	if seed == "" {
		return ""
	}
	scope := strconv.FormatInt(key.ID, 10) + ":" + strconv.Quote(source) + ":" + strconv.Quote(seed)
	digest := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(digest[:16])
}

func (s *AntigravityGatewayService) loadAntigravitySafetyStreak(ctx context.Context, sessionHash string, now time.Time) (antigravitySafetyStreakState, bool) {
	if s == nil || sessionHash == "" {
		return antigravitySafetyStreakState{}, false
	}
	if redisStore, ok := s.cache.(AntigravitySafetyStreakCache); ok && redisStore != nil {
		raw, err := redisStore.GetAntigravitySafetyStreak(ctx, sessionHash)
		if err == nil && len(raw) > 0 {
			var state antigravitySafetyStreakState
			if unmarshalErr := json.Unmarshal(raw, &state); unmarshalErr == nil {
				return state, true
			}
		} else if errors.Is(err, ErrAntigravitySafetyStreakNotFound) {
			return antigravitySafetyStreakState{}, false
		}
	}
	return s.memorySafetyStreakStore().get(sessionHash, now)
}

func (s *AntigravityGatewayService) saveAntigravitySafetyStreak(ctx context.Context, sessionHash string, state antigravitySafetyStreakState, ttl time.Duration, now time.Time) {
	if s == nil || sessionHash == "" {
		return
	}
	s.memorySafetyStreakStore().set(sessionHash, state, ttl, now)
	if redisStore, ok := s.cache.(AntigravitySafetyStreakCache); ok && redisStore != nil {
		if raw, err := json.Marshal(state); err == nil {
			if setErr := redisStore.SetAntigravitySafetyStreak(ctx, sessionHash, raw, ttl); setErr != nil {
				logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-SafetyStreak] redis_set_failed session=%s err=%v", sessionHash, setErr)
			}
		}
	}
}

func (s *AntigravityGatewayService) clearAntigravitySafetyStreak(ctx context.Context, sessionHash string) {
	if s == nil || sessionHash == "" {
		return
	}
	s.memorySafetyStreakStore().del(sessionHash)
	if redisStore, ok := s.cache.(AntigravitySafetyStreakCache); ok && redisStore != nil {
		if delErr := redisStore.DeleteAntigravitySafetyStreak(ctx, sessionHash); delErr != nil {
			logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-SafetyStreak] redis_del_failed session=%s err=%v", sessionHash, delErr)
		}
	}
}

// checkAntigravitySafetyStreakAdmission aborts with a non-retryable HTTP 400 before
// contacting Google upstream when the session is in active safety-streak cooldown.
func (s *AntigravityGatewayService) checkAntigravitySafetyStreakAdmission(ctx context.Context, c *gin.Context, sessionHash string) error {
	if s == nil || c == nil || sessionHash == "" {
		return nil
	}
	now := time.Now()
	state, found := s.loadAntigravitySafetyStreak(ctx, sessionHash, now)
	if !found {
		return nil
	}
	if state.isExpiredCooldownAt(now) {
		s.clearAntigravitySafetyStreak(ctx, sessionHash)
		return nil
	}
	if !state.isTrippedAt(now) {
		return nil
	}

	remainingSec := int(time.Until(state.CooldownUntil).Seconds())
	if remainingSec < 1 {
		remainingSec = 1
	}
	msg := fmt.Sprintf(
		"Request aborted: this session triggered consecutive Gemini safety filter blocks (soft=%d, hard=%d). Please rephrase or trim the flagged prompt context before retrying (cooldown %ds remaining).",
		state.SoftStreak,
		state.HardStreak,
		remainingSec,
	)
	logger.LegacyPrintf(
		"service.antigravity_gateway",
		"[antigravity-SafetyStreak] aborted_pre_upstream session=%s soft_streak=%d hard_streak=%d cooldown_remaining_s=%d",
		sessionHash, state.SoftStreak, state.HardStreak, remainingSec,
	)

	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{
		"type": "error",
		"error": gin.H{
			"type":      "invalid_request_error",
			"code":      antigravitySafetyStreakAbortedCode,
			"message":   msg,
			"retryable": false,
		},
	})
	return fmt.Errorf("%s: %s", antigravitySafetyStreakAbortedCode, msg)
}

type antigravitySafetyStreakObserver struct {
	mu                    sync.Mutex
	rollingTail           string
	sawSoftMarkerA        bool
	sawSoftMarkerB        bool
	sawVisibleText        bool
	sawToolUse            bool
	sawUpstreamSafetyHTTP bool
}

func beginAntigravitySafetyStreakObservation(c *gin.Context) *antigravitySafetyStreakObserver {
	obs := &antigravitySafetyStreakObserver{}
	if c != nil {
		c.Set(antigravitySafetyStreakGinKey, obs)
	}
	return obs
}

func antigravitySafetyStreakObserverFromContext(c *gin.Context) *antigravitySafetyStreakObserver {
	if c == nil {
		return nil
	}
	val, exists := c.Get(antigravitySafetyStreakGinKey)
	if !exists {
		return nil
	}
	obs, _ := val.(*antigravitySafetyStreakObserver)
	return obs
}

func antigravitySoftSafetyBlockedInContext(c *gin.Context) bool {
	return antigravitySafetyStreakObserverFromContext(c).summary().softSafetyBlocked
}

func observeAntigravitySafetyStreakSSEPayload(c *gin.Context, payload []byte) {
	if c == nil || len(payload) == 0 {
		return
	}
	obs := antigravitySafetyStreakObserverFromContext(c)
	if obs == nil && StreamPreheaderConfigured(c) {
		// Stream diagnostics also apply to compatibility and sessionless requests.
		obs = beginAntigravitySafetyStreakObservation(c)
	}
	obs.observeGeminiPayload(payload)
}

func (o *antigravitySafetyStreakObserver) markUpstreamSafetyHTTP() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.sawUpstreamSafetyHTTP = true
	o.mu.Unlock()
}

func (o *antigravitySafetyStreakObserver) observeGeminiPayload(payload []byte) {
	if o == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	parsed := gjson.ParseBytes(payload)
	if inner := parsed.Get("response"); inner.Exists() && inner.IsObject() {
		parsed = inner
	}
	if !parsed.IsObject() {
		return
	}

	candidates := parsed.Get("candidates")
	if !candidates.IsArray() {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	for _, candidate := range candidates.Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			if !part.IsObject() {
				continue
			}
			if part.Get("functionCall").IsObject() {
				o.sawToolUse = true
				continue
			}
			if part.Get("inlineData.data").String() != "" {
				o.sawVisibleText = true
				continue
			}
			if part.Get("thought").Bool() {
				continue
			}
			text := part.Get("text").String()
			if text == "" {
				continue
			}
			if strings.TrimSpace(text) != "" {
				o.sawVisibleText = true
			}
			combined := o.rollingTail + text
			if !o.sawSoftMarkerA && strings.Contains(combined, antigravitySoftSafetyFilterMarkerA) {
				o.sawSoftMarkerA = true
			}
			if !o.sawSoftMarkerB && strings.Contains(combined, antigravitySoftSafetyFilterMarkerB) {
				o.sawSoftMarkerB = true
			}
			const maxTailBytes = 256
			if len(combined) > maxTailBytes {
				o.rollingTail = combined[len(combined)-maxTailBytes:]
			} else {
				o.rollingTail = combined
			}
		}
	}
}

type antigravitySafetyStreakTurnSummary struct {
	softSafetyBlocked bool
	sawVisibleText    bool
	sawToolUse        bool
	sawSafetyHTTP     bool
}

func (o *antigravitySafetyStreakObserver) summary() antigravitySafetyStreakTurnSummary {
	if o == nil {
		return antigravitySafetyStreakTurnSummary{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return antigravitySafetyStreakTurnSummary{
		softSafetyBlocked: o.sawSoftMarkerA && o.sawSoftMarkerB,
		sawVisibleText:    o.sawVisibleText,
		sawToolUse:        o.sawToolUse,
		sawSafetyHTTP:     o.sawUpstreamSafetyHTTP,
	}
}

// isAntigravityUpstreamSafetyBlock detects explicit content/prompt safety blocks in
// upstream HTTP 400/403 responses while leaving account-level 403s (validation, ToS, IAM) untouched.
func isAntigravityUpstreamSafetyBlock(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusForbidden {
		return false
	}
	if len(body) == 0 {
		return false
	}
	if gjson.ValidBytes(body) {
		if blockReason := strings.TrimSpace(gjson.GetBytes(body, "promptFeedback.blockReason").String()); blockReason != "" && blockReason != "BLOCK_REASON_UNSPECIFIED" {
			return true
		}
		if blockReason := strings.TrimSpace(gjson.GetBytes(body, "response.promptFeedback.blockReason").String()); blockReason != "" && blockReason != "BLOCK_REASON_UNSPECIFIED" {
			return true
		}
		for _, detail := range gjson.GetBytes(body, "error.details").Array() {
			reason := strings.ToUpper(strings.TrimSpace(detail.Get("reason").String()))
			switch reason {
			case "ANTIGRAVITY_SAFETY", "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
				return true
			}
		}
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"antigravity_safety",
		"prohibited_content",
		"image_safety",
		"image_prohibited_content",
		"harm_category_",
		"blocked by safety",
		"safety policy",
		"safety settings",
		"blocked by gemini's filters",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isAntigravityHardSafetyOutcome(forwardErr error, cont *antigravityThoughtContinuation, turnSummary antigravitySafetyStreakTurnSummary) bool {
	if turnSummary.sawSafetyHTTP {
		return true
	}
	var noAns *AntigravityNoUsableAnswerError
	if errors.As(forwardErr, &noAns) && noAns != nil && noAns.Code == "antigravity_safety" {
		return true
	}
	if cont != nil {
		if failure := cont.responseFailure(); failure != nil && failure.Code == "antigravity_safety" {
			return true
		}
		if failure := cont.collectedResponseFailure(); failure != nil && failure.Code == "antigravity_safety" {
			return true
		}
	}
	return false
}

func (s *AntigravityGatewayService) recordAntigravitySafetyStreakOutcome(
	ctx context.Context,
	sessionHash string,
	obs *antigravitySafetyStreakObserver,
	cont *antigravityThoughtContinuation,
	forwardResult *ForwardResult,
	forwardErr error,
) {
	if s == nil || sessionHash == "" || obs == nil {
		return
	}
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}

	turn := obs.summary()
	now := time.Now()

	if turn.softSafetyBlocked {
		state, _ := s.loadAntigravitySafetyStreak(ctx, sessionHash, now)
		if state.isExpiredCooldownAt(now) {
			state = antigravitySafetyStreakState{}
		}
		state.SoftStreak++
		state.UpdatedAt = now
		if state.SoftStreak >= antigravitySoftSafetyStreakThreshold {
			state.CooldownUntil = now.Add(antigravitySafetyStreakCooldownTTL)
			logger.LegacyPrintf(
				"service.antigravity_gateway",
				"[antigravity-SafetyStreak] circuit_opened kind=soft session=%s soft_streak=%d hard_streak=%d cooldown=%s",
				sessionHash, state.SoftStreak, state.HardStreak, antigravitySafetyStreakCooldownTTL,
			)
		}
		s.saveAntigravitySafetyStreak(ctx, sessionHash, state, antigravitySafetyStreakRecordTTL, now)
		return
	}

	if isAntigravityHardSafetyOutcome(forwardErr, cont, turn) {
		state, _ := s.loadAntigravitySafetyStreak(ctx, sessionHash, now)
		if state.isExpiredCooldownAt(now) {
			state = antigravitySafetyStreakState{}
		}
		state.HardStreak++
		state.UpdatedAt = now
		if state.HardStreak >= antigravityHardSafetyStreakThreshold {
			state.CooldownUntil = now.Add(antigravitySafetyStreakCooldownTTL)
			logger.LegacyPrintf(
				"service.antigravity_gateway",
				"[antigravity-SafetyStreak] circuit_opened kind=hard session=%s soft_streak=%d hard_streak=%d cooldown=%s",
				sessionHash, state.SoftStreak, state.HardStreak, antigravitySafetyStreakCooldownTTL,
			)
		}
		s.saveAntigravitySafetyStreak(ctx, sessionHash, state, antigravitySafetyStreakRecordTTL, now)
		return
	}

	// Progress reset rule: any clean turn producing non-empty visible text or tool_use
	// immediately clears both soft_streak and hard_streak for this session.
	if forwardErr == nil && forwardResult != nil && !forwardResult.ClientDisconnect && (turn.sawVisibleText || turn.sawToolUse) {
		s.clearAntigravitySafetyStreak(ctx, sessionHash)
	}
}
