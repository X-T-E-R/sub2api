//go:build unit

package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 真实 Handler 夹具：复用 gateway_handler_cancellation_test.go / warmup 单元的脚手架
// （fakeSchedulerCache / fakeGroupRepo / fakeConcurrencyCache + RunModeSimple + authctx），
// 端到端驱动 h.ChatCompletions：Antigravity 账号（A 关、B 开）+ 追加生成传输失败。
//
// 它证明的是 Handler 分支本身，而不是把 seam 当成分支证明：
//   - 账号槽位只获取/释放各一次，上游只收到第一代 + 一次追加生成 —— 若错误分支被当成 failover
//     错误（会 continue 到下一轮 Forward），这里会看到第二次账号槽位与第三次上游请求；
//   - usage 记录真的落到 usageLogRepo.Create（RunModeSimple），且记的是第一代的 8/5 —— 若删掉
//     Chat Completions 错误分支里的 submitUsage(result)，本断言必然失败；
//   - 下游拿到第一代的非成功终态（finish_reason:null）与思考内容，而不是成功终态，也没有第二个
//     错误响应体（gatewayForwardErrorAlreadyCommunicated 对 Task24 终态错误返回 true）。
func TestGatewayChatCompletionsAntigravityThoughtAppendedFailureKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialFailure(t, false, false)
}

func TestGatewayChatCompletionsAntigravityNoUsableAnswerKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialFailure(t, true, false)
}

func TestGatewayChatCompletionsAntigravityEmptyStopKeepsBillingAndSlots(t *testing.T) {
	runGatewayThoughtPartialFailure(t, true, true)
}

func runGatewayThoughtPartialFailure(t *testing.T, noAnswer, empty bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	groupID := int64(9300)
	group := &service.Group{
		ID: groupID, Hydrated: true, Platform: service.PlatformAntigravity, Status: service.StatusActive,
	}
	account := handlerThoughtAccount()
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
	if empty {
		generation = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":0}}}`
		wantOutput = 0
	}
	upstream := &handlerThoughtUpstream{
		responses: []*http.Response{handlerThoughtStream(generation)},
		errors:    []error{nil, errors.New("dial tcp: connection refused")},
	}
	antigravityService := service.NewAntigravityGatewayService(
		nil,
		nil,
		nil,
		service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil),
		nil,
		upstream,
		service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}),
		nil,
	)

	schedulerCache := &fakeSchedulerCache{accounts: []*service.Account{account}}
	schedulerSnapshot := service.NewSchedulerSnapshotService(schedulerCache, nil, nil, nil, nil)
	usageLogRepo := &thoughtUsageLogRepoStub{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	concurrencyCache := &countingThoughtConcurrencyCache{fakeConcurrencyCache: &fakeConcurrencyCache{}}
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)

	gatewayService := service.NewGatewayService(
		nil,                          // accountRepo
		&fakeGroupRepo{group: group}, // groupRepo
		usageLogRepo,                 // usageLogRepo：断言用量记录落地
		nil,                          // usageBillingRepo
		nil,                          // userRepo
		nil,                          // userSubRepo
		nil,                          // userGroupRateRepo
		nil,                          // cache（关闭粘性会话）
		cfg,                          // RunModeSimple：只写用量日志，不进真实扣费链路
		schedulerSnapshot,            // schedulerSnapshot
		service.NewConcurrencyService(concurrencyCache), // concurrencyService：账号槽位走可计数缓存
		service.NewBillingService(cfg, nil),             // billingService
		nil,                                             // rateLimitService
		billingCacheService,                             // billingCacheService
		nil,                                             // identityService
		nil,                                             // httpUpstream（Antigravity 走自己的服务）
		service.NewDeferredService(nil, nil, 0),         // deferredService：RecordUsage 会更新 last_used
		nil,                                             // claudeTokenProvider
		nil,                                             // sessionLimitCache
		nil,                                             // rpmCache
		nil,                                             // digestStore
		nil,                                             // settingService
		nil,                                             // tlsFPProfileService
		nil,                                             // channelService
		nil,                                             // resolver
		nil,                                             // compositeResolver
		nil,                                             // balanceNotifyService
		nil,                                             // userPlatformQuotaRepo
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

	body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"question"}],"stream":false}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	apiKey := &service.APIKey{
		ID: 9302, UserID: 9303, GroupID: &groupID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 9303, Concurrency: 10, Balance: 100},
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

	h.ChatCompletions(c)

	// 下游：第一代的合法内容 + 非成功终态，没有成功完成信号，也没有第二个错误响应体。
	wantStatus := http.StatusOK
	wantCalls := 2
	if noAnswer {
		wantStatus = http.StatusBadGateway
		wantCalls = 1
	}
	require.Equal(t, wantStatus, recorder.Code, "body=%s", recorder.Body.String())
	wire := recorder.Body.String()
	if !empty {
		require.Contains(t, wire, "plan", "第一代已收到的思考仍然交付下游")
	} else {
		require.NotContains(t, wire, "reasoning_content", "do not invent thinking in a truly empty result")
	}
	require.Contains(t, wire, `"finish_reason":null`, "追加生成失败不得伪装成成功完成")
	require.NotContains(t, wire, `"finish_reason":"stop"`)
	if noAnswer {
		require.Contains(t, wire, `"code":"thought_only_response"`)
		require.Equal(t, 1, bytes.Count(recorder.Body.Bytes(), []byte(`"error"`)), "one protocol error, no generic duplicate")
		require.Contains(t, wire, `"prompt_tokens":8`)
		require.Equal(t, int64(wantOutput), gjson.Get(wire, "usage.completion_tokens").Int())
	} else {
		require.NotContains(t, wire, "answer")
		require.NotContains(t, wire, `"error"`, "Task24 终态错误已被视为已告知，不再补写错误响应")
	}

	// 请求/slot：第一代 + 一次追加生成，用户与账号槽位各获取一次、各释放一次（无泄漏、无第二次
	// 账号请求）。若错误分支被当成可 failover 错误，这里会出现第二轮槽位获取与第三次上游请求。
	require.Equal(t, wantCalls, upstream.calls, "no gateway retry/failover; B off makes no appended request")
	require.Equal(t, int64(1), concurrencyCache.userAcquires.Load(), "用户槽位只获取一次")
	require.Equal(t, int64(1), concurrencyCache.userReleases.Load(), "用户槽位必须释放一次")
	require.Equal(t, int64(1), concurrencyCache.accountAcquires.Load(), "账号槽位只获取一次")
	require.Equal(t, int64(1), concurrencyCache.accountReleases.Load(), "账号槽位必须释放一次，不能泄漏")

	// 计费：第一代用量经由 Handler 的错误分支入账（usageLogRepo.Create seam）。
	require.Len(t, usageLogRepo.created, 1, "第一代恰好入账一次")
	require.Equal(t, 8, usageLogRepo.created[0].InputTokens, "入账的是第一代的真实用量")
	require.Equal(t, wantOutput, usageLogRepo.created[0].OutputTokens)
	require.Equal(t, account.ID, usageLogRepo.created[0].AccountID)
}

// thoughtUsageLogRepoStub 记录 RunModeSimple 下真正写入的用量日志。
type thoughtUsageLogRepoStub struct {
	service.UsageLogRepository
	created []*service.UsageLog
}

func (s *thoughtUsageLogRepoStub) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	s.created = append(s.created, log)
	return true, nil
}

// countingThoughtConcurrencyCache 只统计用户/账号槽位的获取与释放次数，其余行为沿用假缓存。
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
