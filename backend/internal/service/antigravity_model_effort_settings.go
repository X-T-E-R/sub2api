package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const SettingKeyAntigravityModelEffort = "antigravity_model_effort"

var ErrInvalidAntigravityModelEffort = errors.New("invalid Antigravity model effort settings")

type AntigravityModelEffortSettings struct {
	Levels        []string `json:"levels"`
	DefaultEffort string   `json:"default_effort"`
}

var antigravityEffortName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var antigravityEffortSettingsSnapshot atomic.Pointer[AntigravityModelEffortSettings]
var antigravityEffortSettingsMu sync.Mutex

func defaultAntigravityModelEffortSettings() AntigravityModelEffortSettings {
	return AntigravityModelEffortSettings{Levels: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, DefaultEffort: "medium"}
}

func normalizeAntigravityModelEffortSettings(in AntigravityModelEffortSettings) (AntigravityModelEffortSettings, error) {
	out := AntigravityModelEffortSettings{Levels: make([]string, 0, len(in.Levels)), DefaultEffort: strings.ToLower(strings.TrimSpace(in.DefaultEffort))}
	if len(in.Levels) == 0 || len(in.Levels) > 64 {
		return out, fmt.Errorf("levels must contain 1 to 64 names")
	}
	seen := map[string]bool{}
	for _, value := range in.Levels {
		value = strings.ToLower(strings.TrimSpace(value))
		if len(value) > 64 || !antigravityEffortName.MatchString(value) || seen[value] {
			return out, fmt.Errorf("effort names must be unique lowercase tokens of at most 64 characters")
		}
		seen[value] = true
		out.Levels = append(out.Levels, value)
	}
	if !seen[out.DefaultEffort] {
		return out, fmt.Errorf("default_effort must belong to levels")
	}
	// Longest first makes composite names such as extra-low unambiguous.
	sort.Slice(out.Levels, func(i, j int) bool {
		if len(out.Levels[i]) != len(out.Levels[j]) {
			return len(out.Levels[i]) > len(out.Levels[j])
		}
		return out.Levels[i] < out.Levels[j]
	})
	return out, nil
}

func currentAntigravityModelEffortSettings() *AntigravityModelEffortSettings {
	if snapshot := antigravityEffortSettingsSnapshot.Load(); snapshot != nil {
		return snapshot
	}
	value, _ := normalizeAntigravityModelEffortSettings(defaultAntigravityModelEffortSettings())
	return &value
}

func (p *AntigravityModelEffortSettings) supports(level string) bool {
	for _, value := range p.Levels {
		if value == level {
			return true
		}
	}
	return false
}

type antigravityEffortPolicyKey struct{}

func antigravityEffortPolicyFromContext(ctx context.Context) *AntigravityModelEffortSettings {
	if ctx != nil {
		if p, ok := ctx.Value(antigravityEffortPolicyKey{}).(*AntigravityModelEffortSettings); ok {
			return p
		}
	}
	return currentAntigravityModelEffortSettings()
}

func (s *SettingService) GetAntigravityModelEffortSettings(ctx context.Context) (AntigravityModelEffortSettings, error) {
	value, _, err := s.readAntigravityModelEffortSettings(ctx)
	return value, err
}

func (s *SettingService) readAntigravityModelEffortSettings(ctx context.Context) (AntigravityModelEffortSettings, string, error) {
	out := defaultAntigravityModelEffortSettings()
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAntigravityModelEffort)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return out, "settings", err
	}
	source := "seed"
	if raw != "" {
		source = "settings"
		out = AntigravityModelEffortSettings{}
		if err = json.Unmarshal([]byte(raw), &out); err != nil {
			return out, source, err
		}
	}
	normalized, err := normalizeAntigravityModelEffortSettings(out)
	return normalized, source, err
}

func (s *SettingService) LoadAntigravityModelEffortSettings(ctx context.Context) error {
	antigravityEffortSettingsMu.Lock()
	defer antigravityEffortSettingsMu.Unlock()
	value, source, err := s.readAntigravityModelEffortSettings(ctx)
	if err != nil {
		return err
	}
	antigravityEffortSettingsSnapshot.Store(&value)
	logger.LegacyPrintf("service.antigravity_model_effort", "loaded key=%s source=%s count=%d default=%s", SettingKeyAntigravityModelEffort, source, len(value.Levels), value.DefaultEffort)
	return nil
}

func (s *SettingService) SetAntigravityModelEffortSettings(ctx context.Context, value AntigravityModelEffortSettings) error {
	antigravityEffortSettingsMu.Lock()
	defer antigravityEffortSettingsMu.Unlock()
	normalized, err := normalizeAntigravityModelEffortSettings(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAntigravityModelEffort, err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	if err = s.settingRepo.Set(ctx, SettingKeyAntigravityModelEffort, string(raw)); err != nil {
		return err
	}
	antigravityEffortSettingsSnapshot.Store(&normalized)
	logger.LegacyPrintf("service.antigravity_model_effort", "updated key=%s source=settings count=%d default=%s", SettingKeyAntigravityModelEffort, len(normalized.Levels), normalized.DefaultEffort)
	return nil
}
