package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// rawUpstreamError 模拟真实的客户端错误文本：上游响应体、DSN、凭据都会被原样拼进去。
const rawUpstreamError = `fetchAvailableModels 失败 (HTTP %d): {"error":{"code":403,"message":"PERMISSION_DENIED"},` +
	`"access_token":"ya29.leaked-access-token","refresh_token":"1//leaked-refresh-token",` +
	`"project":"proj-test-123","email":"ops@example.com","dsn":"postgres://sub2api:sup3r-secret@db.internal:5432/sub2api"}`

func TestFetchFailureKeepsOnlyStageAndStatus(t *testing.T) {
	err := fmt.Errorf("fetchAvailableModels 失败 (HTTP 500): %s", rawUpstreamError)

	got := fetchFailure(err)

	require.Equal(t, stageFetchFailed, got.Stage)
	require.NotNil(t, got.HTTPStatus)
	require.Equal(t, 500, *got.HTTPStatus)
	assertFiniteReportError(t, got)
}

func TestFetchFailureClassifiesForbiddenError(t *testing.T) {
	forbidden := &antigravity.ForbiddenError{StatusCode: 403, Body: rawUpstreamError}

	got := fetchFailure(forbidden)

	require.Equal(t, stageFetchForbidden, got.Stage)
	require.NotNil(t, got.HTTPStatus)
	require.Equal(t, 403, *got.HTTPStatus)
	assertFiniteReportError(t, got)
}

func TestFetchFailureWithoutStatusStaysFinite(t *testing.T) {
	err := errors.New("fetchAvailableModels 请求失败: proxyconnect tcp: dial tcp 10.0.0.1:1080: connect: connection refused")

	got := fetchFailure(err)

	require.Equal(t, stageFetchFailed, got.Stage)
	require.Nil(t, got.HTTPStatus)
	assertFiniteReportError(t, got)
}

func TestTokenFailureKeepsStageAndStatus(t *testing.T) {
	invalidGrant := fmt.Errorf("token 刷新失败 (HTTP 400): %s", `{"error":"invalid_grant","refresh_token":"1//leaked-refresh-token"}`)

	got := tokenFailure(invalidGrant)

	require.Equal(t, stageTokenUnavailable, got.Stage)
	require.NotNil(t, got.HTTPStatus)
	require.Equal(t, 400, *got.HTTPStatus)
	assertFiniteReportError(t, got)

	network := tokenFailure(errors.New("token 刷新请求失败: Post \"https://oauth2.googleapis.com/token\": context deadline exceeded"))
	require.Equal(t, stageTokenUnavailable, network.Stage)
	require.Nil(t, network.HTTPStatus)
	assertFiniteReportError(t, network)
}

func TestExtractHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		status int
		ok     bool
	}{
		{name: "forbidden", text: "fetchAvailableModels 失败 (HTTP 403): body", status: 403, ok: true},
		{name: "server error", text: "token 刷新失败 (HTTP 500): body", status: 500, ok: true},
		{name: "two digits", text: "失败 (HTTP 99): body", ok: false},
		{name: "four digits", text: "失败 (HTTP 1000): body", ok: false},
		{name: "out of range", text: "失败 (HTTP 999): body", ok: false},
		{name: "without parentheses", text: "HTTP 403 forbidden", ok: false},
		{name: "empty", text: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, ok := extractHTTPStatus(tt.text)
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				require.Equal(t, tt.status, status)
			}
		})
	}
}

func TestStageOfIsAlwaysFinite(t *testing.T) {
	require.Equal(t, stageDatabaseUnreachable, stageOf(newStageError(stageDatabaseUnreachable)))
	require.Equal(t, stageUnknownFailure, stageOf(errors.New("raw text must never escape")))
	require.Equal(t, stageUnknownFailure, stageOf(nil))
	require.Equal(t, stageUnknownFailure, stageOf(fmt.Errorf("wrapped: %w", errors.New("raw"))))
}

func TestDescribeAccountFailureIsFinite(t *testing.T) {
	status := 403
	withStatus := describeAccountFailure(63, &reportError{Stage: stageFetchForbidden, HTTPStatus: &status})
	require.Equal(t, "catalog-inspect: account 63 failed stage=fetch_forbidden http_status=403", withStatus)

	plain := describeAccountFailure(64, &reportError{Stage: stageTokenMissing})
	require.Equal(t, "catalog-inspect: account 64 failed stage=token_missing", plain)

	require.Equal(t, "catalog-inspect: account 69 failed stage=unknown_failure", describeAccountFailure(69, nil))
}

// assertFiniteReportError 断言报告里只可能出现枚举 stage 与 100..599 的状态码。
func assertFiniteReportError(t *testing.T, reportErr *reportError) {
	t.Helper()
	require.Contains(t, allStages(), reportErr.Stage, "stage 必须属于有限枚举")
	if reportErr.HTTPStatus != nil {
		require.GreaterOrEqual(t, *reportErr.HTTPStatus, 100)
		require.LessOrEqual(t, *reportErr.HTTPStatus, 599)
	}

	raw, err := json.Marshal(reportErr)
	require.NoError(t, err)
	for _, forbidden := range []string{
		"ya29", "1//", "proj-test-123", "ops@example.com", "sup3r-secret", "postgres://",
		"PERMISSION_DENIED", "invalid_grant", "access_token", "refresh_token", "dial tcp",
	} {
		require.NotContains(t, string(raw), forbidden)
	}
	require.False(t, strings.ContainsAny(string(raw), "\n\t"))
}

// allStages 是 stage 有限枚举的权威清单，README 中列出同一份清单。
func allStages() []string {
	return []string{
		stageAccountListEmpty,
		stageAccountIDInvalid,
		stageAccountIDOutsideSet,
		stageConfigLoadFailed,
		stageTimezoneInitFailed,
		stageDatabaseOpenFailed,
		stageDatabaseUnreachable,
		stageLibrarySilenceFailed,
		stageReportEncodeFailed,
		stageAccountNotFound,
		stageAccountLoadFailed,
		stageNotAntigravity,
		stageNotOAuth,
		stageTokenUnavailable,
		stageTokenMissing,
		stageClientSetupFailed,
		stageFetchForbidden,
		stageFetchFailed,
		stageEmptyResponse,
		stageUnknownFailure,
	}
}

// TestStageEnumIsUniqueAndWellFormed 防止复制粘贴产生重复 stage
// （重复会让“有限枚举”失去判别力，两个不同故障看起来一样）。
func TestStageEnumIsUniqueAndWellFormed(t *testing.T) {
	stages := allStages()
	require.Len(t, stages, 20, "stage 数量变化时必须同步更新本清单与 README")

	seen := make(map[string]struct{}, len(stages))
	for _, stage := range stages {
		require.NotContains(t, seen, stage, "stage 重复: %s", stage)
		seen[stage] = struct{}{}
		require.Regexp(t, `^[a-z][a-z0-9_]*$`, stage, "stage 必须是 snake_case 枚举值: %s", stage)
		require.NotContains(t, stage, " ", "stage 不得包含空格")
	}
}
