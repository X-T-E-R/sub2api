package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type antigravityReauthRepo struct {
	AccountRepository // Any full update or quota-clear call is unexpected.
	account           *Account
	calls             []string
	updateErr         error
}

func (r *antigravityReauthRepo) GetByID(context.Context, int64) (*Account, error) {
	copy := *r.account
	return &copy, nil
}
func (r *antigravityReauthRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.calls = append(r.calls, "credentials")
	if r.updateErr != nil {
		return r.updateErr
	}
	r.account.Credentials = credentials
	return nil
}
func (r *antigravityReauthRepo) ClearError(context.Context, int64) error {
	r.calls = append(r.calls, "error")
	r.account.Status = StatusActive
	r.account.ErrorMessage = ""
	return nil
}
func (r *antigravityReauthRepo) SetSchedulable(_ context.Context, _ int64, value bool) error {
	r.calls = append(r.calls, "schedulable")
	r.account.Schedulable = value
	return nil
}

func TestApplyAntigravityOAuthCredentialsPreservesConfigurationAndQuota(t *testing.T) {
	for _, tc := range []struct {
		name, status        string
		schedulable         bool
		expectedStatus      string
		expectedSchedulable bool
		expectedCalls       []string
	}{
		{"auth error", StatusError, false, StatusActive, true, []string{"credentials", "error", "schedulable"}},
		{"active", StatusActive, true, StatusActive, true, []string{"credentials"}},
		{"manually paused", StatusActive, false, StatusActive, false, []string{"credentials"}},
		{"disabled", StatusDisabled, false, StatusDisabled, false, []string{"credentials"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset := time.Now().Add(24 * time.Hour)
			proxyID := int64(2)
			extra := map[string]any{
				"model_rate_limits":        map[string]any{"gemini-3.8-flash": map[string]any{"rate_limit_reset_at": reset.Format(time.RFC3339), "source": "real429"}},
				"antigravity_quota_scopes": map[string]any{"gemini": true},
				"privacy_mode":             "private", "custom_setting": "keep",
			}
			mapping := map[string]any{"gemini-4-argon": "gemini-4-argon-{effort}"}
			repo := &antigravityReauthRepo{account: &Account{
				ID: 73, Name: "nia", Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Status: tc.status, ErrorMessage: "old auth error", Schedulable: tc.schedulable,
				ProxyID: &proxyID, GroupIDs: []int64{8}, Extra: extra,
				RateLimitedAt: &reset, RateLimitResetAt: &reset, OverloadUntil: &reset,
				TempUnschedulableUntil: &reset, TempUnschedulableReason: "real cooldown",
				Credentials: map[string]any{"access_token": "old-synthetic", "refresh_token": "saved-synthetic", "model_mapping": mapping, "base_url": "https://example.test", "project_id": "project"},
			}}
			expectedExtra, err := json.Marshal(extra)
			require.NoError(t, err)
			svc := &adminServiceImpl{accountRepo: repo}
			got, err := svc.ApplyAntigravityOAuthCredentials(context.Background(), 73, map[string]any{"access_token": "new-synthetic", "expires_at": "12345"})
			require.NoError(t, err)
			require.Equal(t, tc.expectedCalls, repo.calls)
			require.Equal(t, tc.expectedStatus, got.Status)
			require.Equal(t, tc.expectedSchedulable, got.Schedulable)
			if tc.status == StatusError {
				require.Empty(t, got.ErrorMessage)
			}
			require.Equal(t, int64(73), got.ID)
			require.Equal(t, []int64{8}, got.GroupIDs)
			require.Equal(t, &proxyID, got.ProxyID)
			actualExtra, err := json.Marshal(got.Extra)
			require.NoError(t, err)
			require.Equal(t, string(expectedExtra), string(actualExtra))
			require.Equal(t, &reset, got.RateLimitResetAt)
			require.Equal(t, &reset, got.RateLimitedAt)
			require.Equal(t, &reset, got.OverloadUntil)
			require.Equal(t, &reset, got.TempUnschedulableUntil)
			require.Equal(t, "real cooldown", got.TempUnschedulableReason)
			require.Equal(t, mapping, got.Credentials["model_mapping"])
			require.Equal(t, "https://example.test", got.Credentials["base_url"])
			require.Equal(t, "saved-synthetic", got.Credentials["refresh_token"])
			require.Equal(t, "project", got.Credentials["project_id"])
			require.Equal(t, "new-synthetic", got.Credentials["access_token"])
		})
	}
}

func TestApplyAntigravityOAuthCredentialsWriteFailureDoesNotRecover(t *testing.T) {
	repo := &antigravityReauthRepo{account: &Account{ID: 73, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusError}, updateErr: errors.New("write failed")}
	svc := &adminServiceImpl{accountRepo: repo}
	got, err := svc.ApplyAntigravityOAuthCredentials(context.Background(), 73, map[string]any{"access_token": "synthetic"})
	require.EqualError(t, err, "write failed")
	require.Nil(t, got)
	require.Equal(t, []string{"credentials"}, repo.calls)
	require.Equal(t, StatusError, repo.account.Status)
}

func TestApplyAntigravityOAuthCredentialsRejectsWrongPlatformOrMissingGrant(t *testing.T) {
	for _, platform := range []string{PlatformAntigravity, PlatformOpenAI} {
		repo := &antigravityReauthRepo{account: &Account{ID: 73, Platform: platform, Type: AccountTypeOAuth}}
		svc := &adminServiceImpl{accountRepo: repo}
		_, err := svc.ApplyAntigravityOAuthCredentials(context.Background(), 73, map[string]any{})
		require.Error(t, err)
		require.Empty(t, repo.calls)
	}
}
