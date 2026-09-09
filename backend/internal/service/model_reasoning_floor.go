package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const SettingKeyModelReasoningFloor = "openai_model_reasoning_floor"

var ErrInvalidModelReasoningFloor = errors.New("invalid model reasoning settings")

type ModelReasoningFloorRule struct {
	Model     string `json:"model"`
	MinEffort string `json:"min_effort"`
}

type ModelReasoningFloorSettings struct {
	Enabled bool                      `json:"enabled"`
	Rules   []ModelReasoningFloorRule `json:"rules"`
}

func normalizeModelReasoningFloor(in ModelReasoningFloorSettings) (ModelReasoningFloorSettings, error) {
	if len(in.Rules) > 64 {
		return in, fmt.Errorf("at most 64 model reasoning rules are allowed")
	}
	out := ModelReasoningFloorSettings{Enabled: in.Enabled, Rules: make([]ModelReasoningFloorRule, 0, len(in.Rules))}
	seen := make(map[string]bool)
	for _, rule := range in.Rules {
		rule.Model = strings.TrimSpace(rule.Model)
		if rule.Model == "" || len(rule.Model) > 128 || strings.ContainsAny(rule.Model, " \t\r\n*?") {
			return out, fmt.Errorf("model must be an exact model ID of at most 128 characters")
		}
		rule.MinEffort = NormalizeMaxReasoningEffort(rule.MinEffort)
		if rule.MinEffort == "" {
			return out, fmt.Errorf("minimum effort must be minimal, low, medium, high, xhigh or max")
		}
		if seen[rule.Model] {
			return out, fmt.Errorf("duplicate model rule %q", rule.Model)
		}
		seen[rule.Model] = true
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}

func (s *SettingService) GetModelReasoningFloorSettings(ctx context.Context) (ModelReasoningFloorSettings, error) {
	out := ModelReasoningFloorSettings{Rules: []ModelReasoningFloorRule{}}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyModelReasoningFloor)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return out, err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return out, err
		}
	}
	return normalizeModelReasoningFloor(out)
}

func (s *SettingService) LoadModelReasoningFloorSettings(ctx context.Context) error {
	s.modelReasoningFloorMu.Lock()
	defer s.modelReasoningFloorMu.Unlock()
	value, err := s.GetModelReasoningFloorSettings(ctx)
	if err != nil {
		return err
	}
	s.modelReasoningFloorCache.Store(value)
	return nil
}

func (s *SettingService) SetModelReasoningFloorSettings(ctx context.Context, value ModelReasoningFloorSettings) error {
	s.modelReasoningFloorMu.Lock()
	defer s.modelReasoningFloorMu.Unlock()
	normalized, err := normalizeModelReasoningFloor(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidModelReasoningFloor, err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	if err := s.settingRepo.Set(ctx, SettingKeyModelReasoningFloor, string(raw)); err != nil {
		return err
	}
	s.modelReasoningFloorCache.Store(normalized)
	return nil
}

// Model rules are applied after the existing group mapping/ceiling. Runtime
// reads only the snapshot loaded at startup or by the administrative update.
func (s *OpenAIGatewayService) applyModelReasoningFloor(account *Account, model string, body []byte, path string) []byte {
	if s == nil || s.settingService == nil || account == nil || account.Platform != PlatformOpenAI {
		return body
	}
	settings, _ := s.settingService.modelReasoningFloorCache.Load().(ModelReasoningFloorSettings)
	return applyModelReasoningFloor(body, model, path, settings)
}

func applyModelReasoningFloor(body []byte, model, path string, settings ModelReasoningFloorSettings) []byte {
	if !settings.Enabled {
		return body
	}
	minimum := ""
	for _, rule := range settings.Rules {
		if rule.Model == model {
			minimum = rule.MinEffort
			break
		}
	}
	minRank, ok := reasoningEffortRank(minimum)
	if !ok {
		return body
	}
	// Honor either accepted spelling, including mixed compatibility payloads.
	// Add the protocol's canonical field only when no explicit effort exists.
	other := "reasoning_effort"
	if path == other {
		other = "reasoning.effort"
	}
	paths := []string{path}
	if gjson.GetBytes(body, other).Exists() {
		if !gjson.GetBytes(body, path).Exists() {
			paths = nil
		}
		paths = append(paths, other)
	}
	for _, field := range paths {
		body = raiseModelReasoningFloorField(body, field, minimum, minRank)
	}
	return body
}

func raiseModelReasoningFloorField(body []byte, path, minimum string, minRank int) []byte {
	if path == "reasoning.effort" {
		parent := gjson.GetBytes(body, "reasoning")
		if parent.Exists() && !parent.IsObject() && parent.Type != gjson.Null {
			return body
		}
	}
	value := gjson.GetBytes(body, path)
	if value.Exists() && value.Type != gjson.String && value.Type != gjson.Null {
		return body
	}
	current := strings.ToLower(strings.TrimSpace(value.String()))
	rank, recognized := reasoningEffortRank(current)
	if current == "" || current == "none" {
		recognized = true
	}
	// Unknown future effort names are not silently downgraded.
	if !recognized || rank >= minRank {
		return body
	}
	next, err := sjson.SetBytes(body, path, minimum)
	if err != nil {
		return body
	}
	return next
}
