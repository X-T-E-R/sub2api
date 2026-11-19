package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Real GatewayHandler tests for Antigravity thought-only / zero-output /
// missing-terminal continuation and failure accounting.
// Every case drives the actual h.Messages or h.ChatCompletions handler (never
// a synthetic seam stub) and asserts concurrency slot acquire/release balance,
// upstream call counts (no handler-level failover retry), usageLogRepo.Create
// billing accuracy, and exact downstream wire format.

const handlerThoughtGenerationOneLine = `data: {"response":{"responseId":"resp_gen1","candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`

type handlerThoughtUpstream struct {
	responses []*http.Response
	errors    []error
	calls     int
}

func (s *handlerThoughtUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	index := s.calls
	s.calls++
	var resp *http.Response
	if index < len(s.responses) {
		resp = s.responses[index]
	}
	var err error
	if index < len(s.errors) {
		err = s.errors[index]
	}
	if resp == nil && err == nil {
		return nil, errors.New("unexpected upstream call")
	}
	return resp, err
}

func (s *handlerThoughtUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

type handlerThoughtTokenCache struct{}

func (handlerThoughtTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return "fresh-oauth-token", nil
}

func (handlerThoughtTokenCache) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}

func (handlerThoughtTokenCache) DeleteAccessToken(context.Context, string) error { return nil }

func (handlerThoughtTokenCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

func (handlerThoughtTokenCache) ReleaseRefreshLock(context.Context, string) error { return nil }

type handlerThoughtSettingRepo struct{}

func (handlerThoughtSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}

func (handlerThoughtSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func (handlerThoughtSettingRepo) Set(context.Context, string, string) error { return nil }

func (handlerThoughtSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (handlerThoughtSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }

func (handlerThoughtSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (handlerThoughtSettingRepo) Delete(context.Context, string) error { return nil }

func handlerThoughtStream(lines ...string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"request-3757"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))),
	}
}

func handlerThoughtAccount() *service.Account {
	return &service.Account{
		ID:          3757,
		Name:        "antigravity-handler-thought",
		Platform:    service.PlatformAntigravity,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "stale-account-token",
			"project_id":   "project-3757",
			"model_mapping": map[string]any{
				"client-alias": "gemini-3.8-flash",
			},
		},
		Extra: map[string]any{
			service.AntigravitySuppressThoughtOnlyStopExtraKey: false,
			service.AntigravityContinueThoughtOnlyExtraKey:     true,
		},
	}
}

func TestGatewayMessagesAntigravityThoughtAppendedFailureBillsFirstGeneration(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		messages: true,
		stream:   true,
	})
}

func TestGatewayChatCompletionsAntigravityThoughtAppendedFailureKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{})
}

func TestGatewayChatCompletionsAntigravityZeroOutputContinueKeepsBillingAndSlots(t *testing.T) {
	enableAntigravityContinueZeroOutput(t)
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{empty: true, zeroOutput: true})
}

func TestGatewayMessagesAntigravityZeroOutputContinueKeepsBillingAndSlots(t *testing.T) {
	enableAntigravityContinueZeroOutput(t)
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		messages: true, empty: true, zeroOutput: true, expectFailure: true,
		generation:       `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":380743,"cachedContentTokenCount":378694,"candidatesTokenCount":0}}}`,
		secondGeneration: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}}`,
		wantCode:         "antigravity_empty_response", wantStatus: http.StatusBadGateway,
		wantInput: 2049, wantCache: 378694, wantOutput: 0,
	})
}

func TestGatewayMessagesAntigravityZeroOutputUsageKeepsBillingAndSlots(t *testing.T) {
	enableAntigravityContinueZeroOutput(t)
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		messages: true, empty: true, zeroOutput: true, expectFailure: true,
		generation:       `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":380743,"cachedContentTokenCount":378694,"candidatesTokenCount":0}}}`,
		secondGeneration: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":380744,"cachedContentTokenCount":378695,"candidatesTokenCount":0}}}`,
		wantCode:         "antigravity_empty_response", wantStatus: http.StatusBadGateway,
		wantInput: 4098, wantCache: 757389, wantOutput: 0,
	})
}

func enableAntigravityContinueZeroOutput(t *testing.T) {
	t.Helper()
	settings := service.NewSettingService(handlerThoughtSettingRepo{}, nil)
	previous := service.ResolveAntigravityThoughtPolicy(context.Background(), nil)
	t.Cleanup(func() {
		require.NoError(t, settings.SetAntigravityThoughtPolicySettings(context.Background(), service.AntigravityThoughtPolicySettings(previous)))
	})
	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(context.Background(), service.AntigravityThoughtPolicySettings{ContinueZeroOutput: true}))
}

func TestGatewayMessagesAntigravityMissingTerminalReplayKeepsBillingAndSlots(t *testing.T) {
	for _, geminiGroup := range []bool{false, true} {
		runGatewayThoughtPartialCase(t, gatewayThoughtCase{
			noAnswer: true, empty: true, messages: true, missingReplay: true, geminiGroup: geminiGroup,
			generation:       `data: {"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":2}}}`,
			secondGeneration: `data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"sig-empty"}]},"finishReason":"FINISH_REASON_UNSPECIFIED"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":2}}}`,
			wantCode:         "antigravity_missing_terminal", wantStatus: http.StatusBadGateway,
			wantInput: 16, wantCache: 4,
		})
	}
}

func TestGatewayChatCompletionsAntigravityMissingTerminalReplayTransportFailureKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		noAnswer: true, empty: true, missingReplay: true,
		generation: `data: {"response":{"candidates":[{"content":{"parts":[]}}],"usageMetadata":{"promptTokenCount":8}}}`,
		wantCode:   "thought_continuation_error", wantStatus: http.StatusBadGateway,
	})
}

func TestGatewayChatCompletionsAntigravityNoUsableAnswerKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{noAnswer: true})
}

func TestGatewayChatCompletionsAntigravityEmptyStopKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{noAnswer: true, empty: true})
}

// A provider terminal after delivered text, and a prompt-level block, both end
// through the same real handler branch: partial content survives, the request is
// billed once, slots are released once, and exactly one failure reaches the client.
func TestGatewayChatCompletionsAntigravityProviderFailureKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		noAnswer:   true,
		generation: `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial answer"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`,
		wantCode:   "antigravity_malformed_function_call",
		wantOutput: 5,
		wantText:   "partial answer",
	})
	runGatewayThoughtPartialCase(t, gatewayThoughtCase{
		noAnswer:   true,
		generation: `data: {"response":{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":0,"thoughtsTokenCount":2}}}`,
		wantCode:   "antigravity_safety",
		wantStatus: http.StatusForbidden,
		wantOutput: 2,
	})
}

type gatewayThoughtCase struct {
	noAnswer         bool
	empty            bool
	zeroOutput       bool
	missingReplay    bool
	geminiGroup      bool
	messages         bool
	stream           bool
	expectFailure    bool
	generation       string
	secondGeneration string
	wantCode         string
	wantStatus       int
	wantInput        int
	wantCache        int
	wantOutput       int
	wantText         string
}

func runGatewayThoughtPartialCase(t *testing.T, tc gatewayThoughtCase) {
	t.Helper()
	noAnswer, empty := tc.noAnswer, tc.empty
	gin.SetMode(gin.TestMode)

	groupID := int64(9300)
	group := &service.Group{
		ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive,
	}
	if tc.geminiGroup {
		group.Platform = service.PlatformGemini
	}
	account := handlerThoughtAccount()
	if tc.geminiGroup {
		account.Extra["mixed_scheduling"] = true
	}
	if tc.zeroOutput {
		account.Extra[service.AntigravityContinueThoughtOnlyExtraKey] = false
	}
	if noAnswer {
		account.Extra[service.AntigravitySuppressThoughtOnlyStopExtraKey] = true
		account.Extra[service.AntigravityContinueThoughtOnlyExtraKey] = false
	}
	account.Status = service.StatusActive
	account.Schedulable = true
	account.Concurrency = 1
	account.AccountGroups = []service.AccountGroup{{AccountID: account.ID, GroupID: groupID}}

	generation := handlerThoughtGenerationOneLine
	wantOutput := 5
	if noAnswer && !empty {
		generation = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan","functionCall":null}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
	}
	if empty {
		generation = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":0}}}`
		wantOutput = 0
	}
	if tc.generation != "" {
		generation, wantOutput = tc.generation, tc.wantOutput
	}
	upstream := &handlerThoughtUpstream{
		responses: []*http.Response{handlerThoughtStream(generation)},
		errors:    []error{nil, errors.New("dial tcp: connection refused")},
	}
	if tc.secondGeneration != "" {
		upstream.responses = append(upstream.responses, handlerThoughtStream(tc.secondGeneration))
		upstream.errors = []error{nil, nil}
	}
	antigravityService := service.NewAntigravityGatewayService(
		nil,
		nil,
		nil,
		service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil),
		nil,
		upstream,
		service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20, AntigravityRetryMissingTerminal: tc.missingReplay}}),
		nil,
		nil, nil)

	schedulerCache := &fakeSchedulerCache{accounts: []*service.Account{account}}
	schedulerSnapshot := service.NewSchedulerSnapshotService(schedulerCache, nil, nil, nil, nil)
	usageLogRepo := &thoughtUsageLogRepoStub{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrencyCache := &countingThoughtConcurrencyCache{fakeConcurrencyCache: &fakeConcurrencyCache{}}
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)

	gatewayService := service.NewGatewayService(
		nil,
		&fakeGroupRepo{group: group},
		usageLogRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		schedulerSnapshot,
		service.NewConcurrencyService(concurrencyCache),
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheService,
		nil,
		nil,
		service.NewDeferredService(nil, nil, 0),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	h := &GatewayHandler{
		gatewayService:            gatewayService,
		antigravityGatewayService: antigravityService,
		billingCacheService:       billingCacheService,
		concurrencyHelper:         NewConcurrencyHelper(service.NewConcurrencyService(concurrencyCache), SSEPingFormatClaude, 0),
		maxAccountSwitches:        1,
		maxAccountSwitchesGemini:  1,
		cfg:                       cfg,
	}

	streamFlag := "false"
	if tc.stream {
		streamFlag = "true"
	}
	body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"}],"stream":` + streamFlag + `}`)
	path := "/v1/chat/completions"
	if tc.messages {
		body = []byte(`{"model":"client-alias","max_tokens":64,"messages":[{"role":"user","content":"question"}],"stream":` + streamFlag + `}`)
		path = "/v1/messages"
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	apiKey := &service.APIKey{
		ID: 9302, UserID: 9303, GroupID: &groupID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 9303, Concurrency: 10, Balance: 100},
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

	if tc.messages {
		h.Messages(c)
	} else {
		h.ChatCompletions(c)
	}

	wantStatus := http.StatusOK
	wantCalls := 2
	if noAnswer {
		wantStatus = http.StatusBadGateway
		wantCalls = 1
		if tc.missingReplay {
			wantCalls = 2
		}
	}
	if tc.wantStatus != 0 {
		wantStatus = tc.wantStatus
	}
	require.Equal(t, wantStatus, recorder.Code, "body=%s", recorder.Body.String())
	wire := recorder.Body.String()
	if tc.stream && tc.messages {
		require.Contains(t, wire, `"thinking":"plan"`, "第一代已收到的思考仍然交付下游")
		require.Contains(t, wire, `"stop_reason":null`)
		require.Contains(t, wire, `"input_tokens":8`, "下游能看到真实用量")
		require.Contains(t, wire, "thought_continuation_error", "失败以协议错误对外呈现")
		require.Equal(t, 1, strings.Count(wire, `event: error`), "只允许一个错误帧")
		require.NotContains(t, wire, `"type":"message_stop"`)
		require.NotContains(t, wire, `"stop_reason":"end_turn"`)
		require.Equal(t, 1, strings.Count(wire, "event: message_start"), "仍是一个合法的下游响应")
	} else {
		if tc.wantText != "" {
			require.Contains(t, wire, tc.wantText, "content the upstream already produced must reach the client")
		} else if tc.generation != "" {
			// Fixture-specific content is asserted by the caller.
		} else if !empty {
			require.Contains(t, wire, "plan", "第一代已收到的思考仍然交付下游")
		} else {
			require.NotContains(t, wire, "reasoning_content", "do not invent thinking in a truly empty result")
		}
		if tc.geminiGroup {
			require.Equal(t, "FINISH_REASON_UNSPECIFIED", gjson.Get(wire, "candidates.0.finishReason").String(), "Gemini group must retain a non-success terminal")
			require.NotContains(t, wire, `"finishReason":"STOP"`)
		} else if tc.messages {
			require.Contains(t, wire, `"stop_reason":null`, "非流式 Messages 终态必须保持非成功")
			require.NotContains(t, wire, `"stop_reason":"end_turn"`)
		} else {
			require.Contains(t, wire, `"finish_reason":null`, "追加生成失败不得伪装成成功完成")
			require.NotContains(t, wire, `"finish_reason":"stop"`)
		}
		expectsFailure := noAnswer || tc.expectFailure
		if expectsFailure {
			code := tc.wantCode
			if code == "" {
				code = "antigravity_thinking_only"
				if empty {
					code = "antigravity_empty_response"
				}
			}
			if tc.geminiGroup {
				require.True(t, json.Valid(recorder.Body.Bytes()), "Gemini error response must be one valid JSON document")
				require.Equal(t, code, gjson.Get(wire, "error.details.0.reason").String())
				require.Equal(t, 1, bytes.Count(recorder.Body.Bytes(), []byte(`"error":`)), "one protocol error, no generic duplicate")
				require.Equal(t, int64(http.StatusBadGateway), gjson.Get(wire, "error.code").Int())
				require.Equal(t, "UNAVAILABLE", gjson.Get(wire, "error.status").String())
				require.Equal(t, int64(tc.wantInput+tc.wantCache), gjson.Get(wire, "usageMetadata.promptTokenCount").Int())
				require.Equal(t, int64(tc.wantCache), gjson.Get(wire, "usageMetadata.cachedContentTokenCount").Int())
				require.Equal(t, int64(wantOutput), gjson.Get(wire, "usageMetadata.candidatesTokenCount").Int())
			} else {
				require.Contains(t, wire, `"code":"`+code+`"`)
				if tc.messages {
					require.True(t, json.Valid(recorder.Body.Bytes()), "Messages error response must be one valid JSON document")
					require.Equal(t, "error", gjson.Get(wire, "type").String())
					require.Equal(t, int64(tc.wantInput), gjson.Get(wire, "usage.input_tokens").Int())
					require.Equal(t, int64(wantOutput), gjson.Get(wire, "usage.output_tokens").Int())
					require.Equal(t, int64(tc.wantCache), gjson.Get(wire, "usage.cache_read_input_tokens").Int())
				} else {
					require.Equal(t, 1, bytes.Count(recorder.Body.Bytes(), []byte(`"error"`)), "one protocol error, no generic duplicate")
					require.Contains(t, wire, `"prompt_tokens":8`)
					require.Equal(t, int64(wantOutput), gjson.Get(wire, "usage.completion_tokens").Int())
				}
			}
		} else {
			require.NotContains(t, wire, "answer")
			require.NotContains(t, wire, `"error"`, "Task24 终态错误已被视为已告知，不再补写错误响应")
		}
	}

	require.LessOrEqual(t, upstream.calls, 2, "at most one replay beyond the initial upstream request")
	require.Equal(t, wantCalls, upstream.calls, "replay/failure path must not trigger handler retry or account failover")
	require.Equal(t, int64(1), concurrencyCache.userAcquires.Load(), "用户槽位只获取一次")
	require.Equal(t, int64(1), concurrencyCache.userReleases.Load(), "用户槽位必须释放一次")
	require.Equal(t, int64(1), concurrencyCache.accountAcquires.Load(), "账号槽位只获取一次")
	require.Equal(t, int64(1), concurrencyCache.accountReleases.Load(), "账号槽位必须释放一次，不能泄漏")

	require.Len(t, usageLogRepo.created, 1, "请求恰好入账一次")
	wantInput := tc.wantInput
	if wantInput == 0 {
		wantInput = 8
	}
	require.Equal(t, wantInput, usageLogRepo.created[0].InputTokens, "入账的是累计实测用量")
	require.Equal(t, wantOutput, usageLogRepo.created[0].OutputTokens)
	if tc.messages || tc.wantCache != 0 {
		require.Equal(t, tc.wantCache, usageLogRepo.created[0].CacheReadTokens)
	}
	require.Equal(t, account.ID, usageLogRepo.created[0].AccountID)
}

type thoughtUsageLogRepoStub struct {
	service.UsageLogRepository
	created []*service.UsageLog
}

func (s *thoughtUsageLogRepoStub) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	s.created = append(s.created, log)
	return true, nil
}

type countingThoughtConcurrencyCache struct {
	*fakeConcurrencyCache
	userAcquires    atomic.Int64
	userReleases    atomic.Int64
	accountAcquires atomic.Int64
	accountReleases atomic.Int64
}

func (c *countingThoughtConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	c.accountAcquires.Add(1)
	return true, nil
}

func (c *countingThoughtConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.accountReleases.Add(1)
	return nil
}

func (c *countingThoughtConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	c.userAcquires.Add(1)
	return true, nil
}

func (c *countingThoughtConcurrencyCache) ReleaseUserSlot(context.Context, int64, string) error {
	c.userReleases.Add(1)
	return nil
}
