package main

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// fixedAccountIDs 是本工具唯一允许取数的账号集合。任何集合外的 ID 都直接失败关闭，
// 不接受任意账号 ID，避免临时工具被当成通用目录探测器。
var fixedAccountIDs = []int64{63, 64, 69}

// reportSource 标记报告数据的来源接口，避免下游把报告当成别处的模型清单。
const reportSource = "antigravity.fetchAvailableModels"

// modelCapability 是单个模型的“能力投影”：只保留选中字段，
// 不含原始响应、凭证、project、email 或请求头。
type modelCapability struct {
	ID                     string   `json:"id"`
	DisplayName            string   `json:"display_name,omitempty"`
	SupportsImages         *bool    `json:"supports_images,omitempty"`
	SupportsThinking       *bool    `json:"supports_thinking,omitempty"`
	ThinkingBudget         *int     `json:"thinking_budget,omitempty"`
	Recommended            *bool    `json:"recommended,omitempty"`
	MaxTokens              *int     `json:"max_tokens,omitempty"`
	MaxOutputTokens        *int     `json:"max_output_tokens,omitempty"`
	SupportedMimeTypes     []string `json:"supported_mime_types,omitempty"`
	QuotaRemainingFraction *float64 `json:"quota_remaining_fraction,omitempty"`
	QuotaResetTime         string   `json:"quota_reset_time,omitempty"`
}

// accountReport 是单个账号的一次性巡检结果。
//
// Error 恒存在（成功时为 null）且只包含有限 stage 字符串与可选 HTTP 状态码。
// FetchAvailableModels 在 URL 降级成功时也返回成功，此时 Endpoint 是被客户端实际使用的
// 那个 base URL；失败时客户端不暴露最后尝试的 URL，Endpoint 为空且只报告 ConfiguredEndpoints。
type accountReport struct {
	AccountID           int64             `json:"account_id"`
	FetchedAt           string            `json:"fetched_at"`
	Endpoint            string            `json:"endpoint"`
	ConfiguredEndpoints []string          `json:"configured_endpoints"`
	ModelCount          int               `json:"model_count"`
	ModelIDs            []string          `json:"model_ids"`
	Models              []modelCapability `json:"models"`
	DeprecatedModelIDs  map[string]string `json:"deprecated_model_ids,omitempty"`
	Error               *reportError      `json:"error"`
}

// report 是整个命令行输出（stdout 上唯一的 JSON 文档）。
type report struct {
	GeneratedAt string          `json:"generated_at"`
	Source      string          `json:"source"`
	Accounts    []accountReport `json:"accounts"`
}

// tokenSource 是取得 Antigravity access token 的窄接口。
// 生产实现是 *service.AntigravityTokenProvider（见下方编译期断言）。
type tokenSource interface {
	GetAccessTokenForQuota(ctx context.Context, account *service.Account) (string, *service.Account, error)
}

// modelFetcher 是一次逻辑 FetchAvailableModels 调用的窄接口。
// 生产实现是 *antigravity.Client（见下方编译期断言）。
type modelFetcher interface {
	FetchAvailableModels(ctx context.Context, accessToken, projectID string, bodyLimit int64) (*antigravity.FetchAvailableModelsResponse, map[string]any, error)
}

// fetchFactory 按账号构造客户端：代理跟随账号行，与线上模型同步路径一致。
type fetchFactory func(account *service.Account) (modelFetcher, error)

// 编译期断言：正式实现必须持续满足本工具依赖的接口形状。
var (
	_ tokenSource  = (*service.AntigravityTokenProvider)(nil)
	_ modelFetcher = (*antigravity.Client)(nil)
)

// inspector 持有一次巡检所需的依赖，每个账号最多发起一次逻辑 fetchAvailableModels。
type inspector struct {
	tokens     tokenSource
	newFetcher fetchFactory
	bodyLimit  int64
	now        func() time.Time
}

// inspect 只读取一个账号的模型清单：先取 token（可能触发票据刷新），再调用一次 fetchAvailableModels。
func (in *inspector) inspect(ctx context.Context, account *service.Account) (out accountReport) {
	now := time.Now
	if in.now != nil {
		now = in.now
	}
	out = accountReport{
		AccountID:           account.ID,
		FetchedAt:           now().UTC().Format(time.RFC3339),
		ConfiguredEndpoints: append([]string(nil), antigravity.BaseURLs...),
		ModelIDs:            []string{},
		Models:              []modelCapability{},
	}
	defer func() { out.FetchedAt = now().UTC().Format(time.RFC3339) }()

	if account.Platform != service.PlatformAntigravity {
		out.Error = newReportError(stageNotAntigravity)
		return out
	}
	if account.Type != service.AccountTypeOAuth {
		out.Error = newReportError(stageNotOAuth)
		return out
	}

	accessToken, snapshot, err := in.tokens.GetAccessTokenForQuota(ctx, account)
	if err != nil {
		out.Error = tokenFailure(err)
		return out
	}
	if snapshot == nil {
		out.Error = newReportError(stageTokenMissing)
		return out
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		out.Error = newReportError(stageTokenMissing)
		return out
	}

	fetcher, err := in.newFetcher(snapshot)
	if err != nil {
		out.Error = newReportError(stageClientSetupFailed)
		return out
	}

	modelsResp, _, err := fetcher.FetchAvailableModels(
		ctx,
		accessToken,
		strings.TrimSpace(snapshot.GetCredential("project_id")),
		in.bodyLimit,
	)
	if err != nil {
		out.Error = fetchFailure(err)
		return out
	}
	if modelsResp == nil {
		out.Error = newReportError(stageEmptyResponse)
		return out
	}

	out.Endpoint = modelsResp.QuotaBaseURL
	out.ModelIDs, out.Models = projectCatalog(modelsResp)
	out.ModelCount = len(out.Models)
	out.DeprecatedModelIDs = deprecatedModelIDs(modelsResp)
	return out
}

// projectCatalog 把上游 map 投影成确定顺序的能力列表，并返回排序后的模型 ID。
func projectCatalog(modelsResp *antigravity.FetchAvailableModelsResponse) ([]string, []modelCapability) {
	ids := make([]string, 0, len(modelsResp.Models))
	for modelID := range modelsResp.Models {
		ids = append(ids, modelID)
	}
	sort.Strings(ids)

	capabilities := make([]modelCapability, 0, len(ids))
	for _, modelID := range ids {
		info := modelsResp.Models[modelID]
		entry := modelCapability{
			ID:                 modelID,
			DisplayName:        strings.TrimSpace(info.DisplayName),
			SupportsImages:     info.SupportsImages,
			SupportsThinking:   info.SupportsThinking,
			ThinkingBudget:     info.ThinkingBudget,
			Recommended:        info.Recommended,
			MaxTokens:          info.MaxTokens,
			MaxOutputTokens:    info.MaxOutputTokens,
			SupportedMimeTypes: supportedMimeTypes(info.SupportedMimeTypes),
		}
		if info.QuotaInfo != nil {
			entry.QuotaRemainingFraction = info.QuotaInfo.RemainingFraction
			entry.QuotaResetTime = strings.TrimSpace(info.QuotaInfo.ResetTime)
		}
		capabilities = append(capabilities, entry)
	}
	return ids, capabilities
}

// supportedMimeTypes 只保留上游显式标记为支持的类型，并排序保证输出稳定。
func supportedMimeTypes(raw map[string]bool) []string {
	if len(raw) == 0 {
		return nil
	}
	types := make([]string, 0, len(raw))
	for mimeType, supported := range raw {
		if supported {
			types = append(types, mimeType)
		}
	}
	if len(types) == 0 {
		return nil
	}
	sort.Strings(types)
	return types
}

// deprecatedModelIDs 返回废弃模型到替代模型的映射；键顺序由 encoding/json 排序保证稳定。
func deprecatedModelIDs(modelsResp *antigravity.FetchAvailableModelsResponse) map[string]string {
	if len(modelsResp.DeprecatedModelIDs) == 0 {
		return nil
	}
	deprecated := make(map[string]string, len(modelsResp.DeprecatedModelIDs))
	for modelID, info := range modelsResp.DeprecatedModelIDs {
		deprecated[modelID] = info.NewModelID
	}
	return deprecated
}

// parseAccountIDList 解析 -accounts 为去重后的正整数序列，保持输入顺序。
// 失败返回有限 stage 错误，不回显用户输入。
func parseAccountIDList(raw string) ([]int64, error) {
	ids := make([]int64, 0, len(fixedAccountIDs))
	seen := make(map[int64]struct{}, len(fixedAccountIDs))
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil || id <= 0 {
			return nil, newStageError(stageAccountIDInvalid)
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, newStageError(stageAccountListEmpty)
	}
	return ids, nil
}

// resolveFixedAccountIDs 把请求的账号收敛到固定集合，并输出固定顺序。
// 只要出现集合外的 ID 就整体失败关闭：不静默丢弃、不降级为“取能取的那些”。
func resolveFixedAccountIDs(raw string) ([]int64, error) {
	requested, err := parseAccountIDList(raw)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int64]struct{}, len(fixedAccountIDs))
	for _, id := range fixedAccountIDs {
		allowed[id] = struct{}{}
	}
	for _, id := range requested {
		if _, ok := allowed[id]; !ok {
			return nil, newStageError(stageAccountIDOutsideSet)
		}
	}

	selected := make([]int64, 0, len(requested))
	for _, id := range fixedAccountIDs {
		for _, want := range requested {
			if want == id {
				selected = append(selected, id)
				break
			}
		}
	}
	return selected, nil
}

// resolveModelsBodyLimit 复用线上模型列表读取上限的配置语义。
func resolveModelsBodyLimit(cfg *config.Config) int64 {
	if cfg != nil && cfg.Gateway.ModelsListReadMaxBytes > 0 {
		return cfg.Gateway.ModelsListReadMaxBytes
	}
	return config.DefaultModelsListReadMaxBytes
}

// firstAccountError 返回第一个失败账号的报告，用于决定进程退出码与 stderr 诊断行。
func firstAccountError(rep *report) (accountReport, bool) {
	for _, account := range rep.Accounts {
		if account.Error != nil {
			return account, true
		}
	}
	return accountReport{}, false
}

// fixedAccountSetDescription 供用法说明、flag 默认值与错误提示复用，避免三处漂移。
func fixedAccountSetDescription() string {
	parts := make([]string, 0, len(fixedAccountIDs))
	for _, id := range fixedAccountIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// fixedAccountIDsList 返回固定账号集合的逗号列表，用作 -accounts 的默认值。
func fixedAccountIDsList() string {
	parts := make([]string, 0, len(fixedAccountIDs))
	for _, id := range fixedAccountIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}
