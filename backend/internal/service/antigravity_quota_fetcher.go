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
	proxyRepo ProxyRepository
	cfg       *config.Config
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
	accessToken := account.GetCredential("access_token")
	projectID := account.GetCredential("project_id")

	client, err := antigravity.NewClient(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create antigravity client failed: %w", err)
	}

	// 调用 API 获取配额
	modelsResp, modelsRaw, err := client.FetchAvailableModels(ctx, accessToken, projectID, resolveModelsListReadLimit(f.cfg))
	if err != nil {
		// 403 Forbidden: 不报错，返回 is_forbidden 标记
		var forbiddenErr *antigravity.ForbiddenError
		if errors.As(err, &forbiddenErr) {
			now := time.Now()
			fbType := classifyForbiddenType(forbiddenErr.Body)
			return &QuotaResult{
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
		return nil, err
	}

	// 调用 LoadCodeAssist 获取订阅等级和 AI Credits 余额（非关键路径，失败不影响主流程）
	tierRaw, tierNormalized, loadResp, subscriptionState := f.fetchSubscriptionTier(ctx, client, accessToken)

	// 转换为 UsageInfo
	usageInfo := f.buildUsageInfo(modelsResp, tierRaw, tierNormalized, loadResp)
	usageInfo.AntigravitySubscriptionState = subscriptionState

	return &QuotaResult{
		UsageInfo: usageInfo,
		Raw:       modelsRaw,
	}, nil
}

// fetchSubscriptionTier 获取账号订阅等级，失败返回空字符串。
// 同时返回 LoadCodeAssistResponse，以便提取 AI Credits 余额。
func (f *AntigravityQuotaFetcher) fetchSubscriptionTier(ctx context.Context, client *antigravity.Client, accessToken string) (raw, normalized string, loadResp *antigravity.LoadCodeAssistResponse, state AntigravityObservationState) {
	loadResp, _, err := client.LoadCodeAssist(ctx, accessToken)
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
	if loadResp != nil {
		info.AntigravityIneligible = len(loadResp.IneligibleTiers) > 0
	}
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
			Utilization: utilization,
			ResetTime:   resetTime,
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

	// 同时设置 FiveHour 用于兼容展示（取主要模型）
	priorityModels := []string{"claude-sonnet-4-20250514", "claude-sonnet-4", "gemini-2.5-pro"}
	for _, modelName := range priorityModels {
		if modelQuota, ok := info.AntigravityQuota[modelName]; ok {
			progress := &UsageProgress{
				Utilization: float64(modelQuota.Utilization),
			}
			if modelQuota.ResetTime != "" {
				if resetTime, err := time.Parse(time.RFC3339, modelQuota.ResetTime); err == nil {
					progress.ResetsAt = &resetTime
					progress.RemainingSeconds = int(time.Until(resetTime).Seconds())
				}
			}
			info.FiveHour = progress
			break
		}
	}

	if loadResp != nil {
		for _, credit := range loadResp.GetAvailableCredits() {
			if credit.CreditType != "GOOGLE_ONE_AI" {
				continue
			}
			info.AICredits = append(info.AICredits, AICredit{
				CreditType:     credit.CreditType,
				Amount:         parseOptionalCreditAmount(credit.CreditAmount),
				MinimumBalance: parseOptionalCreditAmount(credit.MinimumCreditAmountForUsage),
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
