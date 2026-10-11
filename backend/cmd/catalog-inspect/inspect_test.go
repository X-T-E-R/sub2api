package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// ── 测试替身 ─────────────────────────────────────────────────────────

type fakeTokenSource struct {
	token    string
	snapshot *service.Account
	err      error
	calls    int
}

func (f *fakeTokenSource) GetAccessTokenForQuota(_ context.Context, account *service.Account) (string, *service.Account, error) {
	f.calls++
	if f.err != nil {
		return "", nil, f.err
	}
	if f.snapshot != nil {
		return f.token, f.snapshot, nil
	}
	return f.token, account, nil
}

type fakeFetcher struct {
	resp      *antigravity.FetchAvailableModelsResponse
	err       error
	calls     int
	token     string
	project   string
	bodyLimit int64
}

func (f *fakeFetcher) FetchAvailableModels(_ context.Context, accessToken, projectID string, bodyLimit int64) (*antigravity.FetchAvailableModelsResponse, map[string]any, error) {
	f.calls++
	f.token = accessToken
	f.project = projectID
	f.bodyLimit = bodyLimit
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.resp, nil, nil
}

func testAccount(id int64) *service.Account {
	return &service.Account{
		ID:       id,
		Name:     "antigravity-oauth",
		Platform: service.PlatformAntigravity,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "ya29.test-access-token",
			"refresh_token": "1//test-refresh-token",
			"project_id":    "proj-test-123",
			"email":         "ops@example.com",
		},
	}
}

func newTestInspector(tokens tokenSource, fetcher modelFetcher, bodyLimit int64) *inspector {
	return &inspector{
		tokens: tokens,
		newFetcher: func(*service.Account) (modelFetcher, error) {
			return fetcher, nil
		},
		bodyLimit: bodyLimit,
		now:       func() time.Time { return time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC) },
	}
}

func withBaseURLs(t *testing.T, urls []string) func() {
	t.Helper()
	original := antigravity.BaseURLs
	antigravity.BaseURLs = urls
	return func() { antigravity.BaseURLs = original }
}

func marshalAccount(t *testing.T, out accountReport) string {
	t.Helper()
	raw, err := json.Marshal(report{GeneratedAt: out.FetchedAt, Source: reportSource, Accounts: []accountReport{out}})
	require.NoError(t, err)
	return string(raw)
}

// ptr 保留「上游给了指针字段」与「上游省略该字段」的区别。
func ptr[T any](value T) *T { return &value }

func mustErr(_ []int64, err error) error { return err }

// TestProjectCatalogIsAlwaysSorted 用大量新 map 迭代压住 Go map 遍历顺序随机化，
// 让「模型 ID 必须排序」这条契约在缺少排序时稳定失败，而不是靠运气。
func TestProjectCatalogIsAlwaysSorted(t *testing.T) {
	for iteration := 0; iteration < 128; iteration++ {
		resp := &antigravity.FetchAvailableModelsResponse{Models: map[string]antigravity.ModelInfo{
			"gemini-3-pro":      {},
			"gemini-2.5-flash":  {},
			"claude-sonnet-4-5": {},
			"gemini-2.5-pro":    {},
			"claude-opus-4-5":   {},
		}}

		ids, capabilities := projectCatalog(resp)

		require.True(t, sort.StringsAreSorted(ids), "iteration %d not sorted: %v", iteration, ids)
		require.Len(t, capabilities, len(ids))
		for index, capability := range capabilities {
			require.Equal(t, ids[index], capability.ID)
		}
	}
}

// ── 能力投影 ─────────────────────────────────────────────────────────

func TestInspectProjectsSortedCapabilitiesWithoutSecrets(t *testing.T) {
	fetcher := &fakeFetcher{resp: &antigravity.FetchAvailableModelsResponse{
		QuotaBaseURL: "https://cloudcode-pa.googleapis.com",
		Models: map[string]antigravity.ModelInfo{
			"gemini-3-pro": {
				DisplayName:        "Gemini 3 Pro",
				SupportsImages:     ptr(true),
				SupportsThinking:   ptr(true),
				ThinkingBudget:     ptr(32768),
				Recommended:        ptr(true),
				MaxTokens:          ptr(1048576),
				MaxOutputTokens:    ptr(65536),
				SupportedMimeTypes: map[string]bool{"text/plain": true, "image/png": true, "audio/wav": false},
				QuotaInfo: &antigravity.ModelQuotaInfo{
					RemainingFraction: ptr(0.85),
					ResetTime:         "2026-10-11T10:00:00Z",
				},
			},
			"claude-sonnet-4-5": {DisplayName: "Claude Sonnet 4.5", SupportsThinking: ptr(false)},
			"gemini-2.5-flash":  {},
		},
		DeprecatedModelIDs: map[string]antigravity.DeprecatedModelInfo{
			"gemini-2.0-flash": {NewModelID: "gemini-2.5-flash"},
		},
	}}
	tokens := &fakeTokenSource{token: "ya29.test-access-token"}
	in := newTestInspector(tokens, fetcher, resolveModelsBodyLimit(nil))

	out := in.inspect(context.Background(), testAccount(63))

	require.Nil(t, out.Error)
	require.Equal(t, int64(63), out.AccountID)
	require.Equal(t, "2026-10-11T09:30:00Z", out.FetchedAt)
	require.Equal(t, "https://cloudcode-pa.googleapis.com", out.Endpoint)
	require.Equal(t, append([]string(nil), antigravity.BaseURLs...), out.ConfiguredEndpoints)
	require.Equal(t, []string{"claude-sonnet-4-5", "gemini-2.5-flash", "gemini-3-pro"}, out.ModelIDs)
	require.Equal(t, 3, out.ModelCount)

	// 每账号只允许一次逻辑取数，且上游请求带上快照账号的 project 与配置的读取上限。
	require.Equal(t, 1, tokens.calls)
	require.Equal(t, 1, fetcher.calls)
	require.Equal(t, "ya29.test-access-token", fetcher.token)
	require.Equal(t, "proj-test-123", fetcher.project)
	require.Equal(t, config.DefaultModelsListReadMaxBytes, fetcher.bodyLimit)

	pro := out.Models[2]
	require.Equal(t, "gemini-3-pro", pro.ID)
	require.Equal(t, "Gemini 3 Pro", pro.DisplayName)
	require.True(t, *pro.SupportsImages)
	require.True(t, *pro.SupportsThinking)
	require.Equal(t, 32768, *pro.ThinkingBudget)
	require.True(t, *pro.Recommended)
	require.Equal(t, 1048576, *pro.MaxTokens)
	require.Equal(t, 65536, *pro.MaxOutputTokens)
	require.Equal(t, []string{"image/png", "text/plain"}, pro.SupportedMimeTypes)
	require.Equal(t, 0.85, *pro.QuotaRemainingFraction)
	require.Equal(t, "2026-10-11T10:00:00Z", pro.QuotaResetTime)
	require.Equal(t, map[string]string{"gemini-2.0-flash": "gemini-2.5-flash"}, out.DeprecatedModelIDs)

	// 上游未给的字段保持缺失，显式 false 保留为 false。
	require.Nil(t, out.Models[0].SupportsImages)
	require.False(t, *out.Models[0].SupportsThinking)
	require.Nil(t, out.Models[1].SupportsImages)
	require.Nil(t, out.Models[1].SupportsThinking)
	require.Nil(t, out.Models[1].QuotaRemainingFraction)
	require.Empty(t, out.Models[1].SupportedMimeTypes)

	raw := marshalAccount(t, out)
	for _, forbidden := range []string{
		"ya29.", "1//test-refresh-token", "proj-test-123", "ops@example.com",
		"access_token", "refresh_token", "quotaInfo", "supportedMimeTypes",
	} {
		require.NotContains(t, raw, forbidden)
	}
}

func TestInspectUsesSnapshotProjectID(t *testing.T) {
	snapshot := testAccount(63)
	snapshot.Credentials["project_id"] = "proj-from-snapshot"
	snapshot.Credentials["access_token"] = "ya29.snapshot-token"
	fetcher := &fakeFetcher{resp: &antigravity.FetchAvailableModelsResponse{
		QuotaBaseURL: "https://cloudcode-pa.googleapis.com",
		Models:       map[string]antigravity.ModelInfo{"gemini-2.5-flash": {}},
	}}
	in := newTestInspector(&fakeTokenSource{token: "ya29.snapshot-token", snapshot: snapshot}, fetcher, 8<<20)

	out := in.inspect(context.Background(), testAccount(63))

	require.Nil(t, out.Error)
	require.Equal(t, "proj-from-snapshot", fetcher.project)
	require.Equal(t, "ya29.snapshot-token", fetcher.token)
}

func TestInspectTreatsEmptyCatalogAsObservation(t *testing.T) {
	fetcher := &fakeFetcher{resp: &antigravity.FetchAvailableModelsResponse{
		QuotaBaseURL: "https://cloudcode-pa.googleapis.com",
		Models:       map[string]antigravity.ModelInfo{},
	}}
	in := newTestInspector(&fakeTokenSource{token: "ya29.test-access-token"}, fetcher, 8<<20)

	out := in.inspect(context.Background(), testAccount(63))

	require.Nil(t, out.Error, "上游返回空目录是观察结果，不是工具失败")
	require.Equal(t, 0, out.ModelCount)
	require.NotNil(t, out.ModelIDs)
	require.Empty(t, out.ModelIDs)
	require.Equal(t, 1, fetcher.calls)
}

// ── 真实客户端 + 本地上游 ────────────────────────────────────────────

func TestInspectReportsActualEndpointAfterURLFallback(t *testing.T) {
	var rejectedCalls, servedCalls int32
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&rejectedCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	}))
	defer rejecting.Close()

	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&servedCalls, 1)
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1internal:fetchAvailableModels") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-2.5-flash":{},"gemini-3-pro":{"displayName":"Gemini 3 Pro"}}}`))
	}))
	defer serving.Close()

	restore := withBaseURLs(t, []string{rejecting.URL, serving.URL})
	defer restore()

	client, err := antigravity.NewClient("")
	require.NoError(t, err)

	in := newTestInspector(&fakeTokenSource{token: "ya29.test-access-token"}, client, 8<<20)
	out := in.inspect(context.Background(), testAccount(63))

	require.Nil(t, out.Error)
	require.Equal(t, serving.URL, out.Endpoint, "endpoint 必须是客户端实际使用的基础 URL")
	require.Equal(t, []string{rejecting.URL, serving.URL}, out.ConfiguredEndpoints)
	require.Equal(t, []string{"gemini-2.5-flash", "gemini-3-pro"}, out.ModelIDs)
	// 一次逻辑调用允许客户端按配置顺序降级，因此被拒端点恰好收到一次请求。
	require.Equal(t, int32(1), atomic.LoadInt32(&rejectedCalls))
	require.Equal(t, int32(1), atomic.LoadInt32(&servedCalls))
}

func TestInspectReportsFiniteStageForUpstreamFailure(t *testing.T) {
	leaking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"PERMISSION_DENIED"},` +
			`"access_token":"ya29.leaked-access-token","refresh_token":"1//leaked-refresh-token",` +
			`"project":"proj-test-123","email":"ops@example.com",` +
			`"dsn":"postgres://sub2api:sup3r-secret@db.internal:5432/sub2api"}`))
	}))
	defer leaking.Close()

	restore := withBaseURLs(t, []string{leaking.URL})
	defer restore()

	client, err := antigravity.NewClient("")
	require.NoError(t, err)

	in := newTestInspector(&fakeTokenSource{token: "ya29.test-access-token"}, client, 8<<20)
	out := in.inspect(context.Background(), testAccount(63))

	require.NotNil(t, out.Error)
	require.Equal(t, stageFetchForbidden, out.Error.Stage)
	require.NotNil(t, out.Error.HTTPStatus)
	require.Equal(t, 403, *out.Error.HTTPStatus)
	require.Equal(t, "", out.Endpoint, "失败时客户端不暴露最后尝试的 URL")
	require.Equal(t, []string{leaking.URL}, out.ConfiguredEndpoints)

	raw := marshalAccount(t, out)
	for _, forbidden := range []string{
		"ya29.", "1//", "proj-test-123", "ops@example.com", "sup3r-secret",
		"postgres://", "db.internal", "PERMISSION_DENIED", "invalid_grant",
		"access_token", "refresh_token", "Authorization", "Bearer",
	} {
		require.NotContains(t, raw, forbidden)
	}
	require.Contains(t, raw, stageFetchForbidden)
}

// ── 前置拒绝与失败路径 ───────────────────────────────────────────────

func TestInspectRejectsUnsupportedAccountBeforeFetching(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*service.Account)
		stage  string
	}{
		{
			name:   "not antigravity",
			mutate: func(a *service.Account) { a.Platform = "gemini" },
			stage:  stageNotAntigravity,
		},
		{
			name:   "not oauth",
			mutate: func(a *service.Account) { a.Type = service.AccountTypeAPIKey },
			stage:  stageNotOAuth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := testAccount(63)
			tt.mutate(account)
			tokens := &fakeTokenSource{token: "ya29.test-access-token"}
			fetcher := &fakeFetcher{}
			in := newTestInspector(tokens, fetcher, 8<<20)

			out := in.inspect(context.Background(), account)

			require.NotNil(t, out.Error)
			require.Equal(t, tt.stage, out.Error.Stage)
			require.Equal(t, 0, tokens.calls)
			require.Equal(t, 0, fetcher.calls)
		})
	}
}

func TestInspectReportsFiniteStageForTokenFailure(t *testing.T) {
	rejected := errors.New(`token 刷新失败 (HTTP 400): {"error":"invalid_grant","refresh_token":"1//partial-refresh-token"}` +
		` account=63 access_token=ya29.test-access-token project_id=proj-test-123`)
	in := newTestInspector(&fakeTokenSource{err: rejected}, &fakeFetcher{}, 8<<20)

	out := in.inspect(context.Background(), testAccount(63))

	require.NotNil(t, out.Error)
	require.Equal(t, stageTokenUnavailable, out.Error.Stage)
	require.NotNil(t, out.Error.HTTPStatus)
	require.Equal(t, 400, *out.Error.HTTPStatus)

	raw := marshalAccount(t, out)
	for _, forbidden := range []string{
		"ya29.", "1//", "proj-test-123", "ops@example.com", "invalid_grant", "access_token", "refresh_token",
	} {
		require.NotContains(t, raw, forbidden)
	}
}

func TestInspectRejectsEmptyTokenAndNilSnapshot(t *testing.T) {
	emptyToken := newTestInspector(&fakeTokenSource{token: "   "}, &fakeFetcher{}, 8<<20)
	out := emptyToken.inspect(context.Background(), testAccount(63))
	require.NotNil(t, out.Error)
	require.Equal(t, stageTokenMissing, out.Error.Stage)

	nilSnapshot := newTestInspector(fixedSnapshotTokenSource{}, &fakeFetcher{}, 8<<20)
	out = nilSnapshot.inspect(context.Background(), testAccount(63))
	require.NotNil(t, out.Error)
	require.Equal(t, stageTokenMissing, out.Error.Stage)
}

func TestInspectReportsFiniteStageForNilResponse(t *testing.T) {
	in := &inspector{
		tokens: &fakeTokenSource{token: "ya29.test-access-token"},
		newFetcher: func(*service.Account) (modelFetcher, error) {
			return nilResponseFetcher{}, nil
		},
		bodyLimit: 8 << 20,
	}
	out := in.inspect(context.Background(), testAccount(63))
	require.NotNil(t, out.Error)
	require.Equal(t, stageEmptyResponse, out.Error.Stage)
}

func TestInspectReportsClientSetupFailureAsFiniteStage(t *testing.T) {
	in := &inspector{
		tokens: &fakeTokenSource{token: "ya29.test-access-token"},
		newFetcher: func(*service.Account) (modelFetcher, error) {
			return nil, errors.New("parse proxy: invalid proxy url http://user:secret@proxy:8080")
		},
		bodyLimit: 8 << 20,
	}
	out := in.inspect(context.Background(), testAccount(63))
	require.NotNil(t, out.Error)
	require.Equal(t, stageClientSetupFailed, out.Error.Stage)
	require.NotContains(t, marshalAccount(t, out), "secret@proxy")
}

// ── 账号集合：失败关闭 ───────────────────────────────────────────────

func TestResolveFixedAccountIDsFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []int64
		err  error
	}{
		{name: "default full set", raw: "63,64,69", want: []int64{63, 64, 69}},
		{name: "subset single", raw: "64", want: []int64{64}},
		{name: "spaces and empty fields", raw: " 69 ,, 63 ", want: []int64{63, 69}},
		{name: "canonical order regardless of input order", raw: "69,63,64", want: []int64{63, 64, 69}},
		{name: "duplicates collapse", raw: "69,69,63", want: []int64{63, 69}},
		{name: "empty", raw: "  ", err: newStageError(stageAccountListEmpty)},
		{name: "zero", raw: "0", err: newStageError(stageAccountIDInvalid)},
		{name: "negative", raw: "-4", err: newStageError(stageAccountIDInvalid)},
		{name: "not numeric", raw: "63,abc", err: newStageError(stageAccountIDInvalid)},
		{name: "outside fixed set", raw: "70", err: newStageError(stageAccountIDOutsideSet)},
		{name: "one outside fails whole request", raw: "63,62", err: newStageError(stageAccountIDOutsideSet)},
		{name: "arbitrary ids", raw: "1,2,3", err: newStageError(stageAccountIDOutsideSet)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFixedAccountIDs(tt.raw)
			if tt.err != nil {
				require.Error(t, err)
				require.Equal(t, stageOf(tt.err), stageOf(err), "失败必须是有限 stage，且不泄露输入")
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseAccountIDListRejectsInvalidInput(t *testing.T) {
	require.Equal(t, stageAccountIDInvalid, stageOf(mustErr(parseAccountIDList("abc"))))
	require.Equal(t, stageAccountListEmpty, stageOf(mustErr(parseAccountIDList(","))))

	ids, err := parseAccountIDList(" 70 ")
	require.NoError(t, err)
	require.Equal(t, []int64{70}, ids, "低层解析不负责集合校验")
}

// ── 参数与辅助逻辑 ───────────────────────────────────────────────────

func TestResolveModelsBodyLimit(t *testing.T) {
	require.Equal(t, config.DefaultModelsListReadMaxBytes, resolveModelsBodyLimit(nil))
	require.Equal(t, config.DefaultModelsListReadMaxBytes, resolveModelsBodyLimit(&config.Config{}))

	cfg := &config.Config{}
	cfg.Gateway.ModelsListReadMaxBytes = 16 << 20
	require.Equal(t, int64(16<<20), resolveModelsBodyLimit(cfg))
}

func TestFirstAccountError(t *testing.T) {
	failed := &report{Accounts: []accountReport{
		{AccountID: 63},
		{AccountID: 64, Error: newReportError(stageFetchFailed)},
		{AccountID: 69, Error: newReportError(stageTokenMissing)},
	}}
	account, ok := firstAccountError(failed)
	require.True(t, ok)
	require.Equal(t, int64(64), account.AccountID)
	require.Equal(t, stageFetchFailed, account.Error.Stage)

	_, ok = firstAccountError(&report{Accounts: []accountReport{{AccountID: 63}}})
	require.False(t, ok)
}

func TestFixedAccountSetDescription(t *testing.T) {
	require.Equal(t, "{63,64,69}", fixedAccountSetDescription())
	require.Equal(t, "63,64,69", fixedAccountIDsList())
}

// ── 测试用桩 ─────────────────────────────────────────────────────────

type fixedSnapshotTokenSource struct{}

func (fixedSnapshotTokenSource) GetAccessTokenForQuota(context.Context, *service.Account) (string, *service.Account, error) {
	return "ya29.test-access-token", nil, nil
}

type nilResponseFetcher struct{}

func (nilResponseFetcher) FetchAvailableModels(context.Context, string, string, int64) (*antigravity.FetchAvailableModelsResponse, map[string]any, error) {
	return nil, nil, nil
}
