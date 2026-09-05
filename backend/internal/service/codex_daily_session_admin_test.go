package service

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

type dailyPoolAdminRepo struct {
	AccountRepository
	accounts map[int64]*Account
	writes   int
}

func (r *dailyPoolAdminRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return codexDailySessionUpdatedAccount(r.accounts[id], nil, nil), nil
}
func (r *dailyPoolAdminRepo) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	var result []*Account
	for _, id := range ids {
		account, err := r.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, account)
	}
	return result, nil
}
func (r *dailyPoolAdminRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	result := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Status == StatusActive {
			result = append(result, *account)
		}
	}
	return result, nil
}
func (r *dailyPoolAdminRepo) ListAllWithFilters(_ context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, error) {
	if platform != PlatformOpenAI || accountType != "" || status != "" || search != "" || groupID != 0 || privacyMode != "" {
		panic("unexpected daily pool alias filters")
	}
	result := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		result = append(result, *account)
	}
	return result, nil
}
func (r *dailyPoolAdminRepo) UpdateExtra(_ context.Context, id int64, extra map[string]any) error {
	maps.Copy(r.accounts[id].Extra, extra)
	r.writes++
	return nil
}
func (r *dailyPoolAdminRepo) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	for _, id := range ids {
		maps.Copy(r.accounts[id].Extra, updates.Extra)
	}
	r.writes++
	return int64(len(ids)), nil
}

func TestCodexDailyPoolAdminExtraAndBulkConflict(t *testing.T) {
	first := dailyPoolAccount()
	second := dailyPoolAccount()
	second.ID = 102
	first.Status, second.Status = StatusActive, StatusActive
	repo := &dailyPoolAdminRepo{accounts: map[int64]*Account{first.ID: first, second.ID: second}}
	svc := &adminServiceImpl{accountRepo: repo}
	ctx := context.Background()
	err := svc.UpdateAccountExtra(ctx, first.ID, map[string]any{CodexDailySessionMaxKey: 20})
	require.ErrorContains(t, err, "identical daily session")
	require.Zero(t, repo.writes)
	err = svc.UpdateAccountExtra(ctx, first.ID, map[string]any{CodexDailySessionMinKey: 0})
	require.Error(t, err)
	require.Zero(t, repo.writes)
	result, err := svc.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{first.ID, second.ID}, Extra: map[string]any{CodexDailySessionMaxKey: 20}})
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, 1, repo.writes)
	require.Equal(t, 20, first.Extra[CodexDailySessionMaxKey])
	require.Equal(t, 20, second.Extra[CodexDailySessionMaxKey])
	shadow := dailyPoolAccount()
	shadow.ID = 103
	shadow.ParentAccountID = &first.ID
	shadow.Extra = map[string]any{codexFingerprintModeExtraKey: "session"}
	repo.accounts[shadow.ID] = shadow
	err = svc.UpdateAccountExtra(ctx, shadow.ID, map[string]any{CodexDailySessionEnabledKey: false})
	require.ErrorContains(t, err, "parent account")
	require.Equal(t, 1, repo.writes)
	err = svc.UpdateAccountExtra(ctx, shadow.ID, map[string]any{codexFingerprintModeExtraKey: "off", CodexDailySessionEnabledKey: false})
	require.NoError(t, err)
	require.Equal(t, 2, repo.writes)
	err = svc.UpdateAccountExtra(ctx, first.ID, map[string]any{CodexDailySessionEnabledKey: false})
	require.NoError(t, err)
	require.Equal(t, 3, repo.writes)
}
