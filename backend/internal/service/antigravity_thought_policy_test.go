package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- stub: only the two read/write calls the thought policy may use ---

type antigravityThoughtPolicySettingRepo struct {
	SettingRepository

	values map[string]string
	writes []string
	getErr error
	setErr error
}

func newThoughtPolicySettingRepo() *antigravityThoughtPolicySettingRepo {
	return &antigravityThoughtPolicySettingRepo{values: map[string]string{}}
}

func (r *antigravityThoughtPolicySettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	return r.values[key], nil
}

func (r *antigravityThoughtPolicySettingRepo) Set(_ context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.values[key] = value
	r.writes = append(r.writes, key)
	return nil
}

// thoughtPolicyTestSettings starts from a cold process: no snapshot is published
// yet, so a test observes exactly what the resolver derives. The previous snapshot
// is restored afterwards because it is package state shared with other tests.
func thoughtPolicyTestSettings(t *testing.T, repo *antigravityThoughtPolicySettingRepo) *SettingService {
	t.Helper()
	previous := antigravityThoughtPolicySnapshot.Load()
	antigravityThoughtPolicySnapshot.Store(nil)
	t.Cleanup(func() { antigravityThoughtPolicySnapshot.Store(previous) })
	if repo == nil {
		repo = newThoughtPolicySettingRepo()
	}
	return NewSettingService(repo, nil)
}

func thoughtPolicyCtx() context.Context { return context.Background() }

// Source defaults are off/off and everything the account does not state explicitly
// inherits: missing keys, JSON null and values that are not booleans.
func TestAntigravityThoughtPolicyDefaultsOffAndInherits(t *testing.T) {
	settings := thoughtPolicyTestSettings(t, nil)

	global, err := settings.GetAntigravityThoughtPolicySettings(thoughtPolicyCtx())
	require.NoError(t, err)
	require.Equal(t, DefaultAntigravityThoughtPolicySettings(), global)
	require.False(t, global.SuppressThoughtOnlyStop)
	require.False(t, global.ContinueThoughtOnly)

	for name, account := range map[string]*Account{
		"nil account": nil,
		"no extra":    {ID: 1},
		"null values": {ID: 2, Extra: map[string]any{
			AntigravitySuppressThoughtOnlyStopExtraKey: nil,
			AntigravityContinueThoughtOnlyExtraKey:     nil,
		}},
		"wrong types": {ID: 3, Extra: map[string]any{
			AntigravitySuppressThoughtOnlyStopExtraKey: "yes",
			AntigravityContinueThoughtOnlyExtraKey:     float64(1),
		}},
	} {
		require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(thoughtPolicyCtx(), account), name)
		require.Equal(t, AntigravityThoughtPolicy{}, ResolveAntigravityThoughtPolicy(thoughtPolicyCtx(), account), name)
	}

	// A service without a repository still reports the seed instead of panicking.
	bare := NewSettingService(nil, nil)
	bareGlobal, err := bare.GetAntigravityThoughtPolicySettings(thoughtPolicyCtx())
	require.NoError(t, err)
	require.Equal(t, DefaultAntigravityThoughtPolicySettings(), bareGlobal)
	require.Equal(t, AntigravityThoughtPolicy{}, bare.ResolveAntigravityThoughtPolicy(thoughtPolicyCtx(), &Account{ID: 4}))
	require.Error(t, bare.SetAntigravityThoughtPolicySettings(thoughtPolicyCtx(), DefaultAntigravityThoughtPolicySettings()))

	// Request paths that hold no SettingService (gateway test wiring) must not panic.
	var missing *SettingService
	require.Equal(t, AntigravityThoughtPolicy{}, missing.ResolveAntigravityThoughtPolicy(thoughtPolicyCtx(), &Account{ID: 5}))
}

// An account override wins per switch, and an explicit false is authoritative over a
// global true. Truthy strings are accepted; anything else inherits.
func TestAntigravityThoughtPolicyAccountExplicitValuesWin(t *testing.T) {
	settings := thoughtPolicyTestSettings(t, nil)
	ctx := thoughtPolicyCtx()

	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(ctx, AntigravityThoughtPolicySettings{
		SuppressThoughtOnlyStop: true,
		ContinueThoughtOnly:     true,
	}))

	for name, tc := range map[string]struct {
		extra    map[string]any
		expected AntigravityThoughtPolicy
	}{
		"inherits both": {nil, AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}},
		"explicit false overrides global true": {
			extra: map[string]any{
				AntigravitySuppressThoughtOnlyStopExtraKey: false,
				AntigravityContinueThoughtOnlyExtraKey:     false,
			},
			expected: AntigravityThoughtPolicy{},
		},
		"suppress off only": {
			extra:    map[string]any{AntigravitySuppressThoughtOnlyStopExtraKey: false},
			expected: AntigravityThoughtPolicy{ContinueThoughtOnly: true},
		},
		"continue off only": {
			extra:    map[string]any{AntigravityContinueThoughtOnlyExtraKey: false},
			expected: AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true},
		},
		"null suppress still inherits while continue is off": {
			extra: map[string]any{
				AntigravitySuppressThoughtOnlyStopExtraKey: nil,
				AntigravityContinueThoughtOnlyExtraKey:     false,
			},
			expected: AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true},
		},
		"string false overrides global true": {
			extra:    map[string]any{AntigravityContinueThoughtOnlyExtraKey: " false "},
			expected: AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true},
		},
	} {
		require.Equal(t, tc.expected, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 9, Extra: tc.extra}), name)
		require.Equal(t, tc.expected, ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 9, Extra: tc.extra}), name)
	}

	// The mirror case: an explicit true turns a switch on while the global pair is off.
	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(ctx, DefaultAntigravityThoughtPolicySettings()))
	enabled := &Account{ID: 10, Extra: map[string]any{
		AntigravitySuppressThoughtOnlyStopExtraKey: true,
		AntigravityContinueThoughtOnlyExtraKey:     "true",
	}}
	require.Equal(t, AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, settings.ResolveAntigravityThoughtPolicy(ctx, enabled))

	// Resolution is per account: another account is unaffected.
	require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 11}))
}

// Both switches share exactly one settings key, and a cold process reads the pair back.
func TestAntigravityThoughtPolicyPersistsOneJSONKeyAndReadsBack(t *testing.T) {
	repo := newThoughtPolicySettingRepo()
	settings := thoughtPolicyTestSettings(t, repo)
	ctx := thoughtPolicyCtx()

	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(ctx, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}))
	require.Equal(t, []string{SettingKeyAntigravityThoughtPolicy}, repo.writes)
	require.Equal(t, `{"suppress_thought_only_stop":true,"continue_thought_only":false,"continue_zero_output":false}`, repo.values[SettingKeyAntigravityThoughtPolicy])
	require.Len(t, repo.values, 1, "the two switches must share the single global key")

	// A fresh process has no snapshot and must warm it from the persisted pair.
	antigravityThoughtPolicySnapshot.Store(nil)
	fresh := NewSettingService(repo, nil)
	readBack, err := fresh.GetAntigravityThoughtPolicySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, readBack)
	require.Equal(t, AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true}, fresh.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}))

	// Explicitly stored false/false is a real value, not an absent key.
	require.NoError(t, fresh.SetAntigravityThoughtPolicySettings(ctx, AntigravityThoughtPolicySettings{}))
	require.Equal(t, `{"suppress_thought_only_stop":false,"continue_thought_only":false,"continue_zero_output":false}`, repo.values[SettingKeyAntigravityThoughtPolicy])
	require.Equal(t, AntigravityThoughtPolicy{}, ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}))
}

// One request keeps the pair it started with, including through retries.
func TestAntigravityThoughtPolicyRequestSnapshotIsPinned(t *testing.T) {
	settings := thoughtPolicyTestSettings(t, nil)
	pinnedCtx := WithAntigravityThoughtPolicySnapshot(thoughtPolicyCtx())

	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(thoughtPolicyCtx(), AntigravityThoughtPolicySettings{
		SuppressThoughtOnlyStop: true,
		ContinueThoughtOnly:     true,
	}))

	require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(pinnedCtx, &Account{ID: 1}))
	require.Equal(t, AntigravityThoughtPolicy{}, ResolveAntigravityThoughtPolicy(pinnedCtx, &Account{ID: 1}))

	// Account overrides still apply inside the pinned request.
	overridden := &Account{ID: 2, Extra: map[string]any{AntigravityContinueThoughtOnlyExtraKey: true}}
	require.Equal(t, AntigravityThoughtPolicy{ContinueThoughtOnly: true}, settings.ResolveAntigravityThoughtPolicy(pinnedCtx, overridden))

	// A new request observes the update, and re-pinning keeps the original pair.
	require.Equal(t, AntigravityThoughtPolicy{SuppressThoughtOnlyStop: true, ContinueThoughtOnly: true}, settings.ResolveAntigravityThoughtPolicy(thoughtPolicyCtx(), &Account{ID: 1}))
	require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(WithAntigravityThoughtPolicySnapshot(pinnedCtx), &Account{ID: 1}))
}

func TestAntigravityThoughtPolicyFailuresKeepPublishedPair(t *testing.T) {
	repo := newThoughtPolicySettingRepo()
	settings := thoughtPolicyTestSettings(t, repo)
	ctx := thoughtPolicyCtx()

	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(ctx, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}))

	repo.setErr = errors.New("write unavailable")
	require.Error(t, settings.SetAntigravityThoughtPolicySettings(ctx, DefaultAntigravityThoughtPolicySettings()))
	require.Equal(t, AntigravityThoughtPolicy{ContinueThoughtOnly: true}, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}), "failed persistence must not publish")

	repo.setErr = nil
	repo.getErr = errors.New("read unavailable")
	require.Error(t, settings.LoadAntigravityThoughtPolicySettings(ctx))
	require.Equal(t, AntigravityThoughtPolicy{ContinueThoughtOnly: true}, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}), "a failed reload keeps the published pair")
	_, err := settings.GetAntigravityThoughtPolicySettings(ctx)
	require.Error(t, err)

	// Cold process with an unreadable store reports the seed instead of failing the request.
	antigravityThoughtPolicySnapshot.Store(nil)
	require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}))
}

func TestAntigravityThoughtPolicyCorruptStoredValueIsReported(t *testing.T) {
	repo := newThoughtPolicySettingRepo()
	settings := thoughtPolicyTestSettings(t, repo)
	ctx := thoughtPolicyCtx()

	repo.values[SettingKeyAntigravityThoughtPolicy] = `{"suppress_thought_only_stop":"on"`
	_, err := settings.GetAntigravityThoughtPolicySettings(ctx)
	require.Error(t, err, "an unreadable stored pair must be reported, never silently treated as off/off")
	require.Equal(t, AntigravityThoughtPolicy{}, settings.ResolveAntigravityThoughtPolicy(ctx, &Account{ID: 1}), "a request falls back to the seed rather than failing")

	// An empty stored value is the same situation as an absent key.
	repo.values[SettingKeyAntigravityThoughtPolicy] = ""
	global, err := settings.GetAntigravityThoughtPolicySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, DefaultAntigravityThoughtPolicySettings(), global)

	// A stored pair written by this API reads back verbatim.
	repo.values[SettingKeyAntigravityThoughtPolicy] = `{"continue_thought_only":true,"suppress_thought_only_stop":false}`
	global, err = settings.GetAntigravityThoughtPolicySettings(ctx)
	require.NoError(t, err)
	require.Equal(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, global)
}

func TestAntigravityZeroOutputContinueGlobalSnapshotAndLegacySettings(t *testing.T) {
	repo := newThoughtPolicySettingRepo()
	settings := thoughtPolicyTestSettings(t, repo)
	ctx := context.Background()
	repo.values[SettingKeyAntigravityThoughtPolicy] = `{"suppress_thought_only_stop":true,"continue_thought_only":false}`
	require.NoError(t, settings.LoadAntigravityThoughtPolicySettings(ctx))
	require.False(t, ResolveAntigravityThoughtPolicy(ctx, nil).ContinueZeroOutput)
	pinned := WithAntigravityThoughtPolicySnapshot(ctx)
	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(ctx, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: true}))
	require.False(t, ResolveAntigravityThoughtPolicy(pinned, nil).ContinueZeroOutput)
	require.True(t, ResolveAntigravityThoughtPolicy(ctx, &Account{Extra: map[string]any{"antigravity_continue_zero_output": false, AntigravitySuppressThoughtOnlyStopExtraKey: false}}).ContinueZeroOutput)
	antigravityThoughtPolicySnapshot.Store(nil)
	require.NoError(t, settings.LoadAntigravityThoughtPolicySettings(ctx))
	require.True(t, ResolveAntigravityThoughtPolicy(ctx, nil).ContinueZeroOutput)
}
