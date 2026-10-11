package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// SettingKeyModelTestPrompt stores the operator's global prompt for ordinary
// account connectivity tests. A blank stored value means "no custom prompt", so
// the built-in default is never copied into the database and a later change to
// DefaultModelTestPrompt reaches every account that never customized it.
const SettingKeyModelTestPrompt = "model_test_prompt"

// DefaultModelTestPrompt is the single built-in prompt for ordinary text account
// tests: the text an unconfigured test sends, and the value reported back as
// default_prompt.
const DefaultModelTestPrompt = "Say hello in one short sentence."

// MaxModelTestPromptRunes bounds the configured prompt in Unicode code points,
// so an astral character (emoji) counts once, exactly like the admin UI counter.
const MaxModelTestPromptRunes = 8192

// ErrInvalidModelTestPrompt marks operator input the settings handler must
// answer with 400 instead of 500.
var ErrInvalidModelTestPrompt = errors.New("invalid model test prompt settings")

// ModelTestSettings is the whole body of GET/PUT /admin/settings/model-test.
//
// Prompt is the operator-configured text and stays empty while no custom prompt
// is stored — an empty prompt is what makes ordinary tests fall back to
// DefaultPrompt, so echoing the default back as Prompt would erase the
// difference between "reset" and "saved the default text on purpose". The
// struct deliberately carries no credential, upstream, or provider material.
type ModelTestSettings struct {
	Prompt        string `json:"prompt"`
	DefaultPrompt string `json:"default_prompt"`
}

// normalizeModelTestPrompt trims surrounding whitespace and reports whitespace
// only input as blank, i.e. as "no custom prompt".
func normalizeModelTestPrompt(raw string) string {
	return strings.TrimSpace(raw)
}

// GetModelTestSettings reads the stored prompt together with the built-in
// default. A missing row is the ordinary unconfigured state, not an error, and
// no request path may read a credential through it.
func (s *SettingService) GetModelTestSettings(ctx context.Context) (ModelTestSettings, error) {
	out := defaultModelTestSettings()
	if s == nil || s.settingRepo == nil {
		return out, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyModelTestPrompt)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return out, err
	}
	out.Prompt = normalizeModelTestPrompt(raw)
	return out, nil
}

// EffectiveModelTestPrompt resolves the prompt an ordinary account test must
// send: the configured text, or the built-in default while nothing is stored.
func (s *SettingService) EffectiveModelTestPrompt(ctx context.Context) (string, error) {
	settings, err := s.GetModelTestSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.Prompt == "" {
		return settings.DefaultPrompt, nil
	}
	return settings.Prompt, nil
}

// SetModelTestPrompt validates and persists the operator prompt and returns the
// settings a following GET would report. A blank prompt clears the stored value
// so the built-in default applies again; validation happens before any write, so
// a rejected prompt leaves the previous value in place.
func (s *SettingService) SetModelTestPrompt(ctx context.Context, prompt string) (ModelTestSettings, error) {
	if s == nil || s.settingRepo == nil {
		return ModelTestSettings{}, errors.New("setting repository is not configured")
	}
	prompt = normalizeModelTestPrompt(prompt)
	if prompt != "" {
		if !utf8.ValidString(prompt) {
			return ModelTestSettings{}, fmt.Errorf("%w: prompt must be valid UTF-8", ErrInvalidModelTestPrompt)
		}
		if utf8.RuneCountInString(prompt) > MaxModelTestPromptRunes {
			return ModelTestSettings{}, fmt.Errorf("%w: prompt must contain at most %d Unicode characters", ErrInvalidModelTestPrompt, MaxModelTestPromptRunes)
		}
	}
	if err := s.settingRepo.Set(ctx, SettingKeyModelTestPrompt, prompt); err != nil {
		return ModelTestSettings{}, err
	}
	return s.GetModelTestSettings(ctx)
}

func defaultModelTestSettings() ModelTestSettings {
	return ModelTestSettings{DefaultPrompt: DefaultModelTestPrompt}
}
