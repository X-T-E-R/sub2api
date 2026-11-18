package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const gemini358SoftSafetyBlockText = "This request was blocked by Gemini's filters. They can occasionally trigger by mistake on safe coding, security, or biology-related queries. Please try rephrasing your prompt. You can [send feedback](https://aistudio.google.com/docs/troubleshooting#file-bug) or read more about [our policies here](https://policies.google.com/terms/generative-ai/use-policy)."

func newSafetyStreakSoftBlockSSEResponse() *http.Response {
	// Real Google behavior: HTTP 200 SSE emits the 358-char filter message and closes EOF without finishReason: STOP.
	payload, _ := json.Marshal(gemini358SoftSafetyBlockText)
	sse := `data: {"response":{"responseId":"resp_soft_block","candidates":[{"content":{"parts":[{"text":` + string(payload) + `}]}}]}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func newSafetyStreakHardBlockSSEResponse() *http.Response {
	sse := `data: {"response":{"responseId":"resp_hard_block","candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func newSafetyStreakToolUseSSEResponse() *http.Response {
	sse := `data: {"response":{"responseId":"resp_tool_ok","candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"main.go"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func makeSafetyStreakMessagesBody(sessionUUID string) []byte {
	userHash := strings.Repeat("a", 64)
	body, _ := json.Marshal(map[string]any{
		"model":      "gemini-3.1-pro-high",
		"stream":     true,
		"max_tokens": 512,
		"metadata": map[string]any{
			"user_id": "user_" + userHash + "_account__session_" + sessionUUID,
		},
		"messages": []map[string]any{
			{"role": "user", "content": "Continue."},
		},
	})
	return body
}

func forwardWithAuthenticatedKey(
	svc *AntigravityGatewayService,
	key *APIKey,
	account *Account,
	body []byte,
) (*ForwardResult, error, *httptest.ResponseRecorder, *gin.Context) {
	c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	if key != nil {
		c.Set("api_key", key)
	}
	res, err := svc.Forward(context.Background(), c, account, body, true)
	return res, err, rec, c
}

func TestAntigravitySafetyStreakGuard_SoftStreakPreservesFirstTwoAndAbortsFourth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			antigravityCompatSuccessResponse(), // must never be reached on 4th call!
		},
	}
	svc := newAntigravityThoughtTestService(
		t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
		upstream,
	)

	groupID := int64(5)
	key := &APIKey{ID: 101, UserID: 1, GroupID: &groupID}
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := makeSafetyStreakMessagesBody("11111111-2222-3333-4444-555555555555")

	// Attempts 1, 2, and 3 hit upstream and preserve original streamed output for client "继续" recovery.
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, rec, _ := forwardWithAuthenticatedKey(svc, key, account, body)
		require.Equal(t, http.StatusOK, rec.Code, "attempt=%d", attempt)
		require.Contains(t, rec.Body.String(), "This request was blocked by Gemini's filters.", "attempt=%d must preserve soft-block stream text", attempt)
		require.Equal(t, attempt, upstream.callCount)
	}

	// Attempt 4 in the same session must be aborted pre-upstream with HTTP 400 antigravity_safety_streak_aborted.
	res4, err4, rec4, c4 := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Error(t, err4)
	require.Nil(t, res4)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err4, &failoverErr), "safety streak abort must never trigger UpstreamFailoverError")
	require.True(t, IsResponseCommitted(c4))
	require.Equal(t, http.StatusBadRequest, rec4.Code)
	require.Equal(t, 3, upstream.callCount, "4th request must NOT contact Google upstream")
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec4.Body.Bytes(), "error.type").String())
	require.Equal(t, antigravitySafetyStreakAbortedCode, gjson.GetBytes(rec4.Body.Bytes(), "error.code").String())
	require.False(t, gjson.GetBytes(rec4.Body.Bytes(), "error.retryable").Bool())
}

func TestAntigravitySafetyStreakGuard_CleanToolOrTextProgressResetsStreakImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			newSafetyStreakSoftBlockSSEResponse(), // soft_streak = 1
			newSafetyStreakSoftBlockSSEResponse(), // soft_streak = 2
			newSafetyStreakToolUseSSEResponse(),   // clean tool_use -> resets streak to 0!
			newSafetyStreakSoftBlockSSEResponse(), // soft_streak = 1 (not 3!)
			newSafetyStreakSoftBlockSSEResponse(), // soft_streak = 2
			antigravityCompatSuccessResponse(),    // clean text -> allowed and resets!
		},
	}
	svc := newAntigravityThoughtTestService(
		t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
		upstream,
	)

	groupID := int64(5)
	key := &APIKey{ID: 102, UserID: 1, GroupID: &groupID}
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := makeSafetyStreakMessagesBody("22222222-3333-4444-5555-666666666666")

	// 1 & 2: two soft blocks
	_, _, rec1, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Contains(t, rec1.Body.String(), "blocked by Gemini's filters")
	_, _, rec2, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Contains(t, rec2.Body.String(), "blocked by Gemini's filters")

	// 3: automatic "继续" succeeds with tool_use -> must reset streak to 0!
	res3, err3, rec3, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.NoError(t, err3)
	require.NotNil(t, res3)
	require.Contains(t, rec3.Body.String(), "tool_use")

	// 4 & 5: two more occasional soft blocks must NOT trip circuit because turn 3 reset the counter!
	_, _, rec4, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Equal(t, http.StatusOK, rec4.Code)
	_, _, rec5, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Equal(t, http.StatusOK, rec5.Code)

	// 6: still allowed through to upstream!
	res6, err6, rec6, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.NoError(t, err6)
	require.NotNil(t, res6)
	require.Equal(t, http.StatusOK, rec6.Code)
	require.Equal(t, 6, upstream.callCount)
}

func TestAntigravitySafetyStreakGuard_ParentMainSessionUnaffectedWhenSubagentTrippedAndCrossAccountShared(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			antigravityCompatSuccessResponse(), // consumed by parent main session!
		},
	}
	svc := newAntigravityThoughtTestService(
		t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
		upstream,
	)

	groupID := int64(5)
	key := &APIKey{ID: 103, UserID: 1, GroupID: &groupID}
	accountA := newAntigravityCompatAccount(AccountTypeOAuth)
	accountA.ID = 63
	accountB := newAntigravityCompatAccount(AccountTypeOAuth)
	accountB.ID = 71

	childBody := makeSafetyStreakMessagesBody("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	parentBody := makeSafetyStreakMessagesBody("ffffffff-1111-4222-8333-444444444444")

	// Trip child subagent session across accountA & accountB (3 soft blocks).
	_, _, _, _ = forwardWithAuthenticatedKey(svc, key, accountA, childBody)
	_, _, _, _ = forwardWithAuthenticatedKey(svc, key, accountB, childBody)
	_, _, _, _ = forwardWithAuthenticatedKey(svc, key, accountA, childBody)
	require.Equal(t, 3, upstream.callCount)

	// Child subagent is now blocked even on accountB (cross-account shared streak).
	_, childErr, childRec, _ := forwardWithAuthenticatedKey(svc, key, accountB, childBody)
	require.Error(t, childErr)
	require.Equal(t, http.StatusBadRequest, childRec.Code)
	require.Equal(t, antigravitySafetyStreakAbortedCode, gjson.GetBytes(childRec.Body.Bytes(), "error.code").String())
	require.Equal(t, 3, upstream.callCount)

	// Parent main session with distinct session UUID passes 100% normally!
	parentRes, parentErr, parentRec, _ := forwardWithAuthenticatedKey(svc, key, accountA, parentBody)
	require.NoError(t, parentErr)
	require.NotNil(t, parentRes)
	require.Equal(t, http.StatusOK, parentRec.Code)
	require.Equal(t, 4, upstream.callCount)
}

func TestAntigravitySafetyStreakGuard_HardSafetyBlocksAbortOnThirdAndNeverFailover403Safety(t *testing.T) {
	gin.SetMode(gin.TestMode)

	safety403Body := `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Request blocked by safety policy: PROHIBITED_CONTENT","details":[{"reason":"PROHIBITED_CONTENT"}]}}`
	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			newSafetyStreakHardBlockSSEResponse(), // hard_streak = 1 (SSE SAFETY finishReason)
			{
				StatusCode: http.StatusForbidden, // hard_streak = 2 (HTTP 403 safety block -> must NOT failover!)
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(safety403Body)),
			},
			antigravityCompatSuccessResponse(), // must never be reached on 3rd call!
		},
	}
	svc := newAntigravityThoughtTestService(
		t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
		upstream,
	)

	groupID := int64(5)
	key := &APIKey{ID: 104, UserID: 1, GroupID: &groupID}
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := makeSafetyStreakMessagesBody("99999999-8888-4777-8666-555555555555")

	// 1st hard safety block (SSE finishReason: SAFETY -> AntigravityNoUsableAnswerError Code=="antigravity_safety")
	_, err1, rec1, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.True(t, IsAntigravityNoUsableAnswerError(err1))
	require.Contains(t, rec1.Body.String(), `"code":"antigravity_safety"`)
	require.Equal(t, 1, upstream.callCount)

	// 2nd hard safety block (HTTP 403 PROHIBITED_CONTENT -> must NOT return UpstreamFailoverError!)
	_, err2, rec2, c2 := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Error(t, err2)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err2, &failoverErr), "HTTP 403 safety block must not trigger cross-account failover")
	require.True(t, IsResponseCommitted(c2))
	require.GreaterOrEqual(t, rec2.Code, 400)
	require.Equal(t, 2, upstream.callCount)

	// 3rd request in same session is blocked pre-upstream with HTTP 400 antigravity_safety_streak_aborted!
	res3, err3, rec3, _ := forwardWithAuthenticatedKey(svc, key, account, body)
	require.Error(t, err3)
	require.Nil(t, res3)
	require.Equal(t, http.StatusBadRequest, rec3.Code)
	require.Equal(t, antigravitySafetyStreakAbortedCode, gjson.GetBytes(rec3.Body.Bytes(), "error.code").String())
	require.False(t, gjson.GetBytes(rec3.Body.Bytes(), "error.retryable").Bool())
	require.Equal(t, 2, upstream.callCount, "3rd request after 2 hard safety blocks must not hit upstream")
}

type stubSafetyStreakGatewayCache struct {
	GatewayCache
	data map[string][]byte
	ttls map[string]int64
}

func (s *stubSafetyStreakGatewayCache) GetAntigravitySafetyStreak(_ context.Context, sessionHash string) ([]byte, error) {
	raw, ok := s.data["agy:safety_streak:"+sessionHash]
	if !ok {
		return nil, ErrAntigravitySafetyStreakNotFound
	}
	return raw, nil
}

func (s *stubSafetyStreakGatewayCache) SetAntigravitySafetyStreak(_ context.Context, sessionHash string, payload []byte, ttl time.Duration) error {
	s.data["agy:safety_streak:"+sessionHash] = append([]byte(nil), payload...)
	s.ttls["agy:safety_streak:"+sessionHash] = int64(ttl.Seconds())
	return nil
}

func (s *stubSafetyStreakGatewayCache) DeleteAntigravitySafetyStreak(_ context.Context, sessionHash string) error {
	delete(s.data, "agy:safety_streak:"+sessionHash)
	return nil
}

func TestAntigravitySafetyStreakGuard_RedisStoreAndStatelessBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(),
			newSafetyStreakSoftBlockSSEResponse(), // stateless request still passes!
		},
	}
	svc := newAntigravityThoughtTestService(
		t,
		AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true},
		upstream,
	)
	redisCache := &stubSafetyStreakGatewayCache{
		data: make(map[string][]byte),
		ttls: make(map[string]int64),
	}
	svc.cache = redisCache

	groupID := int64(5)
	key := &APIKey{ID: 205, UserID: 1, GroupID: &groupID}
	account := newAntigravityCompatAccount(AccountTypeOAuth)

	// Stateless request (no metadata.user_id / session header) is never tracked even after 4 soft blocks!
	statelessBody := []byte(`{"model":"gemini-3.1-pro-high","stream":true,"max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 4; i++ {
		_, _, rec, _ := forwardWithAuthenticatedKey(svc, key, account, statelessBody)
		require.Equal(t, http.StatusOK, rec.Code)
	}
	require.Empty(t, redisCache.data)

	// Session-scoped request writes Redis key agy:safety_streak:<hash> with 15m (900s) TTL.
	upstream.responses = append(upstream.responses, newSafetyStreakSoftBlockSSEResponse(), antigravityCompatSuccessResponse())
	sessionBody := makeSafetyStreakMessagesBody("77777777-6666-4555-8444-333333333333")
	_, _, _, _ = forwardWithAuthenticatedKey(svc, key, account, sessionBody)
	require.Len(t, redisCache.data, 1)
	for k, ttlSec := range redisCache.ttls {
		require.True(t, strings.HasPrefix(k, "agy:safety_streak:"))
		require.Equal(t, int64(900), ttlSec)
	}

	// Clean turn deletes the Redis key immediately.
	_, errClean, _, _ := forwardWithAuthenticatedKey(svc, key, account, sessionBody)
	require.NoError(t, errClean)
	require.Empty(t, redisCache.data)
}
