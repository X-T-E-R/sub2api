package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// CodexSessionAccountOwner outlives transient scheduling state and daily slots.
type CodexSessionAccountOwner struct {
	AccountScope string
	AccountID    int64
}

type CodexSessionAffinityRepository interface {
	FindSessionOwner(context.Context, string) (*CodexSessionAccountOwner, error)
	ClaimSessionOwner(context.Context, string, CodexSessionAccountOwner) (*CodexSessionAccountOwner, error)
	FindSessionBindingScopes(context.Context, string) ([]string, error)
}

var ErrCodexSessionAffinity = errors.New("codex session account affinity")

type codexSessionAffinityContextKey struct{}

type codexSessionAffinityState struct {
	binding string
	root    string
	strict  atomic.Bool
	owner   atomic.Pointer[CodexSessionAccountOwner]
}

func codexOriginalSessionID(headers http.Header, body map[string]any) string {
	cm := codexIdentityMetadata(body)
	for _, carrier := range []map[string]any{
		codexMetadataObject(cm[openAIWSTurnMetadataHeader]), cm,
		codexMetadataObject(headers.Get(openAIWSTurnMetadataHeader)),
		{"session_id": headers.Get("session_id"), "session-id": headers.Get("session-id")},
	} {
		for _, key := range []string{"session_id", "session-id"} {
			if value, ok := carrier[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func codexSessionBindingKey(apiKeyID int64, root string) string {
	return codexPoolDigest(fmt.Sprintf("codex-daily-root:v1:%d:%s", apiKeyID, root))
}

// WithCodexSessionAffinity captures only original explicit identity, never a
// content-derived sticky hash or the subsequently projected upstream session.
func WithCodexSessionAffinity(ctx context.Context, c *gin.Context, body []byte) context.Context {
	if ctx == nil || c == nil || c.Request == nil || getAPIKeyIDFromContext(c) <= 0 {
		return ctx
	}
	var metadata map[string]any
	raw := openAIRequestPayloadView(body).Get("client_metadata").Raw
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &metadata)
	}
	root := codexOriginalSessionID(c.Request.Header, map[string]any{"client_metadata": metadata})
	if root == "" {
		return ctx
	}
	return context.WithValue(ctx, codexSessionAffinityContextKey{}, &codexSessionAffinityState{
		root: root, binding: codexSessionBindingKey(getAPIKeyIDFromContext(c), root),
	})
}

func CodexSessionAffinityActive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	return state != nil && state.strict.Load()
}

func (s *CodexDailySessionPool) findSessionOwner(ctx context.Context, repo CodexSessionAffinityRepository, binding string) (*CodexSessionAccountOwner, error) {
	if s.cache != nil {
		if value, ok := s.cache.Get("owner:" + binding); ok {
			if owner, ok := value.(CodexSessionAccountOwner); ok {
				return &owner, nil
			}
		}
	}
	owner, err := repo.FindSessionOwner(ctx, binding)
	if err == nil && owner != nil {
		s.cacheSessionOwner(binding, *owner)
	}
	return owner, err
}

func (s *CodexDailySessionPool) cacheSessionOwner(binding string, owner CodexSessionAccountOwner) {
	if s.cache != nil {
		s.cache.SetWithTTL("owner:"+binding, owner, 1, 15*time.Minute)
	}
}

func (s *OpenAIGatewayService) codexSessionAccountScope(ctx context.Context, account *Account, enroll bool) string {
	if account == nil || !account.IsOpenAIOAuthLike() || account.GetCodexFingerprintMode() != codexFingerprintSession {
		return ""
	}
	source, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil || source == nil {
		return ""
	}
	if enroll {
		enabled, _, _, err := CodexDailySessionPolicy(source)
		if err != nil || !enabled {
			return ""
		}
	}
	return CodexDailySessionScope(source)
}

func codexAffinityError(message string) error {
	return fmt.Errorf("%w: %s", ErrCodexSessionAffinity, message)
}

func (s *OpenAIGatewayService) selectCodexSessionAccount(ctx context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, bool, error) {
	decision := OpenAIAccountScheduleDecision{Layer: "codex_session_owner"}
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	if state == nil || NormalizeOpenAICompatiblePlatform(req.Platform) != PlatformOpenAI || req.RequiredImageCapability != "" {
		return nil, decision, false, nil
	}
	enroll := s.settingService != nil && s.settingService.IsCodexSessionAffinityEnabled(req.GroupID)
	pool := s.codexDailySessionPool
	if pool == nil {
		if enroll {
			return nil, decision, true, codexAffinityError("session owner storage is unavailable")
		}
		return nil, decision, false, nil
	}
	repo, ok := pool.repo.(CodexSessionAffinityRepository)
	if !ok {
		if enroll {
			return nil, decision, true, codexAffinityError("session owner storage is unavailable")
		}
		return nil, decision, false, nil
	}
	owner, err := pool.findSessionOwner(ctx, repo, state.binding)
	if err != nil {
		return nil, decision, true, fmt.Errorf("%w: read session owner: %w", ErrCodexSessionAffinity, err)
	}
	if owner == nil && !enroll {
		return nil, decision, false, nil
	}
	if s.checkChannelPricingRestriction(ctx, req.GroupID, req.RequestedModel) {
		return nil, decision, true, codexAffinityError("channel pricing restriction")
	}
	state.strict.Store(true)

	// Previous-response mappings are evidence even when that account is now
	// unavailable; eligibility must not erase an identity conflict.
	previousID := int64(0)
	previousScope := ""
	if req.PreviousResponseID != "" {
		store := s.getOpenAIWSStateStore()
		if strictStore, ok := store.(*defaultOpenAIWSStateStore); ok {
			previousID, err = strictStore.getResponseAccount(ctx, derefGroupID(req.GroupID), req.PreviousResponseID, true)
		} else {
			previousID, err = store.GetResponseAccount(ctx, derefGroupID(req.GroupID), req.PreviousResponseID)
		}
		if err != nil {
			return nil, decision, true, fmt.Errorf("%w: read response owner: %w", ErrCodexSessionAffinity, err)
		}
		if previousID > 0 {
			account, getErr := s.accountRepo.GetByID(ctx, previousID)
			if getErr != nil {
				return nil, decision, true, fmt.Errorf("%w: read response account: %w", ErrCodexSessionAffinity, getErr)
			}
			previousScope = s.codexSessionAccountScope(ctx, account, false)
			if previousScope == "" {
				return nil, decision, true, codexAffinityError("previous response credential is unavailable")
			}
		}
	}
	recovered := owner == nil
	if owner == nil {
		owner, err = s.recoverCodexSessionOwner(ctx, repo, state, req.GroupID, previousID, previousScope)
		if err != nil {
			return nil, decision, true, err
		}
	}
	if owner != nil && previousScope != "" && previousScope != owner.AccountScope {
		return nil, decision, true, codexAffinityError("previous response belongs to a different credential")
	}
	if owner == nil && req.PreviousResponseID != "" {
		return nil, decision, true, codexAffinityError("continuation credential is unknown; no new account was selected")
	}
	if owner == nil {
		// A new root uses the existing load balancer, restricted before scoring
		// and again after fresh-account validation. No sticky/previous escape.
		newCtx := context.WithValue(ctx, codexSessionEnrollmentContextKey{}, true)
		scheduler := s.getOpenAIAccountScheduler(ctx)
		if scheduler == nil {
			scheduler = &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
		}
		newReq := req
		newReq.PreviousResponseID = ""
		newReq.SessionHash = ""
		newReq.PreserveStickyBinding = true
		selection, selectedDecision, selectErr := scheduler.Select(newCtx, newReq)
		if selectErr != nil || selection == nil || selection.Account == nil {
			return selection, selectedDecision, true, selectErr
		}
		scope := s.codexSessionAccountScope(ctx, selection.Account, true)
		if scope == "" {
			releaseCodexSessionSelection(selection)
			return nil, decision, true, codexAffinityError("selected credential no longer has daily session pooling enabled")
		}
		proposed := CodexSessionAccountOwner{AccountScope: scope, AccountID: selection.Account.ID}
		owner, err = repo.ClaimSessionOwner(ctx, state.binding, proposed)
		if err != nil || owner == nil {
			releaseCodexSessionSelection(selection)
			if err != nil {
				return nil, decision, true, fmt.Errorf("%w: claim session owner: %w", ErrCodexSessionAffinity, err)
			}
			return nil, decision, true, codexAffinityError("claim returned no session owner")
		}
		pool.cacheSessionOwner(state.binding, *owner)
		state.owner.Store(owner)
		if *owner == proposed {
			selectedDecision.Layer = "codex_session_new"
			return selection, selectedDecision, true, nil
		}
		// Only the DB winner may forward, including concurrent first requests
		// handled by different gateway instances.
		releaseCodexSessionSelection(selection)
	} else if recovered {
		// Recovered history must also participate in the first-claim race.
		claimed, claimErr := repo.ClaimSessionOwner(ctx, state.binding, *owner)
		if claimErr != nil || claimed == nil {
			if claimErr != nil {
				return nil, decision, true, fmt.Errorf("%w: restore session owner: %w", ErrCodexSessionAffinity, claimErr)
			}
			return nil, decision, true, codexAffinityError("restore returned no session owner")
		}
		if claimed.AccountScope != owner.AccountScope {
			return nil, decision, true, codexAffinityError("session history conflicts with the established credential")
		}
		owner = claimed
		pool.cacheSessionOwner(state.binding, *owner)
	}
	if previousScope != "" && previousScope != owner.AccountScope {
		return nil, decision, true, codexAffinityError("previous response belongs to a different credential")
	}
	state.owner.Store(owner)
	accountID := owner.AccountID
	if previousID > 0 {
		accountID = previousID
	}
	// Use existing sticky admission and bounded waiting, but never escape or
	// delete the durable binding when the account is temporarily unavailable.
	strictReq := req
	strictReq.SessionHash = state.binding
	strictReq.StickyAccountID = accountID
	strictReq.PreserveStickyBinding = true
	strictReq.StrictSessionAffinity = true
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	selection, _, err := scheduler.selectBySessionHash(ctx, strictReq)
	if err != nil {
		return nil, decision, true, fmt.Errorf("%w: bound account admission: %w", ErrCodexSessionAffinity, err)
	}
	if selection == nil || selection.Account == nil {
		return nil, decision, true, codexAffinityError("bound account is temporarily unavailable; retry this session later")
	}
	if s.codexSessionAccountScope(ctx, selection.Account, false) != owner.AccountScope {
		releaseCodexSessionSelection(selection)
		return nil, decision, true, codexAffinityError("bound credential changed during selection")
	}
	decision.SelectedAccountID = selection.Account.ID
	decision.SelectedAccountType = selection.Account.Type
	decision.StickySessionHit = true
	decision.StickyPreviousHit = previousID > 0 && selection.Account.ID == previousID
	return selection, decision, true, nil
}

func validateCodexSessionProjection(ctx context.Context, source *Account, root string) error {
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	if state == nil || !state.strict.Load() {
		return nil
	}
	owner := state.owner.Load()
	if root != state.root || owner == nil || CodexDailySessionScope(source) != owner.AccountScope {
		return codexAffinityError("request identity changed after account selection; start a separate connection for another root session")
	}
	return nil
}

func validateCodexSessionCredential(ctx context.Context, account *Account) error {
	if !CodexSessionAffinityActive(ctx) {
		return nil
	}
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	return validateCodexSessionProjection(ctx, account, state.root)
}

type codexSessionEnrollmentContextKey struct{}

func releaseCodexSessionSelection(selection *AccountSelectionResult) {
	if selection != nil && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func (s *OpenAIGatewayService) recoverCodexSessionOwner(ctx context.Context, repo CodexSessionAffinityRepository, state *codexSessionAffinityState, groupID *int64, previousID int64, previousScope string) (*CodexSessionAccountOwner, error) {
	scopes, err := repo.FindSessionBindingScopes(ctx, state.binding)
	if err != nil {
		return nil, fmt.Errorf("%w: read session history: %w", ErrCodexSessionAffinity, err)
	}
	if previousScope != "" {
		if len(scopes) > 0 && !containsCodexSessionScope(scopes, previousScope) {
			return nil, codexAffinityError("previous response conflicts with session history")
		}
		return &CodexSessionAccountOwner{AccountScope: previousScope, AccountID: previousID}, nil
	}
	current, legacy := deriveOpenAISessionHashes(state.root)
	if s.cache != nil {
		id, lookupErr := s.getStickySessionAccountID(withOpenAILegacySessionHash(ctx, legacy), groupID, current)
		if lookupErr != nil && !errors.Is(lookupErr, ErrStickySessionNotFound) {
			return nil, fmt.Errorf("%w: read original session binding: %w", ErrCodexSessionAffinity, lookupErr)
		}
		if id > 0 {
			account, getErr := s.accountRepo.GetByID(ctx, id)
			if getErr != nil {
				return nil, fmt.Errorf("%w: read bound account: %w", ErrCodexSessionAffinity, getErr)
			}
			scope := s.codexSessionAccountScope(ctx, account, false)
			if scope != "" && (len(scopes) == 0 || containsCodexSessionScope(scopes, scope)) {
				return &CodexSessionAccountOwner{AccountScope: scope, AccountID: id}, nil
			}
			return nil, codexAffinityError("original session binding conflicts with daily session history or configuration")
		}
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	if len(scopes) != 1 {
		return nil, codexAffinityError("session has multiple historical credentials and no unambiguous continuation binding")
	}
	// Only cold legacy recovery scans accounts. Warm owners are point lookups.
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformOpenAI, "", "", "", 0, "")
	if err != nil {
		return nil, fmt.Errorf("%w: recover credential account: %w", ErrCodexSessionAffinity, err)
	}
	for i := range accounts {
		if !accounts[i].IsShadow() && s.openAIAccountMatchesSchedulingGroup(&accounts[i], groupID) &&
			s.codexSessionAccountScope(ctx, &accounts[i], false) == scopes[0] {
			return &CodexSessionAccountOwner{AccountScope: scopes[0], AccountID: accounts[i].ID}, nil
		}
	}
	return nil, codexAffinityError("historical session credential is unavailable")
}

func containsCodexSessionScope(scopes []string, scope string) bool {
	for _, candidate := range scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
