package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// capturedStreams 把三个真实出口换成可读文件，并返回恢复函数：
//
//   - userStdout: os.Stdout 变量（fmt.Printf 读取它）
//   - userStderr: os.Stderr 变量（fmt.Fprintf(os.Stderr, ...) 读取它）
//   - logSink:    标准库 log 默认 logger 的输出，在进程里就是真正的 fd 2
//     （log 包在 init 时抓走 os.Stderr 的值，之后只会写自己的 writer，
//     所以必须显式 log.SetOutput 才能改变它写向哪里——这正是静音的关键一行）
//
// 之所以要模拟这些句柄：silenceLibraryOutput 的第一个动作就是保存当时的 os.Stdout/os.Stderr
// 作为 JSON 与诊断通道，测试必须让它们指向文件才能断言内容。
func capturedStreams(t *testing.T) (userStdout, userStderr, logSink *os.File, restore func()) {
	t.Helper()

	stdoutFile, err := os.CreateTemp(t.TempDir(), "user-stdout-*")
	require.NoError(t, err)
	stderrFile, err := os.CreateTemp(t.TempDir(), "user-stderr-*")
	require.NoError(t, err)
	logFile, err := os.CreateTemp(t.TempDir(), "process-stderr-*")
	require.NoError(t, err)

	originalStdout, originalStderr := os.Stdout, os.Stderr
	originalLogWriter := log.Writer()
	originalSlog := slog.Default()

	os.Stdout, os.Stderr = stdoutFile, stderrFile
	log.SetOutput(logFile)

	restore = func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		log.SetOutput(originalLogWriter)
		slog.SetDefault(originalSlog)
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		_ = logFile.Close()
	}
	return stdoutFile, stderrFile, logFile, restore
}

func readFile(t *testing.T, file *os.File) string {
	t.Helper()
	content, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	return string(content)
}

// TestSilenceLibraryOutputDiscardsLibraryLogging 直接守住“库日志不出现在任何用户可见流上”：
// 三个实际被触发的出口（fmt.Printf → os.Stdout、标准库 log、默认 slog）都必须被丢弃，
// 而本工具自己的诊断仍要出现在用户的 stderr 上。
func TestSilenceLibraryOutputDiscardsLibraryLogging(t *testing.T) {
	userStdout, userStderr, logSink, restore := capturedStreams(t)
	defer restore()

	stdout, stderr, devNull, err := silenceLibraryOutput()
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	// 模拟被复用代码的三个出口，内容全部是“绝不允许外泄”的文本。
	fmt.Printf("[AntigravityTokenRefresher] Account 63 needs refresh ya29.printed-token\n")
	fmt.Fprintf(os.Stderr, "[AntigravityOAuth] token refreshed refresh_token=1//printed-refresh\n")
	log.Printf("[antigravity] fetchAvailableModels 失败 (HTTP 403): {\"email\":\"ops@example.com\"}")
	slog.Warn("oauth_refresh_update_failed", "dsn", "postgres://sub2api:sup3r-secret@db.internal/sub2api")

	// 本工具自己的输出：JSON 走原 stdout，有限 stage 诊断走原 stderr。
	require.NoError(t, writeReport(stdout, &report{
		GeneratedAt: "2026-10-11T09:30:00Z",
		Source:      reportSource,
		Accounts:    []accountReport{{AccountID: 63, ModelIDs: []string{}, Models: []modelCapability{}}},
	}))
	fmt.Fprintln(stderr, describeAccountFailure(64, newReportError(stageFetchForbidden)))

	jsonText := readFile(t, userStdout)
	stderrText := readFile(t, userStderr)
	logSinkText := readFile(t, logSink)

	forbidden := []string{
		"ya29.printed-token", "1//printed-refresh", "ops@example.com", "sup3r-secret",
		"db.internal", "AntigravityTokenRefresher", "AntigravityOAuth", "[antigravity]",
		"oauth_refresh_update_failed", "needs refresh", "URL fallback",
	}
	for _, text := range []string{jsonText, stderrText} {
		for _, needle := range forbidden {
			require.NotContains(t, text, needle)
		}
	}
	require.Empty(t, strings.TrimSpace(logSinkText), "log 默认 writer（进程 fd 2）必须完全静音")

	var decoded report
	require.NoError(t, json.Unmarshal([]byte(jsonText), &decoded))
	require.Equal(t, int64(63), decoded.Accounts[0].AccountID)
	require.Equal(t, 1, strings.Count(jsonText, "{\n  \"generated_at\""), "stdout 只能有一个 JSON 文档")

	require.Equal(t, "catalog-inspect: account 64 failed stage=fetch_forbidden\n", stderrText)
}

// TestSilenceLibraryOutputCoversRealFetchPath 用真实 antigravity.Client 触发上游 URL 降级：
// 客户端在降级时会 log.Printf，这条真实日志必须被静音掉。
func TestSilenceLibraryOutputCoversRealFetchPath(t *testing.T) {
	var rejectedCalls int32
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&rejectedCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer rejecting.Close()
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-3-pro":{}}}`))
	}))
	defer serving.Close()

	restoreBaseURLs := withBaseURLs(t, []string{rejecting.URL, serving.URL})
	defer restoreBaseURLs()

	userStdout, userStderr, logSink, restoreStreams := capturedStreams(t)
	defer restoreStreams()

	stdout, stderr, devNull, err := silenceLibraryOutput()
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	client, err := antigravity.NewClient("")
	require.NoError(t, err)
	in := newTestInspector(&fakeTokenSource{token: "ya29.test-access-token"}, client, 8<<20)

	out := in.inspect(t.Context(), testAccount(63))
	require.Nil(t, out.Error)
	require.Equal(t, serving.URL, out.Endpoint)
	require.Equal(t, int32(1), atomic.LoadInt32(&rejectedCalls), "降级确实发生过，客户端必然打过日志")

	require.NoError(t, writeReport(stdout, &report{
		GeneratedAt: "2026-10-11T09:30:00Z",
		Source:      reportSource,
		Accounts:    []accountReport{out},
	}))
	fmt.Fprintln(stderr, describeAccountFailure(63, nil))

	jsonText := readFile(t, userStdout)
	stderrText := readFile(t, userStderr)
	require.NotContains(t, jsonText, "URL fallback")
	require.NotContains(t, stderrText, "URL fallback")
	require.NotContains(t, jsonText, "[antigravity]")
	require.NotContains(t, stderrText, "[antigravity]")
	// 客户端用的是标准库 log（写向进程 fd 2），必须被 log.SetOutput(devNull) 吃掉。
	require.Empty(t, strings.TrimSpace(readFile(t, logSink)), "真实降级日志必须被丢弃")
	require.Equal(t, "catalog-inspect: account 63 failed stage=unknown_failure\n", stderrText)
}

func TestRunFailsClosedOnAccountIDs(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		stage string
	}{
		{name: "empty list", raw: "", stage: stageAccountListEmpty},
		{name: "only separators", raw: ",,", stage: stageAccountListEmpty},
		{name: "zero", raw: "0", stage: stageAccountIDInvalid},
		{name: "negative", raw: "-1", stage: stageAccountIDInvalid},
		{name: "not numeric", raw: "63,abc", stage: stageAccountIDInvalid},
		{name: "outside fixed set", raw: "70", stage: stageAccountIDOutsideSet},
		{name: "mixed inside and outside", raw: "63,64,69,70", stage: stageAccountIDOutsideSet},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"-accounts", tt.raw}, &stdout, &stderr)

			require.Equal(t, exitFatal, code)
			require.Empty(t, stdout.String(), "参数错误时不得输出任何 JSON，也不得触碰配置/数据库")
			require.Equal(t, "catalog-inspect: fatal: "+tt.stage+"\n", stderr.String())
		})
	}
}

func TestRunHelpDocumentsWriteSideEffects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-h"}, &stdout, &stderr)

	require.Equal(t, exitOK, code)
	require.Empty(t, stdout.String(), "用法说明只允许出现在 stderr")
	require.Contains(t, stderr.String(), "写副作用")
	require.Contains(t, stderr.String(), "accounts.credentials")
	require.Contains(t, stderr.String(), "退出码")
	require.Contains(t, stderr.String(), fixedAccountSetDescription(), "用法说明必须与固定账号集合保持一致")
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-nope"}, &stdout, &stderr)

	require.Equal(t, exitFatal, code)
	require.Empty(t, stdout.String())
	require.NotEmpty(t, stderr.String())
}

// TestRunDoesNotPrintRawConfigError 守住“不打印原始错误”：
// 配置加载失败的真实原因里带着配置文件绝对路径，stdout/stderr 都不允许出现。
func TestRunDoesNotPrintRawConfigError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-config.yaml")

	var stdout, stderr bytes.Buffer
	code := run([]string{"-config", missing}, &stdout, &stderr)

	require.Equal(t, exitFatal, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "catalog-inspect: fatal: "+stageConfigLoadFailed+"\n", stderr.String())
	require.NotContains(t, stderr.String(), missing)
	require.NotContains(t, stderr.String(), "open ")
	require.NotContains(t, stderr.String(), "config.yaml")
}

func TestWriteReportKeepsErrorsFinite(t *testing.T) {
	in := newTestInspector(&fakeTokenSource{token: "ya29.report-access-token"}, &fakeFetcher{
		err: fmt.Errorf("fetchAvailableModels 失败 (HTTP 503): %s", rawUpstreamError),
	}, 8<<20)

	var buffer bytes.Buffer
	require.NoError(t, writeReport(&buffer, &report{
		GeneratedAt: "2026-10-11T09:30:00Z",
		Source:      reportSource,
		Accounts:    []accountReport{in.inspect(t.Context(), testAccount(63))},
	}))

	text := buffer.String()
	for _, forbidden := range []string{
		"ya29", "1//", "proj-test-123", "ops@example.com", "sup3r-secret",
		"postgres://", "db.internal", "PERMISSION_DENIED", "invalid_grant",
		"access_token", "refresh_token", "Authorization", "Bearer", "dial tcp",
	} {
		require.NotContains(t, text, forbidden)
	}

	var decoded report
	require.NoError(t, json.Unmarshal(buffer.Bytes(), &decoded))
	require.Len(t, decoded.Accounts, 1)
	require.NotNil(t, decoded.Accounts[0].Error)
	require.Equal(t, stageFetchFailed, decoded.Accounts[0].Error.Stage)
	require.NotNil(t, decoded.Accounts[0].Error.HTTPStatus)
	require.Equal(t, 503, *decoded.Accounts[0].Error.HTTPStatus)
	require.Equal(t, int64(63), decoded.Accounts[0].AccountID)
	require.NotEmpty(t, decoded.Accounts[0].ConfiguredEndpoints)
}

// TestSuccessReportHasNullError 固定 JSON 形状：成功账号的 error 必须是 null 而不是缺失。
func TestSuccessReportHasNullError(t *testing.T) {
	in := newTestInspector(&fakeTokenSource{token: "ya29.test-access-token"}, &fakeFetcher{
		resp: &antigravity.FetchAvailableModelsResponse{
			QuotaBaseURL: "https://cloudcode-pa.googleapis.com",
			Models:       map[string]antigravity.ModelInfo{"gemini-3-pro": {}},
		},
	}, 8<<20)

	var buffer bytes.Buffer
	require.NoError(t, writeReport(&buffer, &report{
		GeneratedAt: "2026-10-11T09:30:00Z",
		Source:      reportSource,
		Accounts:    []accountReport{in.inspect(t.Context(), testAccount(69))},
	}))

	require.Contains(t, buffer.String(), `"error": null`)
	require.Contains(t, buffer.String(), `"account_id": 69`)
}
