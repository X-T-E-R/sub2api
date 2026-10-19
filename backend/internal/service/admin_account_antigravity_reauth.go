package service

import (
	"context"
	"errors"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ApplyAntigravityOAuthCredentials applies a new OAuth grant without treating
// authentication recovery as an instruction to clear provider quota/cooldowns.
func (s *adminServiceImpl) ApplyAntigravityOAuthCredentials(ctx context.Context, id int64, credentials map[string]any) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if account.Platform != PlatformAntigravity || !account.IsOAuth() {
		return nil, infraerrors.BadRequest("NOT_ANTIGRAVITY_OAUTH", "account must be Antigravity OAuth")
	}
	access, _ := credentials["access_token"].(string)
	if strings.TrimSpace(access) == "" {
		return nil, infraerrors.BadRequest("MISSING_ACCESS_TOKEN", "re-authorization requires an access token")
	}
	// Unlike the full account editor, re-auth supplies only token metadata; retain
	// model_mapping, endpoint overrides and every other existing credential option.
	merged := shallowCopyMap(account.Credentials)
	if merged == nil {
		merged = make(map[string]any)
	}
	for key, value := range credentials {
		merged[key] = value
	}
	merged = SanitizeStoredCredentials(account.Platform, merged)
	updater, ok := s.accountRepo.(accountCredentialsUpdater)
	if !ok {
		return nil, errors.New("account credential updater is not configured")
	}
	// Production UpdateCredentials writes credentials only for AG, preserving
	// concurrent extra/runtime changes; never fall back to a full account update.
	if err := updater.UpdateCredentials(ctx, id, merged); err != nil {
		return nil, err
	}
	if account.Status == StatusError {
		if err := s.accountRepo.ClearError(ctx, id); err != nil {
			return nil, err
		}
		// SetError disables scheduling. Restore that error state, but leave an active
		// account's manual scheduling pause (and disabled accounts) untouched.
		if err := s.accountRepo.SetSchedulable(ctx, id, true); err != nil {
			return nil, err
		}
	}
	return s.accountRepo.GetByID(ctx, id)
}
