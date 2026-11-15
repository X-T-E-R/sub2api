//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const unspecified429Body = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestAntigravity429CooldownSignals(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.AntigravityFallbackCooldownMinutes = 1
	svc := &AntigravityGatewayService{settingService: &SettingService{cfg: cfg}}
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		want       time.Duration
	}{
		{"bare", unspecified429Body, nil, 3 * time.Second},
		{"empty details", strings.Replace(unspecified429Body, `"status"`, `"details":[],"status"`, 1), nil, 3 * time.Second},
		{"null details", strings.Replace(unspecified429Body, `"status"`, `"details":null,"status"`, 1), nil, 3 * time.Second},
		{"header seconds", unspecified429Body, http.Header{"Retry-After": {"120"}}, 120 * time.Second},
		{"header date", unspecified429Body, http.Header{"Retry-After": {time.Now().Add(150 * time.Second).UTC().Format(http.TimeFormat)}}, 150 * time.Second},
		{"quota metadata", string(buildGeminiRateLimitBody("120s")), nil, 120 * time.Second},
		{"retry info without model", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"90s"}]}}`, nil, 90 * time.Second},
		{"quota info without delay", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`, nil, time.Minute},
		{"rate info without delay", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`, nil, time.Minute},
		{"invalid header keeps fallback", unspecified429Body, http.Header{"Retry-After": {"invalid"}}, time.Minute},
		{"unknown envelope", `{"error":{"message":"other failure"}}`, nil, time.Minute},
		{"malformed", `{`, nil, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want.Seconds(), svc.antigravity429Cooldown(tc.headers, []byte(tc.body)).Seconds(), 1.1)
		})
	}
	// A longer header must never be shortened by body quota metadata.
	require.InDelta(t, 180, svc.antigravity429Cooldown(http.Header{"Retry-After": {"180"}}, buildGeminiRateLimitBody("15s")).Seconds(), 1)
}

func TestAntigravityBare429ModelAndAccountFallback(t *testing.T) {
	for _, model := range []string{"gemini-4-carbon", ""} {
		t.Run(model, func(t *testing.T) {
			repo := &stubAntigravityAccountRepo{}
			svc := &AntigravityGatewayService{accountRepo: repo}
			account := &Account{ID: 77, Platform: PlatformAntigravity}
			start := time.Now()
			svc.handleUpstreamError(context.Background(), "[test]", account, 429, nil, []byte(unspecified429Body), model, 11, "", false)
			if model == "" {
				require.Len(t, repo.rateCalls, 1)
				require.WithinDuration(t, start.Add(3*time.Second), repo.rateCalls[0].resetAt, time.Second)
			} else {
				require.Empty(t, repo.rateCalls, "do not lock the whole account for one prompt")
				require.Len(t, repo.modelRateLimitCalls, 1)
				require.Equal(t, model, repo.modelRateLimitCalls[0].modelKey)
				require.WithinDuration(t, start.Add(3*time.Second), repo.modelRateLimitCalls[0].resetAt, time.Second)
			}
		})
	}
}

type unspecified429Upstream struct {
	HTTPUpstream
	calls int
}

func (u *unspecified429Upstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(unspecified429Body))}, nil
}
func (u *unspecified429Upstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestAntigravityBare429RetryExhaustionKeepsFailoverAndShortCooldown(t *testing.T) {
	repo := &stubAntigravityAccountRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	u := &unspecified429Upstream{}
	account := &Account{ID: 77, Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx: context.Background(), account: account, accessToken: "test", action: "streamGenerateContent",
		body: []byte(`{"request":{"model":"gemini-4-carbon"}}`), requestedModel: "gemini-4-carbon",
		httpUpstream: u, accountRepo: repo, handleError: svc.handleUpstreamError,
	})
	require.NoError(t, err)
	defer result.resp.Body.Close()
	require.Equal(t, antigravityMaxRetries, u.calls)
	require.Equal(t, 429, result.resp.StatusCode)
	body, err := io.ReadAll(result.resp.Body)
	require.NoError(t, err)
	require.Equal(t, unspecified429Body, string(body), "preserve the upstream failure")
	require.True(t, svc.shouldFailoverUpstreamError(result.resp.StatusCode))
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.InDelta(t, 3, time.Until(repo.modelRateLimitCalls[0].resetAt).Seconds(), .5)
	require.Empty(t, repo.rateCalls)
}

func TestAntigravityStructured429PreservesLongHeader(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-4-carbon"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"15s"}]}}`)
	repo := &stubAntigravityAccountRepo{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	result := svc.handleUpstreamError(context.Background(), "[test]", &Account{ID: 77, Platform: PlatformAntigravity}, 429, http.Header{"Retry-After": {"180"}}, body, "gemini-4-carbon", 11, "", false)
	require.NotNil(t, result.SwitchError)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.InDelta(t, 180, time.Until(repo.modelRateLimitCalls[0].resetAt).Seconds(), 1)
}
