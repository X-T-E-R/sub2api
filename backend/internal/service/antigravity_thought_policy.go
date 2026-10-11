package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Task24: independent Antigravity "thought-only terminal" switches.
//
// The two switches are independent and both default to off in source:
//
//   - SuppressThoughtOnlyStop (A) keeps the received thinking, signature and real
//     usage but suppresses the successful terminal completion signal for a response
//     that only ever produced thinking, so the downstream client applies its own
//     continuation policy.
//   - ContinueThoughtOnly (B) continues once by re-sending the received
//     thinking/signature as the assistant tail, without adding any system/user text.
//
// The global pair persists in exactly one JSON settings key. Each account may carry
// an explicit per-switch override in Account.Extra; a missing or null value inherits
// the global switch, while an explicit false always wins over a global true.

// SettingKeyAntigravityThoughtPolicy stores both global switches as one JSON object:
// {"suppress_thought_only_stop":bool,"continue_thought_only":bool}.
const SettingKeyAntigravityThoughtPolicy = "antigravity_thought_policy"

const (
	// Flat per-account override keys inside Account.Extra. Flat keys keep the
	// account payload readable and let each switch be overridden independently.
	AntigravitySuppressThoughtOnlyStopExtraKey = "antigravity_suppress_thought_only_stop"
	AntigravityContinueThoughtOnlyExtraKey     = "antigravity_continue_thought_only"
)

// AntigravityThoughtPolicySettings is the persisted global switch pair.
type AntigravityThoughtPolicySettings struct {
	SuppressThoughtOnlyStop bool `json:"suppress_thought_only_stop"`
	ContinueThoughtOnly     bool `json:"continue_thought_only"`
}

// AntigravityThoughtPolicy is the resolved effective policy for one request.
//
// It is an immutable value snapshot: callers receive a copy, no method mutates it,
// and every request must reuse the single copy it resolved so the two switches
// cannot disagree inside one response.
type AntigravityThoughtPolicy struct {
	SuppressThoughtOnlyStop bool
	ContinueThoughtOnly     bool
}

// DefaultAntigravityThoughtPolicySettings is the seed used when the setting key is
// absent: both switches off, which preserves the pre-Task24 gateway behavior.
func DefaultAntigravityThoughtPolicySettings() AntigravityThoughtPolicySettings {
	return AntigravityThoughtPolicySettings{
		SuppressThoughtOnlyStop: false,
		ContinueThoughtOnly:     false,
	}
}

var antigravityThoughtPolicySnapshot atomic.Pointer[AntigravityThoughtPolicySettings]
var antigravityThoughtPolicyMu sync.Mutex

// currentAntigravityThoughtPolicySettings reads the preloaded snapshot without
// touching persistent settings. An unloaded process reports the seed (off/off).
func currentAntigravityThoughtPolicySettings() AntigravityThoughtPolicySettings {
	if snapshot := antigravityThoughtPolicySnapshot.Load(); snapshot != nil {
		return *snapshot
	}
	return DefaultAntigravityThoughtPolicySettings()
}

type antigravityThoughtPolicyKey struct{}

func antigravityThoughtPolicyFromContext(ctx context.Context) (AntigravityThoughtPolicySettings, bool) {
	if ctx != nil {
		if pinned, ok := ctx.Value(antigravityThoughtPolicyKey{}).(*AntigravityThoughtPolicySettings); ok && pinned != nil {
			return *pinned, true
		}
	}
	return AntigravityThoughtPolicySettings{}, false
}

// WithAntigravityThoughtPolicySnapshot pins the global switches for one downstream
// request. A later global update must not change an in-flight decision, and an
// already pinned request keeps its original pair.
func WithAntigravityThoughtPolicySnapshot(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := antigravityThoughtPolicyFromContext(ctx); ok {
		return ctx
	}
	value := currentAntigravityThoughtPolicySettings()
	return context.WithValue(ctx, antigravityThoughtPolicyKey{}, &value)
}

// GetAntigravityThoughtPolicySettings reads the global switches from persistent
// settings; an absent key reports the seed off/off.
func (s *SettingService) GetAntigravityThoughtPolicySettings(ctx context.Context) (AntigravityThoughtPolicySettings, error) {
	value, _, err := s.readAntigravityThoughtPolicySettings(ctx)
	return value, err
}

func (s *SettingService) readAntigravityThoughtPolicySettings(ctx context.Context) (AntigravityThoughtPolicySettings, string, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultAntigravityThoughtPolicySettings(), "seed", nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAntigravityThoughtPolicy)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return DefaultAntigravityThoughtPolicySettings(), "settings", err
	}
	if raw == "" {
		return DefaultAntigravityThoughtPolicySettings(), "seed", nil
	}
	out := AntigravityThoughtPolicySettings{}
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return DefaultAntigravityThoughtPolicySettings(), "settings", err
	}
	return out, "settings", nil
}

// LoadAntigravityThoughtPolicySettings warms the atomic snapshot used by request
// paths. A failed read leaves the previously published pair untouched.
func (s *SettingService) LoadAntigravityThoughtPolicySettings(ctx context.Context) error {
	antigravityThoughtPolicyMu.Lock()
	defer antigravityThoughtPolicyMu.Unlock()
	value, source, err := s.readAntigravityThoughtPolicySettings(ctx)
	if err != nil {
		return err
	}
	antigravityThoughtPolicySnapshot.Store(&value)
	logger.LegacyPrintf("service.antigravity_thought_policy", "loaded key=%s source=%s suppress_thought_only_stop=%t continue_thought_only=%t", SettingKeyAntigravityThoughtPolicy, source, value.SuppressThoughtOnlyStop, value.ContinueThoughtOnly)
	return nil
}

// SetAntigravityThoughtPolicySettings persists the pair before publishing it, so a
// write failure preserves the previous in-memory decision.
func (s *SettingService) SetAntigravityThoughtPolicySettings(ctx context.Context, value AntigravityThoughtPolicySettings) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("setting repository unavailable")
	}
	antigravityThoughtPolicyMu.Lock()
	defer antigravityThoughtPolicyMu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = s.settingRepo.Set(ctx, SettingKeyAntigravityThoughtPolicy, string(raw)); err != nil {
		return err
	}
	antigravityThoughtPolicySnapshot.Store(&value)
	logger.LegacyPrintf("service.antigravity_thought_policy", "updated key=%s source=settings suppress_thought_only_stop=%t continue_thought_only=%t", SettingKeyAntigravityThoughtPolicy, value.SuppressThoughtOnlyStop, value.ContinueThoughtOnly)
	return nil
}

// ResolveAntigravityThoughtPolicy returns the effective policy for one account.
//
// Precedence per switch: account override -> request-pinned global snapshot ->
// persistently loaded global snapshot. It never mutates global state and never
// writes to persistent settings, so it is safe on a request hot path.
func (s *SettingService) ResolveAntigravityThoughtPolicy(ctx context.Context, account *Account) AntigravityThoughtPolicy {
	if ctx == nil {
		ctx = context.Background()
	}
	global, pinned := antigravityThoughtPolicyFromContext(ctx)
	if !pinned {
		global = s.loadedAntigravityThoughtPolicySettings(ctx)
	}
	return effectiveAntigravityThoughtPolicy(global, account)
}

// ResolveAntigravityThoughtPolicy is the same resolution for callers that do not
// hold a SettingService: it uses the request-pinned snapshot when present and the
// process snapshot otherwise. Use WithAntigravityThoughtPolicySnapshot at ingress
// so every retry inside one request keeps one effective pair.
func ResolveAntigravityThoughtPolicy(ctx context.Context, account *Account) AntigravityThoughtPolicy {
	global, pinned := antigravityThoughtPolicyFromContext(ctx)
	if !pinned {
		global = currentAntigravityThoughtPolicySettings()
	}
	return effectiveAntigravityThoughtPolicy(global, account)
}

// loadedAntigravityThoughtPolicySettings prefers the preloaded snapshot and warms it
// from persistent settings only when no snapshot was ever loaded. Production wiring
// loads the pair at startup, so the request path does not read the database; the
// warm-up exists so an unwired process still honors persisted switches instead of
// silently reporting off/off forever.
func (s *SettingService) loadedAntigravityThoughtPolicySettings(ctx context.Context) AntigravityThoughtPolicySettings {
	if snapshot := antigravityThoughtPolicySnapshot.Load(); snapshot != nil {
		return *snapshot
	}
	if s == nil || s.settingRepo == nil {
		return DefaultAntigravityThoughtPolicySettings()
	}
	if err := s.LoadAntigravityThoughtPolicySettings(ctx); err != nil {
		logger.LegacyPrintf("service.antigravity_thought_policy", "warm-up failed, using seed suppress_thought_only_stop=false continue_thought_only=false: %v", err)
		return DefaultAntigravityThoughtPolicySettings()
	}
	return currentAntigravityThoughtPolicySettings()
}

func effectiveAntigravityThoughtPolicy(global AntigravityThoughtPolicySettings, account *Account) AntigravityThoughtPolicy {
	// The two types share one field layout by design: the conversion keeps the global
	// pair and the resolved policy from drifting apart.
	policy := AntigravityThoughtPolicy(global)
	if account == nil {
		return policy
	}
	if value, ok := antigravityThoughtPolicyOverride(account.Extra, AntigravitySuppressThoughtOnlyStopExtraKey); ok {
		policy.SuppressThoughtOnlyStop = value
	}
	if value, ok := antigravityThoughtPolicyOverride(account.Extra, AntigravityContinueThoughtOnlyExtraKey); ok {
		policy.ContinueThoughtOnly = value
	}
	return policy
}

// antigravityThoughtPolicyOverride reports an account's explicit per-account
// override. Only a real boolean is authoritative, so an explicit false still wins
// over a global true. A missing key, a JSON null, and values of any other type
// inherit the global switch instead of guessing.
func antigravityThoughtPolicyOverride(extra map[string]any, key string) (bool, bool) {
	if extra == nil {
		return false, false
	}
	raw, exists := extra[key]
	if !exists || raw == nil {
		return false, false
	}
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return false, false
		}
		return parsed, true
	default:
		return false, false
	}
}
