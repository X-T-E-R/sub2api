package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetModelTestSettingsDefaultsWhenUnconfigured(t *testing.T) {
	repo := &codexSessionAffinitySettingRepoStub{}
	settings, err := NewSettingService(repo, nil).GetModelTestSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, "", settings.Prompt, "an unconfigured prompt must stay empty so reset keeps meaning something")
	require.Equal(t, DefaultModelTestPrompt, settings.DefaultPrompt)
	require.Equal(t, "Say hello in one short sentence.", DefaultModelTestPrompt)

	effective, err := NewSettingService(repo, nil).EffectiveModelTestPrompt(context.Background())
	require.NoError(t, err)
	require.Equal(t, DefaultModelTestPrompt, effective)
}

func TestModelTestPromptRoundTripTrimResetAndEffectiveValue(t *testing.T) {
	ctx := context.Background()
	repo := &codexSessionAffinitySettingRepoStub{}
	svc := NewSettingService(repo, nil)

	saved, err := svc.SetModelTestPrompt(ctx, "  Reply with the single word: pong.  ")
	require.NoError(t, err)
	require.Equal(t, "Reply with the single word: pong.", saved.Prompt)
	require.Equal(t, DefaultModelTestPrompt, saved.DefaultPrompt)
	require.Equal(t, "Reply with the single word: pong.", repo.values[SettingKeyModelTestPrompt])

	stored, err := svc.GetModelTestSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, saved, stored)
	effective, err := svc.EffectiveModelTestPrompt(ctx)
	require.NoError(t, err)
	require.Equal(t, "Reply with the single word: pong.", effective)

	// Whitespace-only input is the same request as an empty prompt: reset.
	reset, err := svc.SetModelTestPrompt(ctx, "   \n ")
	require.NoError(t, err)
	require.Equal(t, "", reset.Prompt)
	require.Equal(t, DefaultModelTestPrompt, reset.DefaultPrompt)
	require.Equal(t, "", repo.values[SettingKeyModelTestPrompt])

	effective, err = svc.EffectiveModelTestPrompt(ctx)
	require.NoError(t, err)
	require.Equal(t, DefaultModelTestPrompt, effective)
}

func TestSetModelTestPromptLimitsRunesNotBytes(t *testing.T) {
	ctx := context.Background()
	repo := &codexSessionAffinitySettingRepoStub{values: map[string]string{SettingKeyModelTestPrompt: "kept"}}
	svc := NewSettingService(repo, nil)

	// 8192 astral characters are 32768 bytes: the limit counts Unicode code
	// points, so the same length in bytes must be accepted.
	atLimit := strings.Repeat("😀", MaxModelTestPromptRunes)
	require.Greater(t, len(atLimit), MaxModelTestPromptRunes)
	saved, err := svc.SetModelTestPrompt(ctx, atLimit)
	require.NoError(t, err)
	require.Equal(t, atLimit, saved.Prompt)
	require.Equal(t, atLimit, repo.values[SettingKeyModelTestPrompt])

	overLimit := strings.Repeat("😀", MaxModelTestPromptRunes+1)
	_, err = svc.SetModelTestPrompt(ctx, overLimit)
	require.ErrorIs(t, err, ErrInvalidModelTestPrompt)
	require.Equal(t, atLimit, repo.values[SettingKeyModelTestPrompt], "a rejected prompt must not be persisted")
}

func TestModelTestSettingsExposeOnlyPromptFields(t *testing.T) {
	raw, err := json.Marshal(ModelTestSettings{Prompt: "hi", DefaultPrompt: DefaultModelTestPrompt})
	require.NoError(t, err)

	var keys map[string]any
	require.NoError(t, json.Unmarshal(raw, &keys))
	require.ElementsMatch(t, []string{"prompt", "default_prompt"}, mapKeysOf(keys))
}

func mapKeysOf(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestModelTestSettingsPropagateRepositoryFailures(t *testing.T) {
	ctx := context.Background()

	readErr := errors.New("settings unavailable")
	svc := NewSettingService(&codexSessionAffinitySettingRepoStub{getErr: readErr}, nil)
	_, err := svc.GetModelTestSettings(ctx)
	require.ErrorIs(t, err, readErr)
	_, err = svc.EffectiveModelTestPrompt(ctx)
	require.ErrorIs(t, err, readErr)

	repo := &codexSessionAffinitySettingRepoStub{values: map[string]string{SettingKeyModelTestPrompt: "kept"}}
	writeErr := errors.New("write unavailable")
	repo.writeErr = writeErr
	_, err = NewSettingService(repo, nil).SetModelTestPrompt(ctx, "replacement")
	require.ErrorIs(t, err, writeErr)
	require.Equal(t, "kept", repo.values[SettingKeyModelTestPrompt])
}

func TestModelTestSettingsWithoutRepositoryUseTheBuiltInDefault(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(nil, nil)

	settings, err := svc.GetModelTestSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, "", settings.Prompt)
	require.Equal(t, DefaultModelTestPrompt, settings.DefaultPrompt)

	_, err = svc.SetModelTestPrompt(ctx, "unreachable")
	require.Error(t, err)
}
