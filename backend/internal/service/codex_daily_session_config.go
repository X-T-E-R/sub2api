package service

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	CodexDailySessionEnabledKey = "codex_daily_session_pool_enabled"
	CodexDailySessionMinKey     = "codex_daily_session_pool_min"
	CodexDailySessionMaxKey     = "codex_daily_session_pool_max"
)

func codexDailySessionInteger(value any) (int, bool) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return int(n), !math.IsNaN(n) && n >= 1 && n <= 1000 && math.Trunc(n) == n
}

func hasCodexDailySessionSettings(extra map[string]any) bool {
	for _, key := range []string{CodexDailySessionEnabledKey, CodexDailySessionMinKey, CodexDailySessionMaxKey} {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// CodexDailySessionPolicy validates the saved opt-in without changing legacy defaults.
func CodexDailySessionPolicy(account *Account) (enabled bool, min, max int, err error) {
	if account == nil {
		return
	}
	extra := account.Extra
	invalid := func(message string) (bool, int, int, error) {
		return false, 0, 0, infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_INVALID", message)
	}
	if value, exists := extra[CodexDailySessionEnabledKey]; exists {
		var ok bool
		enabled, ok = value.(bool)
		if !ok {
			return invalid("codex_daily_session_pool_enabled must be a boolean")
		}
	}
	min, minOK := codexDailySessionInteger(extra[CodexDailySessionMinKey])
	max, maxOK := codexDailySessionInteger(extra[CodexDailySessionMaxKey])
	if _, exists := extra[CodexDailySessionMinKey]; exists && !minOK {
		return invalid("daily session minimum must be an integer between 1 and 1000")
	}
	if _, exists := extra[CodexDailySessionMaxKey]; exists && !maxOK {
		return invalid("daily session maximum must be an integer between 1 and 1000")
	}
	if minOK && maxOK && min > max {
		return invalid("daily session minimum must not exceed maximum")
	}
	if enabled && (!account.IsOpenAIOAuthLike() || account.GetCodexFingerprintMode() != codexFingerprintSession || !minOK || !maxOK) {
		return invalid("daily session pooling requires OpenAI OAuth/setup-token, session mode and an explicit min/max range")
	}
	if enabled {
		accountID := strings.TrimSpace(account.GetChatGPTAccountID())
		userID := strings.TrimSpace(account.GetCredential("chatgpt_user_id"))
		completeMetadata := accountID != "" && userID != ""
		stableSetupToken := account.Type == AccountTypeSetupToken && accountID == "" && userID == "" && strings.TrimSpace(account.GetOpenAIAccessToken()) != ""
		if !completeMetadata && !stableSetupToken {
			return false, 0, 0, infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_IDENTITY_REQUIRED", "daily session pooling requires complete ChatGPT account and user IDs, or a setup-token with stable bearer identity; complete credential metadata before enabling")
		}
	}
	return
}

// CodexDailySessionScope uses the same actual credential namespace as thread/cache isolation.
func CodexDailySessionScope(account *Account) string {
	namespace := codexAccountIdentityNamespace(account)
	if namespace == "" {
		return ""
	}
	return codexPoolDigest("codex-daily-account:v1:" + namespace)
}

func validateCodexDailySessionAccounts(ctx context.Context, repo AccountRepository, candidates ...*Account) error {
	needAliases := false
	replacements := make(map[int64]bool)
	scopes := make(map[string]bool)
	for _, account := range candidates {
		enabled, _, _, err := CodexDailySessionPolicy(account)
		if err != nil {
			return err
		}
		if account.IsShadow() && hasCodexDailySessionSettings(account.Extra) {
			_, hasMin := account.Extra[CodexDailySessionMinKey]
			_, hasMax := account.Extra[CodexDailySessionMaxKey]
			// Mode changes on shadows remain editable. A synchronized false flag
			// outside session mode is inert; intervals still belong to the parent.
			if enabled || hasMin || hasMax {
				return infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_INHERITED", "manage daily session pooling on the credential parent account")
			}
		}
		needAliases = needAliases || enabled
		if enabled {
			scopes[CodexDailySessionScope(account)] = true
		}
		replacements[account.ID] = true
	}
	if !needAliases {
		return nil
	}
	// Inactive/error aliases still own the same pool policy. ListByPlatform is
	// scheduling-oriented (active only); this unfiltered status query matches
	// allocation's nondeleted-account boundary.
	accounts, err := repo.ListAllWithFilters(ctx, PlatformOpenAI, "", "", "", 0, "")
	if err != nil {
		return err
	}
	all := append([]*Account(nil), candidates...)
	for i := range accounts {
		if !replacements[accounts[i].ID] && scopes[CodexDailySessionScope(&accounts[i])] {
			all = append(all, &accounts[i])
		}
	}
	return ValidateCodexDailySessionAliases(all)
}

func validateCodexDailySessionShadowUpdate(previous, candidate *Account, updates map[string]any) error {
	if !candidate.IsShadow() || candidate.GetCodexFingerprintMode() != codexFingerprintSession {
		return nil
	}
	for _, key := range []string{CodexDailySessionEnabledKey, CodexDailySessionMinKey, CodexDailySessionMaxKey} {
		if value, provided := updates[key]; provided {
			prior, existed := previous.Extra[key]
			if !existed || !reflect.DeepEqual(prior, value) {
				return infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_INHERITED", "manage daily session pooling on the credential parent account")
			}
		}
	}
	return nil
}

func ValidateCodexDailySessionAliases(accounts []*Account) error {
	ranges := make(map[string][2]int)
	for _, account := range accounts {
		if account.IsShadow() {
			continue
		}
		enabled, min, max, err := CodexDailySessionPolicy(account)
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		scope := CodexDailySessionScope(account)
		if scope == "" {
			return infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_IDENTITY_REQUIRED", "daily session pooling requires a stable credential account identity")
		}
		interval := [2]int{min, max}
		if prior, exists := ranges[scope]; exists && prior != interval {
			return infraerrors.BadRequest("CODEX_DAILY_SESSION_POOL_ALIAS_CONFLICT", "enabled aliases of the same credential account must have identical daily session min/max; update them together")
		}
		ranges[scope] = interval
	}
	return nil
}

func codexDailySessionUpdatedAccount(account *Account, extra, credentials map[string]any) *Account {
	candidate := *account
	candidate.Extra = maps.Clone(account.Extra)
	if candidate.Extra == nil {
		candidate.Extra = make(map[string]any)
	}
	maps.Copy(candidate.Extra, extra)
	candidate.Credentials = maps.Clone(account.Credentials)
	if candidate.Credentials == nil {
		candidate.Credentials = make(map[string]any)
	}
	maps.Copy(candidate.Credentials, credentials)
	return &candidate
}
