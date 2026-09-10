package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type codexSessionAffinitySettingRepoStub struct {
	values   map[string]string
	getErr   error
	writeErr error
}

func (r *codexSessionAffinitySettingRepoStub) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (r *codexSessionAffinitySettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *codexSessionAffinitySettingRepoStub) Set(_ context.Context, key, value string) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *codexSessionAffinitySettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *codexSessionAffinitySettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *codexSessionAffinitySettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *codexSessionAffinitySettingRepoStub) Delete(context.Context, string) error {
	return nil
}

func TestCodexSessionAffinitySettingsPersistenceAndRequestPathCache(t *testing.T) {
	repo := &codexSessionAffinitySettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	value, err := svc.GetCodexSessionAffinitySettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{}, value.GroupIDs)

	groupID := int64(11)
	require.False(t, svc.IsCodexSessionAffinityEnabled(&groupID))
	require.NoError(t, svc.SetCodexSessionAffinitySettings(context.Background(), CodexSessionAffinitySettings{GroupIDs: []int64{11, 23}}))
	require.JSONEq(t, `{"group_ids":[11,23]}`, repo.values[SettingKeyCodexSessionAffinity])
	require.True(t, svc.IsCodexSessionAffinityEnabled(&groupID))
	otherGroupID := int64(24)
	require.False(t, svc.IsCodexSessionAffinityEnabled(&otherGroupID))
	require.False(t, svc.IsCodexSessionAffinityEnabled(nil))

	// The runtime getter must use the snapshot after persistence, even when the
	// repository becomes unavailable afterward.
	repo.getErr = errors.New("request path must not query SQL")
	require.True(t, svc.IsCodexSessionAffinityEnabled(&groupID))
}

func TestCodexSessionAffinitySettingsLoadAndWriteFailurePreserveCache(t *testing.T) {
	repo := &codexSessionAffinitySettingRepoStub{values: map[string]string{
		SettingKeyCodexSessionAffinity: `{"group_ids":[7]}`,
	}}
	svc := NewSettingService(repo, nil)
	require.NoError(t, svc.LoadCodexSessionAffinitySettings(context.Background()))

	selected := int64(7)
	require.True(t, svc.IsCodexSessionAffinityEnabled(&selected))
	repo.writeErr = errors.New("database unavailable")
	require.Error(t, svc.SetCodexSessionAffinitySettings(context.Background(), CodexSessionAffinitySettings{GroupIDs: []int64{9}}))
	require.True(t, svc.IsCodexSessionAffinityEnabled(&selected))
	newGroup := int64(9)
	require.False(t, svc.IsCodexSessionAffinityEnabled(&newGroup))

	repo.getErr = errors.New("database unavailable")
	require.Error(t, svc.LoadCodexSessionAffinitySettings(context.Background()))
	require.True(t, svc.IsCodexSessionAffinityEnabled(&selected))
}

func TestCodexSessionAffinitySettingsValidation(t *testing.T) {
	repo := &codexSessionAffinitySettingRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	for _, groupIDs := range [][]int64{
		{0},
		{-1},
		{3, 3},
		make([]int64, maxCodexSessionAffinityGroupIDs+1),
	} {
		err := svc.SetCodexSessionAffinitySettings(context.Background(), CodexSessionAffinitySettings{GroupIDs: groupIDs})
		require.ErrorIs(t, err, ErrInvalidCodexSessionAffinity)
	}
}
