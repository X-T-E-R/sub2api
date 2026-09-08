package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const (
	forbiddenTypeValidation = "validation"
	forbiddenTypeViolation  = "violation"
	forbiddenTypeForbidden  = "forbidden"

	// 机器可读的错误码
	errorCodeForbidden       = "forbidden"
	errorCodeUnauthenticated = "unauthenticated"
	errorCodeRateLimited     = "rate_limited"
	errorCodeNetworkError    = "network_error"

	antigravityObservationAvailable   AntigravityObservationState = "available"
	antigravityObservationPartial     AntigravityObservationState = "partial"
	antigravityObservationUnavailable AntigravityObservationState = "unavailable"
)

// AntigravityQuotaFetcher 从 Antigravity API 获取额度
type AntigravityQuotaFetcher struct {
	proxyRepo     ProxyRepository
	cfg           *config.Config
	tokenProvider *AntigravityTokenProvider
}

func ProvideAntigravityQuotaFetcher(proxyRepo ProxyRepository, cfg *config.Config, tokenProvider *AntigravityTokenProvider) *AntigravityQuotaFetcher {
	fetcher := NewAntigravityQuotaFetcher(proxyRepo, cfg)
	fetcher.tokenProvider = tokenProvider
	return fetcher
}

// NewAntigravityQuotaFetcher 创建 AntigravityQuotaFetcher
func NewAntigravityQuotaFetcher(proxyRepo ProxyRepository, cfg *config.Config) *AntigravityQuotaFetcher {
	return &AntigravityQuotaFetcher{proxyRepo: proxyRepo, cfg: cfg}
}

// CanFetch 检查是否可以获取此账户的额度
func (f *AntigravityQuotaFetcher) CanFetch(account *Account) bool {
	if account == nil || account.Platform != PlatformAntigravity || account.Type != AccountTypeOAuth {
		return false
	}
	accessToken := account.GetCredential("access_token")
	return accessToken != ""
}

// FetchQuota 获取 Antigravity 账户额度信息
func (f *AntigravityQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	account, err := f.prepareQuotaAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return f.fetchQuotaWithOrigin(ctx, account, proxyURL, resolveAntigravityForwardBaseURL(account))
}

// Prepare the credential snapshot before admitting a request to a quota flight.
// The flight must not reread the account and change its origin after admission.
func (f *AntigravityQuotaFetcher) prepareQuotaAccount(ctx context.Context, account *Account) (*Account, error) {
	if f.tokenProvider != nil {
		_, snapshot, err := f.tokenProvider.GetAccessTokenForQuota(ctx, account)
		return snapshot, err
	}
	return snapshotOAuthRefreshAccount(account), nil
}

func (f *AntigravityQuotaFetcher) fetchQuotaWithOrigin(ctx context.Context, account *Account, proxyURL, baseURL string) (*QuotaResult, error) {
	accessToken := account.GetCredential("access_token")
	projectID := account.GetCredential("project_id")
	scope := antigravityQuotaScopeForOrigin(account, baseURL)

	client, err := antigravity.NewQuotaClient(proxyURL, baseURL)
	if err != nil {
		return nil, fmt.Errorf("create antigravity client failed: %w", err)
	}

	// Resolve the project before quota reads; discovery never onboards or persists
	// a project and failure still permits quota reads with the stored hint.
	tierRaw, tierNormalized, loadResp, subscriptionState := f.fetchSubscriptionTier(ctx, client, accessToken, projectID)
	if loadResp != nil && strings.TrimSpace(loadResp.CloudAICompanionProject) != "" {
		projectID = strings.TrimSpace(loadResp.CloudAICompanionProject)
	}
	// Keep acquisition provenance even if the model endpoint fails after token
	// rotation. The stored credential scope and actual query project are distinct.
	result := &QuotaResult{antigravityScope: scope, antigravityProjectID: projectID}

	// 调用 API 获取配额
	modelsResp, modelsRaw, err := client.FetchAvailableModels(ctx, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	if err != nil {
		// 403 Forbidden: 不报错，返回 is_forbidden 标记
		var forbiddenErr *antigravity.ForbiddenError
		if errors.As(err, &forbiddenErr) {
			now := time.Now()
			fbType := classifyForbiddenType(forbiddenErr.Body)
			return &QuotaResult{
				antigravityScope:     scope,
				antigravityProjectID: projectID,
				UsageInfo: &UsageInfo{
					Source:                       "active",
					UpdatedAt:                    &now,
					AntigravityQuotaState:        antigravityObservationUnavailable,
					AntigravitySubscriptionState: antigravityObservationUnavailable,
					IsForbidden:                  true,
					ForbiddenReason:              forbiddenErr.Body,
					ForbiddenType:                fbType,
					ValidationURL:                extractValidationURL(forbiddenErr.Body),
					NeedsVerify:                  fbType == forbiddenTypeValidation,
					IsBanned:                     fbType == forbiddenTypeViolation,
					ErrorCode:                    errorCodeForbidden,
				},
			}, nil
		}
		return result, err
	}

	// 转换为 UsageInfo
	usageInfo := f.buildUsageInfo(modelsResp, tierRaw, tierNormalized, loadResp)
	usageInfo.AntigravitySubscriptionState = subscriptionState
	summary, summaryErr := client.RetrieveUserQuotaSummary(ctx, accessToken, projectID, modelsResp.QuotaBaseURL, resolveModelsListReadLimit(f.cfg))
	if summaryErr != nil {
		// Summary is optional; do not expose upstream bodies or discard model data.
		summary = nil
	}
	applyAntigravitySummary(usageInfo, summary, time.Now())

	result.UsageInfo = usageInfo
	result.Raw = modelsRaw
	return result, nil
}

// fetchSubscriptionTier 获取账号订阅等级，失败返回空字符串。
// 同时返回 LoadCodeAssistResponse，以便提取 AI Credits 余额。
func (f *AntigravityQuotaFetcher) fetchSubscriptionTier(ctx context.Context, client *antigravity.Client, accessToken, projectID string) (raw, normalized string, loadResp *antigravity.LoadCodeAssistResponse, state AntigravityObservationState) {
	// Discovery is optional and must leave time for the model quota request.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	loadResp, _, err := client.LoadCodeAssistForQuota(ctx, accessToken, projectID)
	if err != nil {
		slog.Warn("failed to fetch subscription tier", "error", err)
		return "", "", nil, antigravityObservationUnavailable
	}
	if loadResp == nil {
		return "", "", nil, antigravityObservationUnavailable
	}

	raw = loadResp.GetTier() // 已有方法：paidTier > currentTier
	normalized = normalizeTier(raw)
	state = antigravityObservationUnavailable
	if strings.TrimSpace(raw) != "" {
		state = antigravityObservationAvailable
	}
	return raw, normalized, loadResp, state
}

// normalizeTier 将原始 tier 字符串归一化为 FREE/PRO/ULTRA/UNKNOWN
func normalizeTier(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "ultra"):
		return "ULTRA"
	case strings.Contains(lower, "pro"):
		return "PRO"
	case strings.Contains(lower, "free"):
		return "FREE"
	default:
		return "UNKNOWN"
	}
}

// buildUsageInfo 将 API 响应转换为 UsageInfo。
func (f *AntigravityQuotaFetcher) buildUsageInfo(modelsResp *antigravity.FetchAvailableModelsResponse, tierRaw, tierNormalized string, loadResp *antigravity.LoadCodeAssistResponse) *UsageInfo {
	now := time.Now()
	info := &UsageInfo{
		Source:                  "active",
		UpdatedAt:               &now,
		AntigravityQuota:        make(map[string]*AntigravityModelQuota),
		AntigravityQuotaDetails: make(map[string]*AntigravityModelDetail),
		SubscriptionTier:        tierNormalized,
		SubscriptionTierRaw:     tierRaw,
		AntigravityQuotaState:   antigravityObservationUnavailable,
	}
	info.AntigravityIneligibleTiers = normalizeAntigravityIneligibleTiers(loadResp)
	info.AntigravityIneligible = len(info.AntigravityIneligibleTiers) > 0
	if modelsResp == nil {
		return info
	}

	// 遍历所有模型，填充 AntigravityQuota 和 AntigravityQuotaDetails
	invalidObservations := 0
	for modelName, modelInfo := range modelsResp.Models {
		if modelInfo.QuotaInfo == nil {
			continue
		}
		modelName = strings.TrimSpace(modelName)
		remainingFraction, ok := validRemainingFraction(modelInfo.QuotaInfo.RemainingFraction)
		if modelName == "" || !ok {
			invalidObservations++
			continue
		}

		// remainingFraction 是剩余比例 (0.0-1.0)，转换为使用率百分比
		utilization := int(math.Round((1.0 - remainingFraction) * 100))
		resetTime, resetValid := normalizeAntigravityResetTime(modelInfo.QuotaInfo.ResetTime)
		if !resetValid {
			invalidObservations++
		}

		info.AntigravityQuota[modelName] = &AntigravityModelQuota{
			RemainingFraction: &remainingFraction,
			Utilization:       utilization,
			ResetTime:         resetTime,
		}

		// 填充模型详细能力信息
		detail := &AntigravityModelDetail{
			DisplayName:        modelInfo.DisplayName,
			SupportsImages:     modelInfo.SupportsImages,
			SupportsThinking:   modelInfo.SupportsThinking,
			ThinkingBudget:     modelInfo.ThinkingBudget,
			Recommended:        modelInfo.Recommended,
			MaxTokens:          modelInfo.MaxTokens,
			MaxOutputTokens:    modelInfo.MaxOutputTokens,
			SupportedMimeTypes: modelInfo.SupportedMimeTypes,
		}
		info.AntigravityQuotaDetails[modelName] = detail
	}
	if len(info.AntigravityQuota) > 0 {
		info.AntigravityQuotaState = antigravityObservationAvailable
		if invalidObservations > 0 {
			info.AntigravityQuotaState = antigravityObservationPartial
		}
	}

	// 废弃模型转发规则
	if len(modelsResp.DeprecatedModelIDs) > 0 {
		info.ModelForwardingRules = make(map[string]string, len(modelsResp.DeprecatedModelIDs))
		for oldID, deprecated := range modelsResp.DeprecatedModelIDs {
			info.ModelForwardingRules[oldID] = deprecated.NewModelID
		}
	}

	if loadResp != nil {
		for _, credit := range loadResp.GetAvailableCredits() {
			if strings.TrimSpace(credit.CreditType) == "" {
				continue
			}
			info.AICredits = append(info.AICredits, AICredit{
				CreditType:         credit.CreditType,
				AmountText:         credit.CreditAmount,
				MinimumBalanceText: credit.MinimumCreditAmountForUsage,
				Amount:             parseOptionalCreditAmount(credit.CreditAmount),
				MinimumBalance:     parseOptionalCreditAmount(credit.MinimumCreditAmountForUsage),
			})
		}
	}

	return info
}

func validRemainingFraction(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1 {
		return 0, false
	}
	return *value, true
}

// normalizeAntigravityResetTime keeps a missing reset as an unknown value and
// rejects malformed non-empty timestamps without discarding the utilization.
func normalizeAntigravityResetTime(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return "", false
	}
	return value, true
}

func parseOptionalCreditAmount(value string) *float64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil
	}
	return &parsed
}

// GetProxyURL 获取账户的代理 URL
func (f *AntigravityQuotaFetcher) GetProxyURL(ctx context.Context, account *Account) string {
	if account.ProxyID == nil || f.proxyRepo == nil {
		return ""
	}
	proxy, err := f.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// classifyForbiddenType 根据 403 响应体判断禁止类型
func classifyForbiddenType(body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.Contains(lower, "validation_required") ||
		strings.Contains(lower, "verify your account") ||
		strings.Contains(lower, "validation_url"):
		return forbiddenTypeValidation
	case strings.Contains(lower, "terms of service") ||
		strings.Contains(lower, "violation"):
		return forbiddenTypeViolation
	default:
		return forbiddenTypeForbidden
	}
}

// urlPattern 用于从 403 响应体中提取 URL（降级方案）
var urlPattern = regexp.MustCompile(`https://[^\s"'\\]+`)

// extractValidationURL 从 403 响应 JSON 中提取验证/申诉链接
func extractValidationURL(body string) string {
	// 1. 尝试结构化 JSON 提取: /error/details[*]/metadata/validation_url 或 appeal_url
	var parsed struct {
		Error struct {
			Details []struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil {
		for _, detail := range parsed.Error.Details {
			if u := detail.Metadata["validation_url"]; u != "" {
				return u
			}
			if u := detail.Metadata["appeal_url"]; u != "" {
				return u
			}
		}
	}

	// 2. 降级：正则匹配 URL
	lower := strings.ToLower(body)
	if !strings.Contains(lower, "validation") &&
		!strings.Contains(lower, "verify") &&
		!strings.Contains(lower, "appeal") {
		return ""
	}
	// 先解码常见转义再匹配
	normalized := strings.ReplaceAll(body, `\u0026`, "&")
	if m := urlPattern.FindString(normalized); m != "" {
		return m
	}
	return ""
}
