package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

const SettingKeyCodexSessionAffinity = "codex_session_affinity"

const maxCodexSessionAffinityGroupIDs = 1000

var ErrInvalidCodexSessionAffinity = errors.New("invalid Codex session affinity settings")

// CodexSessionAffinitySettings selects groups whose new explicit Codex root
// sessions may be enrolled into fixed credential affinity.
type CodexSessionAffinitySettings struct {
	GroupIDs []int64 `json:"group_ids"`
}

func normalizeCodexSessionAffinitySettings(in CodexSessionAffinitySettings) (CodexSessionAffinitySettings, error) {
	if len(in.GroupIDs) > maxCodexSessionAffinityGroupIDs {
		return CodexSessionAffinitySettings{GroupIDs: append([]int64(nil), in.GroupIDs...)}, fmt.Errorf("at most %d group IDs are allowed", maxCodexSessionAffinityGroupIDs)
	}

	out := CodexSessionAffinitySettings{GroupIDs: make([]int64, 0, len(in.GroupIDs))}
	seen := make(map[int64]struct{}, len(in.GroupIDs))
	for _, groupID := range in.GroupIDs {
		if groupID <= 0 {
			return out, fmt.Errorf("group IDs must be positive")
		}
		if _, ok := seen[groupID]; ok {
			return out, fmt.Errorf("duplicate group ID %d", groupID)
		}
		seen[groupID] = struct{}{}
		out.GroupIDs = append(out.GroupIDs, groupID)
	}
	return out, nil
}

func (s *SettingService) GetCodexSessionAffinitySettings(ctx context.Context) (CodexSessionAffinitySettings, error) {
	out := CodexSessionAffinitySettings{GroupIDs: []int64{}}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyCodexSessionAffinity)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return out, err
	}
	if raw == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, err
	}
	return normalizeCodexSessionAffinitySettings(out)
}

// LoadCodexSessionAffinitySettings warms the atomic snapshot used by request
// paths. A failed read leaves the previous snapshot untouched.
func (s *SettingService) LoadCodexSessionAffinitySettings(ctx context.Context) error {
	s.codexSessionAffinityMu.Lock()
	defer s.codexSessionAffinityMu.Unlock()
	value, err := s.GetCodexSessionAffinitySettings(ctx)
	if err != nil {
		return err
	}
	s.codexSessionAffinityCache.Store(value)
	return nil
}

// SetCodexSessionAffinitySettings persists a validated snapshot before
// publishing it to request-path readers. A write failure therefore preserves
// the previous in-memory decision.
func (s *SettingService) SetCodexSessionAffinitySettings(ctx context.Context, value CodexSessionAffinitySettings) error {
	s.codexSessionAffinityMu.Lock()
	defer s.codexSessionAffinityMu.Unlock()
	normalized, err := normalizeCodexSessionAffinitySettings(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCodexSessionAffinity, err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	if err := s.settingRepo.Set(ctx, SettingKeyCodexSessionAffinity, string(raw)); err != nil {
		return err
	}
	s.codexSessionAffinityCache.Store(normalized)
	return nil
}

// IsCodexSessionAffinityEnabled reads only the preloaded atomic snapshot; it
// never queries persistent settings on the request path.
func (s *SettingService) IsCodexSessionAffinityEnabled(groupID *int64) bool {
	if s == nil || groupID == nil || *groupID <= 0 {
		return false
	}
	settings, _ := s.codexSessionAffinityCache.Load().(CodexSessionAffinitySettings)
	for _, selectedID := range settings.GroupIDs {
		if selectedID == *groupID {
			return true
		}
	}
	return false
}
