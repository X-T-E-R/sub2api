package service

import (
	"context"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type antigravityRateLimitWireKey struct{}

type antigravityRateLimitWireTarget struct {
	accountID int64
	model     string
}

// Bind only inside a physical Forward attempt, after mapping/transformation.
// The account identity prevents a failover from reusing another account's route.
func withAntigravityRateLimitWireModel(ctx context.Context, account *Account, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if account == nil || model == "" {
		return ctx
	}
	return context.WithValue(ctx, antigravityRateLimitWireKey{}, antigravityRateLimitWireTarget{account.ID, model})
}

func antigravityRateLimitWireModel(ctx context.Context, account *Account) string {
	if ctx == nil || account == nil {
		return ""
	}
	target, ok := ctx.Value(antigravityRateLimitWireKey{}).(antigravityRateLimitWireTarget)
	if !ok || target.accountID != account.ID {
		return ""
	}
	return target.model
}

func normalizeAntigravityModelName(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(normalized, "/publishers/google/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/google/models/"):]
	} else if idx := strings.LastIndex(normalized, "/publishers/anthropic/models/"); idx != -1 {
		normalized = normalized[idx+len("/publishers/anthropic/models/"):]
	} else if idx := strings.LastIndex(normalized, "/models/"); idx != -1 {
		normalized = normalized[idx+len("/models/"):]
	} else {
		normalized = strings.TrimPrefix(normalized, "publishers/google/models/")
		normalized = strings.TrimPrefix(normalized, "publishers/anthropic/models/")
		normalized = strings.TrimPrefix(normalized, "models/")
	}
	return normalized
}

// resolveAntigravityModelKey 根据请求的模型名解析限流 key
// 返回空字符串表示无法解析
func resolveAntigravityModelKey(requestedModel string) string {
	return normalizeAntigravityModelName(requestedModel)
}

// IsSchedulableForModel 结合模型级限流判断是否可调度。
// 保持旧签名以兼容既有调用方；默认使用 context.Background()。
func (a *Account) IsSchedulableForModel(requestedModel string) bool {
	return a.IsSchedulableForModelWithContext(context.Background(), requestedModel)
}

func (a *Account) IsSchedulableForModelWithContext(ctx context.Context, requestedModel string) bool {
	if a == nil {
		return false
	}
	if !a.IsSchedulable() {
		return false
	}
	if a.isModelRateLimitedWithContext(ctx, requestedModel) {
		// Antigravity + overages 启用 + 积分未耗尽 → 放行（有积分可用）
		if a.Platform == PlatformAntigravity && a.IsOveragesEnabled() && !a.isCreditsExhausted() {
			return true
		}
		return false
	}
	return true
}

// GetRateLimitRemainingTime 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTime(requestedModel string) time.Duration {
	return a.GetRateLimitRemainingTimeWithContext(context.Background(), requestedModel)
}

// GetRateLimitRemainingTimeWithContext 获取限流剩余时间（模型级限流）
// 返回 0 表示未限流或已过期
func (a *Account) GetRateLimitRemainingTimeWithContext(ctx context.Context, requestedModel string) time.Duration {
	if a == nil {
		return 0
	}
	return a.GetModelRateLimitRemainingTimeWithContext(ctx, requestedModel)
}
