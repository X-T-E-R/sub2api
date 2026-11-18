package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	WarmupBucketModeAutoBuckets  = "auto_buckets"
	WarmupBucketModeGeminiOnly   = "gemini_only"
	WarmupBucketModeClaudeOnly   = "claude_only"
	WarmupBucketModeCustomModels = "custom_models"

	WarmupDefaultConcurrency = 3
	WarmupMaxConcurrency     = 6
	WarmupMaxBatchAccounts   = 100

	warmupInterBucketMinJitter = 1200 * time.Millisecond
	warmupInterBucketMaxJitter = 2200 * time.Millisecond
	warmupWorkerStartMinJitter = 200 * time.Millisecond
	warmupWorkerStartMaxJitter = 900 * time.Millisecond
	warmupPostRefreshDelay     = 1200 * time.Millisecond

	warmupThinkingBudgetTokens = 1024
	warmupThinkingMaxTokens    = 4096
	warmupStandardMaxTokens    = 512
)

var ErrAccountWarmupUnavailable = infraerrors.ServiceUnavailable(
	"ACCOUNT_WARMUP_UNAVAILABLE",
	"account warmup service is not available",
)

var warmupGeminiPriority = []string{
	"gemini-4-carbon",
	"gemini-4-hydrogen",
	"gemini-4-argon",
	"gemini-3.8-flash",
	"gemini-3-flash",
	"gemini-2.5-flash",
}

var warmupClaudePriority = []string{
	"claude-opus-5-5",
	"claude-sonnet-5-5",
	"claude-opus-4-6-thinking",
	"claude-sonnet-4-6",
}

// AccountWarmupUsageRefresher decouples post-warmup usage refresh for clean testing while
// remaining satisfied directly by *AccountUsageService in production Wire bindings.
type AccountWarmupUsageRefresher interface {
	GetUsage(ctx context.Context, accountID int64, force ...bool) (*UsageInfo, error)
}

// WarmupAccountsRequest defines the JSON request contract for single and batch account warmup.
type WarmupAccountsRequest struct {
	AccountIDs   []int64  `json:"account_ids"`
	BucketMode   string   `json:"bucket_mode"`
	Models       []string `json:"models"`
	Effort       string   `json:"effort"`
	Concurrency  int      `json:"concurrency"`
	RefreshUsage *bool    `json:"refresh_usage"`
}

// WarmupBucketRunResult reports the outcome of warming a single quota bucket/model on an account.
type WarmupBucketRunResult struct {
	Bucket          string `json:"bucket"`
	RequestedModel  string `json:"requested_model"`
	ResolvedModel   string `json:"resolved_model"`
	PromptSummary   string `json:"prompt_summary"`
	ExpectedAnswer  string `json:"expected_answer"`
	ResponseExcerpt string `json:"response_excerpt"`
	AnswerMatched   bool   `json:"answer_matched"`
	StatusCode      int    `json:"status_code"`
	DurationMs      int64  `json:"duration_ms"`
	Error           string `json:"error"`
}

// WarmupAccountResult reports the aggregated warmup outcome and refreshed quota windows for one account.
type WarmupAccountResult struct {
	AccountID   int64                   `json:"account_id"`
	AccountName string                  `json:"account_name"`
	Platform    string                  `json:"platform"`
	Schedulable bool                    `json:"schedulable"`
	Success     bool                    `json:"success"`
	Error       string                  `json:"error"`
	Runs        []WarmupBucketRunResult `json:"runs"`
	Usage       *UsageInfo              `json:"usage,omitempty"`
	UsageError  string                  `json:"usage_error"`
}

// WarmupAccountsResponse defines the JSON response contract for single and batch account warmup.
type WarmupAccountsResponse struct {
	TotalAccounts     int                   `json:"total_accounts"`
	SucceededAccounts int                   `json:"succeeded_accounts"`
	FailedAccounts    int                   `json:"failed_accounts"`
	BucketMode        string                `json:"bucket_mode"`
	Results           []WarmupAccountResult `json:"results"`
}

type warmupPlannedRun struct {
	bucket         string
	requestedModel string
}

// AccountWarmupOption customizes pacing or collaborators in tests without affecting production wiring.
type AccountWarmupOption func(*AccountWarmupService)

// WithWarmupSleep overrides time-based pacing delays (jitter and post-warmup refresh wait).
func WithWarmupSleep(fn func(ctx context.Context, d time.Duration) error) AccountWarmupOption {
	return func(s *AccountWarmupService) {
		if fn != nil {
			s.sleepFn = fn
		}
	}
}

// WithWarmupRandDuration overrides randomized jitter sampling.
func WithWarmupRandDuration(fn func(min, max time.Duration) time.Duration) AccountWarmupOption {
	return func(s *AccountWarmupService) {
		if fn != nil {
			s.randDurationFn = fn
		}
	}
}

// WithWarmupUsageRefresher overrides the usage refresher implementation.
func WithWarmupUsageRefresher(refresher AccountWarmupUsageRefresher) AccountWarmupOption {
	return func(s *AccountWarmupService) {
		s.usageRefresher = refresher
	}
}

// AccountWarmupService executes real Antigravity generation requests across quota buckets
// to start 5-hour and 7-day rolling reset timers without touching AccountTestService.
type AccountWarmupService struct {
	accountRepo               AccountRepository
	antigravityGatewayService *AntigravityGatewayService
	usageRefresher            AccountWarmupUsageRefresher
	sleepFn                   func(ctx context.Context, d time.Duration) error
	randDurationFn            func(min, max time.Duration) time.Duration
}

// NewAccountWarmupService creates a standalone AccountWarmupService.
func NewAccountWarmupService(
	accountRepo AccountRepository,
	antigravityGatewayService *AntigravityGatewayService,
	accountUsageService *AccountUsageService,
	opts ...AccountWarmupOption,
) *AccountWarmupService {
	var refresher AccountWarmupUsageRefresher
	if accountUsageService != nil {
		refresher = accountUsageService
	}
	svc := &AccountWarmupService{
		accountRepo:               accountRepo,
		antigravityGatewayService: antigravityGatewayService,
		usageRefresher:            refresher,
		sleepFn:                   defaultWarmupSleep,
		randDurationFn:            defaultWarmupRandDuration,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(svc)
		}
	}
	return svc
}

// ProvideAccountWarmupService wires AccountWarmupService for Google Wire without variadic ambiguity.
func ProvideAccountWarmupService(
	accountRepo AccountRepository,
	antigravityGatewayService *AntigravityGatewayService,
	accountUsageService *AccountUsageService,
) *AccountWarmupService {
	return NewAccountWarmupService(accountRepo, antigravityGatewayService, accountUsageService)
}

func defaultWarmupSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultWarmupRandDuration(minDur, maxDur time.Duration) time.Duration {
	if maxDur <= minDur {
		return minDur
	}
	spanMs := int((maxDur - minDur) / time.Millisecond)
	if spanMs <= 0 {
		return minDur
	}
	offsetMs, err := cryptoRandIntRange(0, spanMs)
	if err != nil {
		return minDur
	}
	return minDur + time.Duration(offsetMs)*time.Millisecond
}

type normalizedWarmupOptions struct {
	accountIDs   []int64
	bucketMode   string
	customModels []string
	effort       string
	concurrency  int
	refreshUsage bool
}

func normalizeWarmupRequest(req *WarmupAccountsRequest) (*normalizedWarmupOptions, error) {
	if req == nil {
		return nil, infraerrors.BadRequest("INVALID_WARMUP_REQUEST", "request is required")
	}

	seenIDs := make(map[int64]struct{}, len(req.AccountIDs))
	accountIDs := make([]int64, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		if id <= 0 {
			return nil, infraerrors.BadRequest("INVALID_WARMUP_REQUEST", "account_ids must contain positive integers")
		}
		if _, exists := seenIDs[id]; exists {
			continue
		}
		seenIDs[id] = struct{}{}
		accountIDs = append(accountIDs, id)
	}
	if len(accountIDs) == 0 {
		return nil, infraerrors.BadRequest("INVALID_WARMUP_REQUEST", "account_ids must not be empty")
	}
	if len(accountIDs) > WarmupMaxBatchAccounts {
		return nil, infraerrors.BadRequest(
			"INVALID_WARMUP_REQUEST",
			fmt.Sprintf("account_ids cannot exceed %d accounts per batch", WarmupMaxBatchAccounts),
		)
	}

	bucketMode := strings.ToLower(strings.TrimSpace(req.BucketMode))
	if bucketMode == "" {
		bucketMode = WarmupBucketModeAutoBuckets
	}
	switch bucketMode {
	case WarmupBucketModeAutoBuckets, WarmupBucketModeGeminiOnly, WarmupBucketModeClaudeOnly, WarmupBucketModeCustomModels:
	default:
		return nil, infraerrors.BadRequest(
			"INVALID_WARMUP_REQUEST",
			fmt.Sprintf("unsupported bucket_mode %q", req.BucketMode),
		)
	}

	var customModels []string
	if bucketMode == WarmupBucketModeCustomModels {
		seenModels := make(map[string]struct{}, len(req.Models))
		for _, raw := range req.Models {
			m := strings.TrimPrefix(strings.TrimSpace(raw), "models/")
			if m == "" {
				continue
			}
			if _, exists := seenModels[m]; exists {
				continue
			}
			seenModels[m] = struct{}{}
			customModels = append(customModels, m)
		}
		if len(customModels) == 0 {
			return nil, infraerrors.BadRequest("INVALID_WARMUP_REQUEST", "models must not be empty when bucket_mode is custom_models")
		}
	}

	effort := strings.ToLower(strings.TrimSpace(req.Effort))
	if effort != "" && !antigravityEffortName.MatchString(effort) {
		return nil, infraerrors.BadRequest("INVALID_WARMUP_REQUEST", fmt.Sprintf("invalid effort %q", req.Effort))
	}

	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = WarmupDefaultConcurrency
	} else if concurrency > WarmupMaxConcurrency {
		concurrency = WarmupMaxConcurrency
	}
	if concurrency > len(accountIDs) {
		concurrency = len(accountIDs)
	}

	refreshUsage := true
	if req.RefreshUsage != nil {
		refreshUsage = *req.RefreshUsage
	}

	return &normalizedWarmupOptions{
		accountIDs:   accountIDs,
		bucketMode:   bucketMode,
		customModels: customModels,
		effort:       effort,
		concurrency:  concurrency,
		refreshUsage: refreshUsage,
	}, nil
}

// WarmupAccounts runs single or batch Antigravity quota warmup with bounded concurrency,
// per-account serial bucket pacing, unique mental-math prompts, and forced usage refresh.
func (s *AccountWarmupService) WarmupAccounts(ctx context.Context, req *WarmupAccountsRequest) (*WarmupAccountsResponse, error) {
	if s == nil || s.accountRepo == nil || s.antigravityGatewayService == nil {
		return nil, ErrAccountWarmupUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opts, err := normalizeWarmupRequest(req)
	if err != nil {
		return nil, err
	}

	results := make([]WarmupAccountResult, len(opts.accountIDs))
	seenPrompts := &sync.Map{}

	if len(opts.accountIDs) == 1 {
		results[0] = s.warmupSingleAccount(ctx, opts.accountIDs[0], opts, seenPrompts)
	} else {
		type workItem struct {
			index     int
			accountID int64
		}
		workCh := make(chan workItem, len(opts.accountIDs))
		for idx, id := range opts.accountIDs {
			workCh <- workItem{index: idx, accountID: id}
		}
		close(workCh)

		var wg sync.WaitGroup
		for workerIdx := 0; workerIdx < opts.concurrency; workerIdx++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Per-worker startup jitter across multi-account batches.
				startupJitter := s.randDurationFn(warmupWorkerStartMinJitter, warmupWorkerStartMaxJitter)
				if err := s.sleepFn(ctx, startupJitter); err != nil {
					for item := range workCh {
						results[item.index] = WarmupAccountResult{
							AccountID: item.accountID,
							Success:   false,
							Error:     fmt.Sprintf("warmup canceled: %v", err),
							Runs:      []WarmupBucketRunResult{},
						}
					}
					return
				}
				for item := range workCh {
					results[item.index] = s.warmupSingleAccount(ctx, item.accountID, opts, seenPrompts)
				}
			}()
		}
		wg.Wait()
	}

	succeeded := 0
	failed := 0
	for i := range results {
		if results[i].Runs == nil {
			results[i].Runs = []WarmupBucketRunResult{}
		}
		if results[i].Success {
			succeeded++
		} else {
			failed++
		}
	}

	return &WarmupAccountsResponse{
		TotalAccounts:     len(results),
		SucceededAccounts: succeeded,
		FailedAccounts:    failed,
		BucketMode:        opts.bucketMode,
		Results:           results,
	}, nil
}

func (s *AccountWarmupService) warmupSingleAccount(
	ctx context.Context,
	accountID int64,
	opts *normalizedWarmupOptions,
	seenPrompts *sync.Map,
) WarmupAccountResult {
	res := WarmupAccountResult{
		AccountID: accountID,
		Runs:      []WarmupBucketRunResult{},
	}
	if err := ctx.Err(); err != nil {
		res.Error = fmt.Sprintf("warmup canceled: %v", err)
		return res
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		res.Error = fmt.Sprintf("load account %d failed: %v", accountID, err)
		return res
	}
	if account == nil {
		res.Error = fmt.Sprintf("account %d not found", accountID)
		return res
	}

	res.AccountID = account.ID
	res.AccountName = account.Name
	res.Platform = account.Platform
	res.Schedulable = account.Schedulable

	if account.Platform != PlatformAntigravity {
		res.Error = fmt.Sprintf("unsupported account platform %q: warmup requires antigravity", account.Platform)
		return res
	}
	if account.Type == AccountTypeAPIKey {
		res.Error = "unsupported antigravity account type \"apikey\": warmup requires oauth or upstream account"
		return res
	}
	if account.IsShadow() {
		res.Error = "shadow accounts do not hold independent credentials; warm up the parent account"
		return res
	}

	plannedRuns, err := resolveWarmupPlannedRuns(ctx, account, opts.bucketMode, opts.customModels, opts.effort)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	anySucceeded := false
	var runErrors []string

	for idx, planned := range plannedRuns {
		if idx > 0 {
			jitter := s.randDurationFn(warmupInterBucketMinJitter, warmupInterBucketMaxJitter)
			if sleepErr := s.sleepFn(ctx, jitter); sleepErr != nil {
				runErrors = append(runErrors, fmt.Sprintf("%s: canceled before run (%v)", planned.requestedModel, sleepErr))
				break
			}
		}

		runResult := s.executeWarmupBucketRun(ctx, account, planned, opts.effort, seenPrompts)
		res.Runs = append(res.Runs, runResult)
		if runResult.Error == "" && runResult.StatusCode == http.StatusOK {
			anySucceeded = true
		} else if runResult.Error != "" {
			runErrors = append(runErrors, fmt.Sprintf("%s: %s", planned.requestedModel, runResult.Error))
		} else {
			runErrors = append(runErrors, fmt.Sprintf("%s: status %d", planned.requestedModel, runResult.StatusCode))
		}
	}

	if len(res.Runs) == len(plannedRuns) && len(runErrors) == 0 && len(res.Runs) > 0 {
		res.Success = true
	} else if len(runErrors) > 0 {
		res.Error = strings.Join(runErrors, "; ")
	}

	if opts.refreshUsage && anySucceeded && s.usageRefresher != nil {
		if sleepErr := s.sleepFn(ctx, warmupPostRefreshDelay); sleepErr != nil {
			res.UsageError = fmt.Sprintf("usage refresh canceled: %v", sleepErr)
		} else {
			usage, usageErr := s.usageRefresher.GetUsage(ctx, account.ID, true)
			if usageErr != nil {
				res.UsageError = usageErr.Error()
			} else if usage != nil {
				res.Usage = usage
				if strings.TrimSpace(usage.Error) != "" {
					res.UsageError = usage.Error
				}
			}
		}
	}

	return res
}

func resolveWarmupPlannedRuns(
	ctx context.Context,
	account *Account,
	bucketMode string,
	customModels []string,
	requestedEffort string,
) ([]warmupPlannedRun, error) {
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return nil, errors.New("account model_mapping is empty")
	}

	switch bucketMode {
	case WarmupBucketModeAutoBuckets:
		geminiRuns := resolveGeminiWarmupRuns(account, mapping)
		claudeRuns := resolveClaudeWarmupRuns(mapping)
		combined := make([]warmupPlannedRun, 0, len(geminiRuns)+len(claudeRuns))
		combined = append(combined, geminiRuns...)
		combined = append(combined, claudeRuns...)
		if len(combined) == 0 {
			return nil, errors.New("no supported gemini or claude models found in account model_mapping")
		}
		return combined, nil

	case WarmupBucketModeGeminiOnly:
		geminiRuns := resolveGeminiWarmupRuns(account, mapping)
		if len(geminiRuns) == 0 {
			return nil, errors.New("no supported gemini model found in account model_mapping")
		}
		return geminiRuns, nil

	case WarmupBucketModeClaudeOnly:
		claudeRuns := resolveClaudeWarmupRuns(mapping)
		if len(claudeRuns) == 0 {
			return nil, errors.New("no supported claude model found in account model_mapping")
		}
		return claudeRuns, nil

	case WarmupBucketModeCustomModels:
		policy := antigravityEffortPolicyFromContext(WithAntigravityEffortPolicySnapshot(ctx))
		checkEffort := requestedEffort
		if checkEffort == "" {
			checkEffort = "low"
		}
		runs := make([]warmupPlannedRun, 0, len(customModels))
		for _, m := range customModels {
			decision := decideAntigravityEffortMapping(mapping, m, checkEffort, policy)
			if !decision.matched {
				// Retry with empty effort in case low is disabled globally and template uses another default.
				decision = decideAntigravityEffortMapping(mapping, m, "", policy)
			}
			if !decision.matched {
				return nil, fmt.Errorf("model %q is not configured in account model_mapping", m)
			}
			runs = append(runs, warmupPlannedRun{
				bucket:         classifyWarmupBucket(m, decision.model),
				requestedModel: m,
			})
		}
		return runs, nil

	default:
		return nil, fmt.Errorf("unsupported bucket_mode %q", bucketMode)
	}
}

func resolveGeminiWarmupRuns(account *Account, mapping map[string]string) []warmupPlannedRun {
	primary := ""
	for _, candidate := range warmupGeminiPriority {
		if target, ok := mapping[candidate]; ok && strings.TrimSpace(target) != "" {
			primary = candidate
			break
		}
	}
	if primary == "" {
		keys := sortedMappingKeys(mapping)
		for _, k := range keys {
			lower := strings.ToLower(k)
			if strings.HasPrefix(lower, "gemini-") && !strings.Contains(lower, "image") && strings.TrimSpace(mapping[k]) != "" {
				primary = k
				break
			}
		}
	}
	if primary == "" {
		return nil
	}

	runs := []warmupPlannedRun{{
		bucket:         "gemini",
		requestedModel: primary,
	}}

	// Pro accounts maintain a separate versioned Argon 7d pool when gemini-4-argon is mapped alongside another Gemini model.
	if primary != "gemini-4-argon" && strings.EqualFold(strings.TrimSpace(account.GetCredential("plan_type")), "Pro") {
		if target, ok := mapping["gemini-4-argon"]; ok && strings.TrimSpace(target) != "" {
			runs = append(runs, warmupPlannedRun{
				bucket:         "gemini",
				requestedModel: "gemini-4-argon",
			})
		}
	}
	return runs
}

func resolveClaudeWarmupRuns(mapping map[string]string) []warmupPlannedRun {
	primary := ""
	for _, candidate := range warmupClaudePriority {
		if target, ok := mapping[candidate]; ok && strings.TrimSpace(target) != "" {
			primary = candidate
			break
		}
	}
	if primary == "" {
		keys := sortedMappingKeys(mapping)
		for _, k := range keys {
			lower := strings.ToLower(k)
			if strings.HasPrefix(lower, "claude-") && strings.TrimSpace(mapping[k]) != "" {
				primary = k
				break
			}
		}
	}
	if primary == "" {
		return nil
	}
	return []warmupPlannedRun{{
		bucket:         "claude",
		requestedModel: primary,
	}}
}

func sortedMappingKeys(mapping map[string]string) []string {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		if strings.Contains(k, "*") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func classifyWarmupBucket(requestedModel, resolvedModel string) string {
	for _, m := range []string{resolvedModel, requestedModel} {
		lower := strings.ToLower(strings.TrimSpace(m))
		if strings.HasPrefix(lower, "gemini-") {
			return "gemini"
		}
		if strings.HasPrefix(lower, "claude-") {
			return "claude"
		}
	}
	return "custom"
}

func buildWarmupRequestPayload(
	baseCtx context.Context,
	account *Account,
	requestedModel string,
	requestedEffort string,
	promptText string,
) ([]byte, context.Context, string, error) {
	ctx := WithSingleAccountRetry(baseCtx, true, true)
	ctx = WithAntigravityEffortPolicySnapshot(ctx)

	mapping := account.GetModelMapping()
	target, matched := resolveRequestedModelInMapping(mapping, requestedModel)
	userID := buildWarmupMetadataUserID()

	newBasePayload := func(maxTokens int) map[string]any {
		return map[string]any{
			"model":      requestedModel,
			"stream":     true,
			"max_tokens": maxTokens,
			"metadata": map[string]any{
				"user_id": userID,
			},
			"messages": []map[string]any{
				{
					"role":    "user",
					"content": promptText,
				},
			},
		}
	}

	if matched && strings.Contains(target, "{effort") {
		chosenEffort := requestedEffort
		if chosenEffort == "" {
			chosenEffort = "low"
		}

		marshalWithEffort := func(eff string) ([]byte, context.Context, error) {
			payload := newBasePayload(warmupThinkingMaxTokens)
			payload["output_config"] = map[string]any{"effort": eff}
			payload["thinking"] = map[string]any{
				"type":          "enabled",
				"budget_tokens": warmupThinkingBudgetTokens,
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, ctx, err
			}
			effCtx := WithAntigravityRequestEffort(ctx, raw)
			effCtx = WithThinkingEnabled(effCtx, true, true)
			return raw, effCtx, nil
		}

		body, effCtx, err := marshalWithEffort(chosenEffort)
		if err != nil {
			return nil, ctx, "", err
		}
		if errMsg := antigravityEffortRequestError(effCtx, account, requestedModel); errMsg != "" {
			if chosenEffort == "low" {
				fallbackBody, fallbackCtx, fbErr := marshalWithEffort("high")
				if fbErr == nil && antigravityEffortRequestError(fallbackCtx, account, requestedModel) == "" {
					body = fallbackBody
					effCtx = fallbackCtx
				} else {
					return nil, effCtx, "", errors.New(errMsg)
				}
			} else {
				return nil, effCtx, "", errors.New(errMsg)
			}
		}

		resolved := resolveFinalAntigravityModelKey(effCtx, account, requestedModel)
		if resolved == "" {
			return nil, effCtx, "", fmt.Errorf("model %q could not be resolved for account", requestedModel)
		}
		return body, effCtx, resolved, nil
	}

	// Non-{effort} target: inspect -thinking suffix or pre-expanded template suffix in custom_models.
	policy := antigravityEffortPolicyFromContext(ctx)
	preExpandedTemplate := !matched && decideAntigravityEffortMapping(mapping, requestedModel, "", policy).effortApplied
	isThinkingSuffix := strings.HasSuffix(strings.ToLower(requestedModel), "-thinking") ||
		strings.HasSuffix(strings.ToLower(target), "-thinking")

	var payload map[string]any
	thinkingEnabled := false
	if isThinkingSuffix || preExpandedTemplate {
		payload = newBasePayload(warmupThinkingMaxTokens)
		payload["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": warmupThinkingBudgetTokens,
		}
		thinkingEnabled = true
	} else {
		payload = newBasePayload(warmupStandardMaxTokens)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ctx, "", err
	}
	reqCtx := WithAntigravityRequestEffort(ctx, body)
	if thinkingEnabled {
		reqCtx = WithThinkingEnabled(reqCtx, true, true)
	}
	if errMsg := antigravityEffortRequestError(reqCtx, account, requestedModel); errMsg != "" {
		return nil, reqCtx, "", errors.New(errMsg)
	}
	resolved := resolveFinalAntigravityModelKey(reqCtx, account, requestedModel)
	if resolved == "" {
		if account.Type == AccountTypeUpstream {
			resolved = requestedModel
		} else {
			return nil, reqCtx, "", fmt.Errorf("model %q is not in account model_mapping", requestedModel)
		}
	}
	return body, reqCtx, resolved, nil
}

func (s *AccountWarmupService) executeWarmupBucketRun(
	ctx context.Context,
	account *Account,
	planned warmupPlannedRun,
	requestedEffort string,
	seenPrompts *sync.Map,
) WarmupBucketRunResult {
	startTime := time.Now()
	runRes := WarmupBucketRunResult{
		Bucket:         planned.bucket,
		RequestedModel: planned.requestedModel,
	}

	prompt, err := generateUniqueWarmupPrompt(seenPrompts)
	if err != nil {
		runRes.DurationMs = time.Since(startTime).Milliseconds()
		runRes.Error = fmt.Sprintf("generate warmup prompt: %v", err)
		return runRes
	}
	runRes.PromptSummary = prompt.PromptSummary
	runRes.ExpectedAnswer = prompt.ExpectedAnswer

	body, runCtx, resolvedModel, err := buildWarmupRequestPayload(ctx, account, planned.requestedModel, requestedEffort, prompt.Prompt)
	runRes.ResolvedModel = resolvedModel
	if err != nil {
		runRes.StatusCode = http.StatusBadRequest
		runRes.DurationMs = time.Since(startTime).Milliseconds()
		runRes.Error = err.Error()
		return runRes
	}

	gateway := s.antigravityGatewayService
	rawState := &warmupUpstreamStreamState{}
	isolated := NewAntigravityGatewayService(
		gateway.accountRepo,
		gateway.cache,
		gateway.schedulerSnapshot,
		gateway.tokenProvider,
		gateway.rateLimitService,
		&warmupObservedUpstream{HTTPUpstream: gateway.httpUpstream, state: rawState},
		gateway.settingService,
		gateway.internal500Cache,
		gateway.proxyStreamCircuit,
		gateway.tlsFPProfileService,
	)

	recorder := newWarmupResponseRecorder(runCtx)
	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost, "http://sub2api.internal/v1/messages", bytes.NewReader(body))
	if err != nil {
		runRes.DurationMs = time.Since(startTime).Milliseconds()
		runRes.Error = fmt.Sprintf("build warmup request: %v", err)
		return runRes
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	ginCtx := &gin.Context{
		Request: httpReq,
		Writer:  recorder,
	}

	forwardResult, forwardErr := isolated.Forward(runCtx, ginCtx, account, body, false)
	recorder.finalize()
	runRes.DurationMs = time.Since(startTime).Milliseconds()

	if forwardResult != nil && strings.TrimSpace(forwardResult.UpstreamModel) != "" {
		runRes.ResolvedModel = forwardResult.UpstreamModel
	}

	rawSnap := rawState.snapshot()
	statusCode := resolveWarmupStatusCode(recorder, rawSnap, forwardErr)
	runRes.StatusCode = statusCode

	assistantText := strings.TrimSpace(recorder.textBuilder.String())
	thinkingText := strings.TrimSpace(recorder.thinkingBuilder.String())
	excerptSource := assistantText
	if excerptSource == "" && thinkingText != "" {
		excerptSource = thinkingText
	}
	runRes.ResponseExcerpt = formatWarmupResponseExcerpt(excerptSource, warmupMaxResponseExcerptLen)
	runRes.AnswerMatched = matchWarmupExpectedAnswer(assistantText, prompt.ExpectedAnswer)

	if forwardErr != nil {
		runRes.Error = extractWarmupForwardError(forwardErr, recorder)
		return runRes
	}
	if statusCode != http.StatusOK {
		runRes.Error = extractWarmupRecorderError(statusCode, recorder)
		return runRes
	}
	if rawSnap.failure != nil {
		runRes.Error = rawSnap.failure.Error()
		return runRes
	}
	if recorder.streamError != "" {
		runRes.Error = recorder.streamError
		return runRes
	}
	if !recorder.hasMessageStop && !recorder.hasStopReason && !rawSnap.terminal {
		runRes.Error = "upstream stream ended without generation completion"
		return runRes
	}
	if !recorder.hasCompletion {
		runRes.Error = "upstream returned empty assistant text and thinking"
		return runRes
	}

	s.clearStaleModelRateLimits(runCtx, account, planned.requestedModel, runRes.ResolvedModel)
	return runRes
}

func (s *AccountWarmupService) clearStaleModelRateLimits(
	ctx context.Context,
	account *Account,
	requestedModel string,
	resolvedModel string,
) {
	if s == nil || s.accountRepo == nil || account == nil || len(account.Extra) == 0 {
		return
	}
	rawLimits, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	if !ok || len(rawLimits) == 0 {
		return
	}

	now := time.Now()
	clearKeys := map[string]struct{}{}
	if m := strings.TrimSpace(resolvedModel); m != "" && m != creditsExhaustedKey {
		clearKeys[m] = struct{}{}
	}
	if m := strings.TrimSpace(requestedModel); m != "" && m != creditsExhaustedKey {
		clearKeys[m] = struct{}{}
	}

	remaining := make(map[string]any, len(rawLimits))
	changed := false
	for key, val := range rawLimits {
		if _, shouldClear := clearKeys[key]; shouldClear {
			changed = true
			continue
		}
		if resetAt := account.modelRateLimitResetAt(key); resetAt != nil && !now.Before(*resetAt) {
			changed = true
			continue
		}
		remaining[key] = val
	}
	if !changed {
		return
	}

	if len(remaining) == 0 {
		if err := s.accountRepo.ClearModelRateLimits(ctx, account.ID); err != nil {
			logger.LegacyPrintf("service.account_warmup", "clear model_rate_limits failed account=%d err=%v", account.ID, err)
			return
		}
		delete(account.Extra, modelRateLimitsKey)
	} else {
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{modelRateLimitsKey: remaining}); err != nil {
			logger.LegacyPrintf("service.account_warmup", "update model_rate_limits failed account=%d err=%v", account.ID, err)
			return
		}
		account.Extra[modelRateLimitsKey] = remaining
	}

	if s.antigravityGatewayService != nil && s.antigravityGatewayService.schedulerSnapshot != nil {
		_ = s.antigravityGatewayService.schedulerSnapshot.UpdateAccountInCache(ctx, account)
	}
}

func resolveWarmupStatusCode(recorder *warmupResponseRecorder, rawSnap warmupUpstreamStreamSnapshot, forwardErr error) int {
	var failoverErr *UpstreamFailoverError
	if errors.As(forwardErr, &failoverErr) && failoverErr.StatusCode > 0 {
		return failoverErr.StatusCode
	}
	var promptTooLong *PromptTooLongError
	if errors.As(forwardErr, &promptTooLong) && promptTooLong.StatusCode > 0 {
		return promptTooLong.StatusCode
	}
	if recorder.wroteHeader && recorder.statusCode >= 400 {
		return recorder.statusCode
	}
	if rawSnap.statusCode > 0 {
		return rawSnap.statusCode
	}
	if recorder.wroteHeader && recorder.statusCode > 0 {
		return recorder.statusCode
	}
	if forwardErr == nil {
		return http.StatusOK
	}
	return 0
}

func extractWarmupForwardError(forwardErr error, recorder *warmupResponseRecorder) string {
	var failoverErr *UpstreamFailoverError
	if errors.As(forwardErr, &failoverErr) {
		if msg := strings.TrimSpace(extractAntigravityErrorMessage(failoverErr.ResponseBody)); msg != "" {
			return fmt.Sprintf("upstream HTTP %d: %s", failoverErr.StatusCode, sanitizeUpstreamErrorMessage(msg))
		}
		return fmt.Sprintf("upstream failover HTTP %d", failoverErr.StatusCode)
	}
	var promptTooLong *PromptTooLongError
	if errors.As(forwardErr, &promptTooLong) {
		if msg := strings.TrimSpace(extractAntigravityErrorMessage(promptTooLong.Body)); msg != "" {
			return fmt.Sprintf("upstream HTTP %d: %s", promptTooLong.StatusCode, sanitizeUpstreamErrorMessage(msg))
		}
		return promptTooLong.Error()
	}
	if recorder.streamError != "" {
		return recorder.streamError
	}
	if rawMsg := strings.TrimSpace(gjson.GetBytes(recorder.rawBody.Bytes(), "error.message").String()); rawMsg != "" {
		return rawMsg
	}
	return forwardErr.Error()
}

func extractWarmupRecorderError(statusCode int, recorder *warmupResponseRecorder) string {
	if recorder.streamError != "" {
		return recorder.streamError
	}
	if rawMsg := strings.TrimSpace(gjson.GetBytes(recorder.rawBody.Bytes(), "error.message").String()); rawMsg != "" {
		return fmt.Sprintf("HTTP %d: %s", statusCode, rawMsg)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

type warmupUpstreamStreamSnapshot struct {
	statusCode int
	terminal   bool
	failure    error
}

type warmupUpstreamStreamState struct {
	mu         sync.Mutex
	statusCode int
	terminal   bool
	failure    error
	pending    string
}

func (s *warmupUpstreamStreamState) resetAttempt(statusCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = statusCode
	s.terminal = false
	s.failure = nil
	s.pending = ""
}

func (s *warmupUpstreamStreamState) recordStatus(statusCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = statusCode
}

func (s *warmupUpstreamStreamState) snapshot() warmupUpstreamStreamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return warmupUpstreamStreamSnapshot{
		statusCode: s.statusCode,
		terminal:   s.terminal,
		failure:    s.failure,
	}
}

func (s *warmupUpstreamStreamState) observeLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if raw == "" || raw == "[DONE]" {
		return
	}
	data := []byte(raw)
	if !gjson.ValidBytes(data) {
		return
	}
	if e := gjson.GetBytes(data, "error"); e.Exists() {
		msg := strings.TrimSpace(e.Get("message").String())
		if msg == "" {
			msg = "upstream stream error"
		}
		s.failure = fmt.Errorf("upstream stream error: %s", msg)
	}
	candidates := gjson.GetBytes(data, "response.candidates")
	if !candidates.Exists() {
		candidates = gjson.GetBytes(data, "candidates")
	}
	for _, candidate := range candidates.Array() {
		reason := candidate.Get("finishReason").String()
		switch reason {
		case "":
		case "STOP", "MAX_TOKENS":
			s.terminal = true
		default:
			s.failure = fmt.Errorf("upstream generation ended with %s", reason)
		}
	}
}

func (s *warmupUpstreamStreamState) observe(p []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 0 {
		s.pending += string(p)
		for {
			idx := strings.IndexByte(s.pending, '\n')
			if idx < 0 {
				break
			}
			line := strings.TrimSuffix(s.pending[:idx], "\r")
			s.observeLine(line)
			s.pending = s.pending[idx+1:]
		}
		if len(s.pending) > 8<<20 {
			s.failure = errors.New("upstream stream frame exceeds 8 MiB")
			s.pending = ""
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if s.pending != "" {
				s.observeLine(strings.TrimSuffix(s.pending, "\r"))
				s.pending = ""
			}
		} else if s.failure == nil {
			s.failure = err
		}
	}
}

type warmupObservedBody struct {
	io.ReadCloser
	state *warmupUpstreamStreamState
}

func (b *warmupObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.state.observe(p[:n], err)
	return n, err
}

type warmupObservedUpstream struct {
	HTTPUpstream
	state *warmupUpstreamStreamState
}

func (u *warmupObservedUpstream) wrap(resp *http.Response, err error) (*http.Response, error) {
	if resp != nil {
		if err == nil && resp.StatusCode == http.StatusOK && resp.Body != nil {
			u.state.resetAttempt(resp.StatusCode)
			resp.Body = &warmupObservedBody{ReadCloser: resp.Body, state: u.state}
		} else {
			u.state.recordStatus(resp.StatusCode)
		}
	}
	return resp, err
}

func (u *warmupObservedUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	if u.HTTPUpstream == nil {
		return nil, errors.New("http upstream is not configured")
	}
	resp, err := u.HTTPUpstream.Do(req, proxy, id, concurrency)
	return u.wrap(resp, err)
}

func (u *warmupObservedUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if u.HTTPUpstream == nil {
		return nil, errors.New("http upstream is not configured")
	}
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
	return u.wrap(resp, err)
}

// warmupResponseRecorder captures Claude SSE frames streamed by AntigravityGatewayService.Forward
// in memory without allocating a Gin engine.
type warmupResponseRecorder struct {
	ctx             context.Context
	headers         http.Header
	statusCode      int
	wroteHeader     bool
	size            int
	rawBody         bytes.Buffer
	pendingSSE      string
	textBuilder     strings.Builder
	thinkingBuilder strings.Builder
	hasCompletion   bool
	hasStopReason   bool
	hasMessageStop  bool
	streamError     string
}

var _ gin.ResponseWriter = (*warmupResponseRecorder)(nil)

func newWarmupResponseRecorder(ctx context.Context) *warmupResponseRecorder {
	return &warmupResponseRecorder{
		ctx:        ctx,
		headers:    make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (w *warmupResponseRecorder) Header() http.Header {
	return w.headers
}

func (w *warmupResponseRecorder) WriteHeader(code int) {
	if !w.wroteHeader || code >= 400 {
		w.statusCode = code
		w.wroteHeader = true
	}
}

func (w *warmupResponseRecorder) WriteHeaderNow() {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
}

func (w *warmupResponseRecorder) Status() int {
	return w.statusCode
}

func (w *warmupResponseRecorder) Size() int {
	return w.size
}

func (w *warmupResponseRecorder) Written() bool {
	return w.wroteHeader || w.size > 0
}

func (w *warmupResponseRecorder) Flush() {}

func (w *warmupResponseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack not supported on warmup recorder")
}

func (w *warmupResponseRecorder) CloseNotify() <-chan bool {
	ch := make(chan bool)
	return ch
}

func (w *warmupResponseRecorder) Pusher() http.Pusher {
	return nil
}

func (w *warmupResponseRecorder) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *warmupResponseRecorder) Write(p []byte) (int, error) {
	if w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	w.size += len(p)
	if w.rawBody.Len() < 64<<10 {
		_, _ = w.rawBody.Write(p)
	}
	w.pendingSSE += strings.ReplaceAll(string(p), "\r\n", "\n")
	for {
		sep := strings.Index(w.pendingSSE, "\n\n")
		if sep < 0 {
			break
		}
		frame := w.pendingSSE[:sep]
		w.pendingSSE = w.pendingSSE[sep+2:]
		w.consumeSSEFrame(frame)
	}
	return len(p), nil
}

func (w *warmupResponseRecorder) finalize() {
	if strings.TrimSpace(w.pendingSSE) != "" {
		w.consumeSSEFrame(w.pendingSSE)
		w.pendingSSE = ""
	}
}

func (w *warmupResponseRecorder) consumeSSEFrame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" || !gjson.Valid(raw) {
			continue
		}
		ev := gjson.Parse(raw)
		switch ev.Get("type").String() {
		case "content_block_delta":
			switch ev.Get("delta.type").String() {
			case "text_delta":
				text := ev.Get("delta.text").String()
				if text != "" {
					w.textBuilder.WriteString(text)
				}
				if strings.TrimSpace(text) != "" {
					w.hasCompletion = true
				}
			case "thinking_delta":
				thinking := ev.Get("delta.thinking").String()
				if thinking != "" {
					w.thinkingBuilder.WriteString(thinking)
				}
				if strings.TrimSpace(thinking) != "" {
					w.hasCompletion = true
				}
			}
		case "content_block_start":
			switch ev.Get("content_block.type").String() {
			case "text":
				if text := ev.Get("content_block.text").String(); strings.TrimSpace(text) != "" {
					w.textBuilder.WriteString(text)
					w.hasCompletion = true
				}
			case "thinking":
				if thinking := ev.Get("content_block.thinking").String(); strings.TrimSpace(thinking) != "" {
					w.thinkingBuilder.WriteString(thinking)
					w.hasCompletion = true
				}
			}
		case "message_delta":
			if stopReason := strings.TrimSpace(ev.Get("delta.stop_reason").String()); stopReason != "" {
				w.hasStopReason = true
			}
		case "message_stop":
			w.hasMessageStop = true
		case "error":
			msg := strings.TrimSpace(ev.Get("error.message").String())
			if msg == "" {
				msg = "upstream stream error"
			}
			w.streamError = msg
		}
	}
}
