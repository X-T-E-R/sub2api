package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// CodexSessionAccountOwner outlives transient scheduling state and daily slots.
type CodexSessionAccountOwner struct {
	AccountScope string
	AccountID    int64
	Revision     int64
}

type CodexSessionAffinityRepository interface {
	FindSessionOwner(context.Context, string) (*CodexSessionAccountOwner, error)
	ClaimSessionOwner(context.Context, string, CodexSessionAccountOwner) (*CodexSessionAccountOwner, error)
	MoveSessionOwner(context.Context, string, CodexSessionAccountOwner, CodexSessionAccountOwner) (*CodexSessionAccountOwner, error)
	FindSessionBindingScopes(context.Context, string) ([]string, error)
}

var ErrCodexSessionAffinity = errors.New("codex session account affinity")

type codexSessionAffinityContextKey struct{}

type codexSessionAffinityState struct {
	binding       string
	root          string
	strict        atomic.Bool
	owner         atomic.Pointer[CodexSessionAccountOwner]
	openAI        atomic.Bool
	resetPrevious atomic.Bool
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

func codexSessionReplayProtected(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	return state != nil && (state.openAI.Load() || state.strict.Load())
}

// CodexSessionMayRetry requires a definite rejection, not merely the absence
// of downstream text. An EOF or a first-output timeout can still be billable.
func CodexSessionMayRetry(ctx context.Context, err *UpstreamFailoverError) bool {
	if !codexSessionReplayProtected(ctx) {
		return true
	}
	if err == nil || err.TransportError != "" || err.SafeToFailoverAfterWrite {
		return false
	}
	if err.IsCredentialFailure() {
		return err.Scope == GatewayFailureScopeAccount && err.ShouldRetryNextAccount()
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func CodexSessionMayFailover(ctx context.Context, err *UpstreamFailoverError) bool {
	if !codexSessionReplayProtected(ctx) {
		return true
	}
	return CodexSessionMayRetry(ctx, err) && !err.RequestScopedTransient && err.ShouldRetryNextAccount()
}

func codexSessionCredentialUnavailable(ctx context.Context, message string) error {
	if !codexSessionReplayProtected(ctx) {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, &UpstreamFailoverError{
		StatusCode: http.StatusBadGateway,
		Stage:      GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount,
		Reason: "openai_credential_unavailable", NextAccountAction: NextAccountRetry,
		ClientStatusCode: http.StatusBadGateway, ClientMessage: "Upstream account credentials are unavailable",
	})
}

func (s *OpenAIGatewayService) codexSessionWSDialFailover(ctx context.Context, account *Account, err error) *UpstreamFailoverError {
	if !codexSessionReplayProtected(ctx) {
		return nil
	}
	var dialErr *openAIWSDialError
	if !errors.As(err, &dialErr) || dialErr == nil {
		return nil
	}
	switch dialErr.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		message := extractUpstreamErrorMessage(dialErr.ResponseBody)
		if s.shouldFailoverOpenAIUpstreamResponse(dialErr.StatusCode, message, dialErr.ResponseBody) {
			return s.newOpenAIAccountFailoverError(account, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, message, false, false)
		}
	}
	return nil
}

func (s *OpenAIGatewayService) codexSessionAccountScope(ctx context.Context, account *Account, enroll bool) string {
	scope, _ := s.codexSessionAccountScopeWithError(ctx, account, enroll)
	return scope
}

func (s *OpenAIGatewayService) codexSessionAccountScopeWithError(ctx context.Context, account *Account, enroll bool) (string, error) {
	if account == nil || !account.IsOpenAIOAuthLike() || account.GetCodexFingerprintMode() != codexFingerprintSession {
		return "", nil
	}
	source, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil || source == nil {
		return "", err
	}
	if enroll {
		enabled, _, _, err := CodexDailySessionPolicy(source)
		if err != nil || !enabled {
			return "", err
		}
	}
	return CodexDailySessionScope(source), nil
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
	state.openAI.Store(true)
	state.resetPrevious.Store(false)
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
	// Mutable ownership must be read across replicas; the immutable daily slot
	// cache remains independent and unchanged.
	owner, err := repo.FindSessionOwner(ctx, state.binding)
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
	if owner == nil && req.PreviousResponseID != "" {
		return nil, decision, true, codexAffinityError("continuation credential is unknown; no new account was selected")
	}
	if owner != nil && recovered {
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
	}
	if owner != nil {
		selection, ownerDecision, selectErr := s.selectCodexSessionOwner(ctx, state, req, owner, previousID, previousScope)
		if selectErr != nil || selection != nil {
			return selection, ownerDecision, true, selectErr
		}
		if req.PreviousResponseID != "" && !req.PreviousResponseCanMove {
			return nil, decision, true, codexAffinityError("continuation credential is unavailable; full replayable input is required to change accounts")
		}
	}

	// Only a failed admission (quota, disabled or incompatible account) reaches
	// balancing. Concurrency returns a bounded wait above; metrics never escape.
	newCtx := context.WithValue(ctx, codexSessionEnrollmentContextKey{}, true)
	scheduler := s.getOpenAIAccountScheduler(ctx)
	if scheduler == nil {
		scheduler = &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	}
	newReq := req
	newReq.PreviousResponseID, newReq.SessionHash = "", ""
	newReq.StickyAccountID = 0
	newReq.PreserveStickyBinding = true
	if owner != nil {
		newReq.ExcludedIDs = maps.Clone(req.ExcludedIDs)
		if newReq.ExcludedIDs == nil {
			newReq.ExcludedIDs = make(map[int64]struct{})
		}
		newReq.ExcludedIDs[owner.AccountID] = struct{}{}
	}
	selection, selectedDecision, err := scheduler.Select(newCtx, newReq)
	if err != nil {
		return nil, decision, true, fmt.Errorf("%w: select replacement: %w", ErrCodexSessionAffinity, err)
	}
	if selection == nil || selection.Account == nil {
		return nil, decision, true, codexAffinityError("no eligible session account")
	}
	scope := s.codexSessionAccountScope(ctx, selection.Account, true)
	if scope == "" {
		releaseCodexSessionSelection(selection)
		return nil, decision, true, codexAffinityError("selected credential no longer has daily session pooling enabled")
	}
	proposed := CodexSessionAccountOwner{AccountScope: scope, AccountID: selection.Account.ID}
	var claimed *CodexSessionAccountOwner
	if owner == nil {
		claimed, err = repo.ClaimSessionOwner(ctx, state.binding, proposed)
		selectedDecision.Layer = "codex_session_new"
	} else {
		claimed, err = repo.MoveSessionOwner(ctx, state.binding, *owner, proposed)
		selectedDecision.Layer = "codex_session_failover"
	}
	if err != nil || claimed == nil {
		releaseCodexSessionSelection(selection)
		if err != nil {
			return nil, decision, true, fmt.Errorf("%w: update session owner: %w", ErrCodexSessionAffinity, err)
		}
		return nil, decision, true, codexAffinityError("update returned no session owner")
	}
	if claimed.AccountID == proposed.AccountID && claimed.AccountScope == proposed.AccountScope {
		state.owner.Store(claimed)
		state.resetPrevious.Store(req.PreviousResponseID != "" && owner != nil)
		return selection, selectedDecision, true, nil
	}
	// A concurrent request already moved ownership. Release this candidate and
	// admit the actual winner once; a stale request must never move it back.
	releaseCodexSessionSelection(selection)
	if req.PreviousResponseID != "" && owner != nil &&
		(owner.AccountID != claimed.AccountID || owner.AccountScope != claimed.AccountScope) {
		state.resetPrevious.Store(true)
	}
	selection, decision, err = s.selectCodexSessionOwner(ctx, state, req, claimed, previousID, previousScope)
	if err == nil && selection == nil {
		err = codexAffinityError("current session account is unavailable; retry this session later")
	}
	return selection, decision, true, err
}

func (s *OpenAIGatewayService) selectCodexSessionOwner(ctx context.Context, state *codexSessionAffinityState, req OpenAIAccountScheduleRequest, owner *CodexSessionAccountOwner, previousID int64, previousScope string) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: "codex_session_owner"}
	if previousScope != "" && previousScope != owner.AccountScope {
		if !req.PreviousResponseCanMove {
			return nil, decision, codexAffinityError("previous response belongs to a different credential")
		}
		state.resetPrevious.Store(true)
	}
	state.owner.Store(owner)
	accountID := owner.AccountID
	if previousID > 0 && previousScope == owner.AccountScope {
		accountID = previousID
	}
	if accountID <= 0 {
		return nil, decision, nil
	}
	strictReq := req
	strictReq.SessionHash = state.binding
	strictReq.StickyAccountID = accountID
	strictReq.PreserveStickyBinding = true
	strictReq.StrictSessionAffinity = true
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	selection, _, err := scheduler.selectBySessionHash(ctx, strictReq)
	if err != nil {
		return nil, decision, fmt.Errorf("%w: bound account admission: %w", ErrCodexSessionAffinity, err)
	}
	if selection == nil || selection.Account == nil {
		return nil, decision, nil
	}
	scope, err := s.codexSessionAccountScopeWithError(ctx, selection.Account, false)
	if err != nil {
		releaseCodexSessionSelection(selection)
		return nil, decision, fmt.Errorf("%w: read credential scope: %w", ErrCodexSessionAffinity, err)
	}
	if scope != owner.AccountScope {
		releaseCodexSessionSelection(selection)
		return nil, decision, nil
	}
	decision.SelectedAccountID = selection.Account.ID
	decision.SelectedAccountType = selection.Account.Type
	decision.StickySessionHit = true
	decision.StickyPreviousHit = previousID > 0 && selection.Account.ID == previousID
	return selection, decision, nil
}

// CodexSessionMayResetPreviousResponse distinguishes known ownership changes
// from an expired response cache entry on an otherwise unchanged account.
func CodexSessionMayResetPreviousResponse(ctx context.Context) bool {
	state, _ := ctx.Value(codexSessionAffinityContextKey{}).(*codexSessionAffinityState)
	return state != nil && state.resetPrevious.Load()
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
