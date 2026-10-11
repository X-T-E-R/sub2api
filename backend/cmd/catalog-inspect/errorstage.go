package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// 本工具对外只输出**有限枚举**的失败信号，不输出任何自由文本错误。
//
// 原因：上游错误体会把原始响应原样拼进 error（`fetchAvailableModels 失败 (HTTP %d): %s`），
// 数据库错误会带上 libpq DSN（`host=... password=...`），令牌端点错误会回显 refresh_token。
// “打印前脱敏”要一直赌正则不漏；改成有限 stage 后，任何原始文本都没有出口：
// 唯一从错误里提取出来的非文本信息是一个 100..599 的 HTTP 状态码。
const (
	// 账号解析/入参阶段（全部为有限模板，不回显输入）。
	stageAccountListEmpty    = "invalid_arguments_account_list_empty"
	stageAccountIDInvalid    = "invalid_arguments_account_id_invalid"
	stageAccountIDOutsideSet = "invalid_arguments_account_id_outside_fixed_set"

	// 环境准备阶段。
	stageConfigLoadFailed     = "config_load_failed"
	stageTimezoneInitFailed   = "timezone_init_failed"
	stageDatabaseOpenFailed   = "database_open_failed"
	stageDatabaseUnreachable  = "database_unreachable"
	stageLibrarySilenceFailed = "library_output_silence_failed"
	stageReportEncodeFailed   = "report_encode_failed"

	// 单账号阶段。
	stageAccountNotFound   = "account_not_found"
	stageAccountLoadFailed = "account_load_failed"
	stageNotAntigravity    = "not_antigravity_account"
	stageNotOAuth          = "not_oauth_account"
	stageTokenUnavailable  = "token_unavailable"
	stageTokenMissing      = "token_missing"
	stageClientSetupFailed = "client_setup_failed"
	stageFetchForbidden    = "fetch_forbidden"
	stageFetchFailed       = "fetch_failed"
	stageEmptyResponse     = "empty_response"
	stageUnknownFailure    = "unknown_failure"
)

// reportError 是账号级失败信号：stage 取自上面的有限枚举，
// http_status 只在能从错误里精确提取到 3 位状态码时出现。
type reportError struct {
	Stage      string `json:"stage"`
	HTTPStatus *int   `json:"http_status,omitempty"`
}

// stageError 携带有限 stage 字符串，供全局失败路径保持同样的有限性。
type stageError struct {
	stage string
}

func (e *stageError) Error() string { return e.stage }

func newStageError(stage string) error { return &stageError{stage: stage} }

// stageOf 把任意错误压成有限 stage；未知错误也落在一个固定的 stage 上。
func stageOf(err error) string {
	var staged *stageError
	if errors.As(err, &staged) {
		return staged.stage
	}
	return stageUnknownFailure
}

func newReportError(stage string) *reportError {
	return &reportError{Stage: stage}
}

// tokenFailure 归类“取 access token 失败”。令牌端点错误里唯一可信的结构信息就是状态码
// （例如 invalid_grant 对应 400），其余文本全部丢弃。
func tokenFailure(err error) *reportError {
	return stageWithStatus(stageTokenUnavailable, err)
}

// fetchFailure 归类“一次逻辑 fetchAvailableModels 失败”。
// 403 有专门的类型化错误；其他失败尽量取出 HTTP 状态码，便于区分上游故障与凭据问题。
func fetchFailure(err error) *reportError {
	var forbidden *antigravity.ForbiddenError
	if errors.As(err, &forbidden) {
		status := forbidden.StatusCode
		if status < 100 || status > 599 {
			return newReportError(stageFetchForbidden)
		}
		return &reportError{Stage: stageFetchForbidden, HTTPStatus: &status}
	}
	return stageWithStatus(stageFetchFailed, err)
}

// stageWithStatus 只从错误文本里精确提取 `(HTTP <ddd>)` 形式的状态码。
func stageWithStatus(stage string, err error) *reportError {
	report := newReportError(stage)
	if err == nil {
		return report
	}
	if status, ok := extractHTTPStatus(err.Error()); ok {
		report.HTTPStatus = &status
	}
	return report
}

// httpStatusPattern 匹配上游错误文本里的 `(HTTP 403)` 片段；命中的数字之外的内容不会被使用。
var httpStatusPattern = regexp.MustCompile(`\(HTTP (\d{3})\)`)

// extractHTTPStatus 返回错误文本里的 HTTP 状态码；失败量本身就是有限枚举的一部分。
func extractHTTPStatus(text string) (int, bool) {
	match := httpStatusPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	status, err := strconv.Atoi(match[1])
	if err != nil || status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}

// describeAccountFailure 生成退出码 1 时 stderr 上的诊断行：账号 ID + 有限 stage。
func describeAccountFailure(accountID int64, reportErr *reportError) string {
	stage := stageUnknownFailure
	if reportErr != nil && reportErr.Stage != "" {
		stage = reportErr.Stage
	}
	if reportErr != nil && reportErr.HTTPStatus != nil {
		return fmt.Sprintf("catalog-inspect: account %d failed stage=%s http_status=%d", accountID, stage, *reportErr.HTTPStatus)
	}
	return fmt.Sprintf("catalog-inspect: account %d failed stage=%s", accountID, stage)
}
