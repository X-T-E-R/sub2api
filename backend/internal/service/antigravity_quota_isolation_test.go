//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func quotaIsolationAccount() *Account {
	a := effortTestAccount(map[string]string{
		"gemini-3.8-flash": "gemini-3.8-flash-{effort}",
		"flash-alias":      "gemini-3.8-flash-medium",
		"gemini-4-argon":   "gemini-4-argon-{effort}",
		"argon-alias":      "gemini-4-argon-medium",
		"custom-*":         "gemini-3.8-flash-medium",
	})
	a.ID = 73
	a.Status = StatusActive
	a.Schedulable = true
	return a
}

func TestAntigravityQuotaIsolationLegacy73SchedulerAndForward(t *testing.T) {
	effortTestSettings(t)
	a := quotaIsolationAccount()
	// Exact production scopes/reset from account73 / ops32675. Relative time
	// keeps the same active-limit condition after that production reset expires.
	reset := time.Now().Add(105 * time.Hour)
	for _, key := range []string{"gemini-3.8-flash-medium", "gemini-3.8-flash", antigravityGeminiModelRateLimitKey} {
		setAccountModelRateLimitSnapshot(a, key, reset, "", time.Now())
	}
	ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"output_config":{"effort":"medium"}}`))
	for _, tc := range []struct {
		model   string
		blocked bool
	}{
		{"gemini-3.8-flash", true}, {"flash-alias", true}, {"custom-request", true},
		{"gemini-3.8-flash-high", false}, {"gemini-4-argon", false}, {"argon-alias", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			require.Equal(t, !tc.blocked, a.IsSchedulableForModelWithContext(ctx, tc.model), "formal scheduler model guard")
			body, _ := json.Marshal(map[string]any{"model": tc.model, "max_tokens": 256, "output_config": map[string]any{"effort": "medium"}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			_, err := svc.Forward(ctx, c, a, body, false)
			if tc.blocked {
				require.Error(t, err)
				require.Zero(t, upstream.callCount)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, upstream.callCount)
			}
		})
	}
	other := quotaIsolationAccount()
	other.ID = 64
	require.True(t, other.IsSchedulableForModelWithContext(ctx, "flash-alias"), "another account has independent quota")
	require.Contains(t, a.Extra[modelRateLimitsKey], antigravityGeminiModelRateLimitKey, "read compatibility must not erase persisted evidence")
}

func TestAntigravityQuotaIsolationWritesResolvedTargetOnly(t *testing.T) {
	effortTestSettings(t)
	for _, model := range []string{"gemini-3.8-flash", "flash-alias", "custom-request"} {
		t.Run(model, func(t *testing.T) {
			repo := &stubAntigravityAccountRepo{}
			svc := &AntigravityGatewayService{accountRepo: repo}
			ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"medium"}`))
			svc.handleUpstreamError(ctx, "test", quotaIsolationAccount(), 429, http.Header{}, []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"model":"gemini-3.8-flash","quotaResetDelay":"105h35m47s"}}]}}`), model, 0, "", false)
			require.Empty(t, repo.rateCalls)
			require.Len(t, repo.modelRateLimitCalls, 1)
			require.Equal(t, "gemini-3.8-flash-medium", repo.modelRateLimitCalls[0].modelKey)
			require.Equal(t, int64(73), repo.modelRateLimitCalls[0].accountID)
		})
	}
}

func TestAntigravityQuotaIsolationAccountLimitStillBlocks(t *testing.T) {
	effortTestSettings(t)
	a := quotaIsolationAccount()
	reset := time.Now().Add(time.Hour)
	a.RateLimitResetAt = &reset
	for _, model := range []string{"flash-alias", "gemini-4-argon"} {
		require.False(t, a.IsSchedulableForModelWithContext(context.Background(), model))
	}
	// A real account-level limit with no attributable model retains the existing
	// conservative account-level writer instead of dropping quota enforcement.
	repo := &stubAntigravityAccountRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	svc.handleUpstreamError(context.Background(), "test", a, 429, http.Header{}, []byte(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`), "", 0, "", false)
	require.Len(t, repo.rateCalls, 1)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestAntigravityQuotaIsolationLiteralBaseAndSharedTarget(t *testing.T) {
	effortTestSettings(t)
	a := effortTestAccount(map[string]string{"literal": "wire-bucket", "alias": "wire-bucket", "family": "wire-bucket-{effort}", "family-low": "same-wire-target", "family-high": "same-wire-target"})
	a.ID = 73
	setAccountModelRateLimitSnapshot(a, "wire-bucket", time.Now().Add(time.Hour), "legacy literal", time.Now())
	setAccountModelRateLimitSnapshot(a, "same-wire-target", time.Now().Add(time.Hour), "quota", time.Now())
	for _, tc := range []struct {
		model, effort string
		limited       bool
	}{
		{"literal", "medium", true}, {"alias", "high", true},
		{"family", "medium", false}, {"family", "low", true}, {"family", "high", true},
	} {
		ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"`+tc.effort+`"}`))
		require.Equal(t, tc.limited, a.isModelRateLimitedWithContext(ctx, tc.model), tc.model+"/"+tc.effort)
	}
	encoded, err := json.Marshal(a)
	require.NoError(t, err)
	var persisted Account
	require.NoError(t, json.Unmarshal(encoded, &persisted))
	require.True(t, persisted.isModelRateLimitedWithContext(context.Background(), "literal"), "persistent/cache JSON must retain literal base limits")
	expired := time.Now().Add(-time.Second)
	setAccountModelRateLimitSnapshot(&persisted, "wire-bucket", expired, "legacy literal", time.Now())
	require.False(t, persisted.isModelRateLimitedWithContext(context.Background(), "alias"), "normal expiry restores only the expired target")
	require.True(t, persisted.isModelRateLimitedWithContext(context.Background(), "family-high"))
}

func TestAntigravityQuotaIsolationRetryUsesWireTargetNotReportedName(t *testing.T) {
	effortTestSettings(t)
	for _, preLimited := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "precheck"}[preLimited], func(t *testing.T) {
			a := effortTestAccount(map[string]string{"client-alias": "mapped-before-transform"})
			a.ID = 73
			if preLimited {
				setAccountModelRateLimitSnapshot(a, "actual-wire-model", time.Now().Add(time.Hour), "quota", time.Now())
			}
			body := []byte(`{"model":"actual-wire-model"}`)
			response := `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"reported-model-group"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"20s"}]}}`
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}}}
			repo := &stubAntigravityAccountRepo{}
			svc := &AntigravityGatewayService{accountRepo: repo}
			_, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{ctx: context.Background(), prefix: "test", account: a, requestedModel: "client-alias", accessToken: "fixture", action: "streamGenerateContent", body: body, httpUpstream: upstream, accountRepo: repo, handleError: svc.handleUpstreamError})
			require.Error(t, err)
			if preLimited {
				require.Zero(t, upstream.callCount)
				require.Empty(t, repo.modelRateLimitCalls)
			} else {
				require.Equal(t, 1, upstream.callCount)
				require.Len(t, repo.modelRateLimitCalls, 1)
				require.Equal(t, "actual-wire-model", repo.modelRateLimitCalls[0].modelKey)
			}
			bound := withAntigravityRateLimitWireModel(context.Background(), a, body)
			other := *a
			other.ID = 64
			require.Empty(t, antigravityRateLimitWireModel(bound, &other), "wire binding must not leak across failover accounts")
		})
	}
}

func TestAntigravityQuotaIsolationShortRateLimitNotQuota(t *testing.T) {
	short := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"specific-model"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.2s"}]}}`)
	retry, limit, _, model, capacity := shouldTriggerAntigravitySmartRetry(quotaIsolationAccount(), short)
	require.True(t, retry)
	require.False(t, limit)
	require.False(t, capacity)
	require.Equal(t, "specific-model", model)
	quota := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"model":"specific-model","quotaResetDelay":"105h35m47s"}}]}}`)
	retry, limit, _, _, capacity = shouldTriggerAntigravitySmartRetry(quotaIsolationAccount(), quota)
	require.False(t, retry)
	require.False(t, limit)
	require.False(t, capacity)
	require.NotNil(t, ParseGeminiRateLimitResetTime(quota), "long quota stays with quota-reset handling rather than short retry classification")
}
