package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Task24 的形式化调用方夹具：追加生成（B）失败时，Forward 交给 Handler 的东西。
//
// 与 service 包的 Forward 夹具不同，这里从 Handler 的调用点出发，证明三件在分支层面才成立的
// 事情（gateway_handler.go 的 Messages）：
//   - Forward 返回「第一代可计费结果 + *service.AntigravityThoughtContinuationError」（:886）；
//     该错误不是 UpstreamFailoverError，因此 `if err != nil` 分支里唯一的 failover 分支
//     （:1017 起，FailoverContinue 会 continue 到下一轮上游请求与新的账号/并发槽位）不可达；
//   - 错误分支的 `if result != nil { submitForwardUsage(result) }`（:1063）会执行，它调用的
//     usage 提交 seam submitUsageRecordTask（:931）恰好入账一次第一代用量；
//   - 协议错误已在 Forward 内写出，gatewayForwardErrorAlreadyCommunicated（:1036）对本错误返回
//     true，Handler 不会再补写第二个错误帧；下游保留第一代思考与真实用量，绝不是成功终态。
//
// 注意：本夹具直接调用 Forward + usage 提交 seam，证明的是分支前提与入账 seam 本身；真正端到端
// 驱动 h.ChatCompletions 的版本在 gateway_handler_antigravity_thought_continuation_unit_test.go
// （`go test -tags unit`），那里断言槽位获取/释放次数与 usageLogRepo.Create 的落地记录。

const handlerThoughtGenerationOneLine = `data: {"response":{"responseId":"resp_gen1","candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true,"thoughtSignature":"sig-plan"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`

// handlerThoughtUpstream 是按调用顺序返回响应的最小 HTTPUpstream，并记录上游请求次数，
// 用来证明追加生成失败后没有重试、没有新的账号请求（即没有新的并发/调度槽位）。
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

func handlerThoughtClaudeContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	return c, recorder
}

// handlerThoughtAccount 只通过账号 Extra 打开开关（A 关、B 开）：即使 A 关，追加生成失败也不得
// 被当成第一代的成功完成。
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
	gin.SetMode(gin.TestMode)
	upstream := &handlerThoughtUpstream{
		responses: []*http.Response{handlerThoughtStream(handlerThoughtGenerationOneLine)},
		errors:    []error{nil, errors.New("dial tcp: connection refused")},
	}
	svc := service.NewAntigravityGatewayService(
		nil,
		nil,
		nil,
		service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil),
		nil,
		upstream,
		service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}),
		nil,
	)
	h := &GatewayHandler{antigravityGatewayService: svc}

	body := []byte(`{"model":"client-alias","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"question"}]}`)
	c, recorder := handlerThoughtClaudeContext(body)
	account := handlerThoughtAccount()

	// 与 gateway_handler.go Messages 的调用点完全一致（:881 记录写入字节数，:886 调用 Forward）。
	writerSizeBeforeForward := c.Writer.Size()
	result, err := h.antigravityGatewayService.Forward(c.Request.Context(), c, account, body, false)

	// 契约 1：追加生成失败时 Forward 返回"第一代可计费结果 + Task24 终态错误"。
	require.Error(t, err, "追加生成失败不得被伪装成成功完成")
	require.True(t, service.IsAntigravityThoughtContinuationError(err), "err=%v", err)
	var terminalErr *service.AntigravityThoughtContinuationError
	require.ErrorAs(t, err, &terminalErr)
	require.NotEmpty(t, terminalErr.Stage)
	require.NotNil(t, result, "第一代必须保持可计费")

	// 契约 2：该错误不是 UpstreamFailoverError，所以 Messages 唯一的 failover 分支
	// （:1017-1035，FailoverContinue 会 continue 到下一轮上游请求/新账号/新槽位）不可达。
	var failoverErr *service.UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "终态错误不得被当成 failover 错误")
	require.False(t, errors.Is(err, context.Canceled))
	require.False(t, errors.Is(err, context.DeadlineExceeded))

	// 契约 3：协议错误已经在 Forward 内写出，因此 Handler 的 gatewayForwardErrorAlreadyCommunicated
	// （:1036）判定为已告知，不会再补写第二个错误帧（ensureForwardErrorResponse，:1039）。
	require.True(t, gatewayForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err),
		"终态错误必须被视为已向下游告知，避免第二个错误响应")

	// 没有重试、没有新账号请求：上游只收到第一代与一次追加生成（同一个账号、同一个并发槽位）。
	require.Equal(t, 2, upstream.calls, "exactly the first generation and one appended generation")

	// 契约 4：计费走 Handler 真正的 usage 提交 seam（Messages 错误分支 :1063 的
	// `if result != nil { submitForwardUsage(result) }` → submitUsageRecordTask，:931）。
	// worker 池未注入时它同步执行，因此这里能直接观测到入账的用量。
	var billed []*service.ForwardResult
	h.submitUsageRecordTask(c.Request.Context(), func(context.Context) {
		billed = append(billed, result)
	})
	require.Len(t, billed, 1, "第一代恰好入账一次，且不会因为失败的追加生成而漏计")
	require.Equal(t, 8, billed[0].Usage.InputTokens, "入账的是第一代的真实用量")
	require.Equal(t, 5, billed[0].Usage.OutputTokens)

	// 线格式：协议错误 + 第一代思考与用量，没有成功终态，且只有一处错误帧。
	wire := recorder.Body.String()
	require.Contains(t, wire, `"thinking":"plan"`, "第一代已收到的思考仍然交付下游")
	require.Contains(t, wire, `"stop_reason":null`)
	require.Contains(t, wire, `"input_tokens":8`, "下游能看到真实用量")
	require.Contains(t, wire, "thought_continuation_error", "失败以协议错误对外呈现")
	require.Equal(t, 1, strings.Count(wire, `event: error`), "只允许一个错误帧")
	require.NotContains(t, wire, `"type":"message_stop"`)
	require.NotContains(t, wire, `"stop_reason":"end_turn"`)
	require.Equal(t, 1, strings.Count(wire, "event: message_start"), "仍是一个合法的下游响应")
}
