package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type antigravityFingerprintUpstream struct {
	queuedHTTPUpstreamStub
	profiles []*tlsfingerprint.Profile
}

func (s *antigravityFingerprintUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, errors.New("plain transport bypassed fingerprint resolution")
}

func (s *antigravityFingerprintUpstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	s.profiles = append(s.profiles, profile)
	return s.queuedHTTPUpstreamStub.Do(req, proxy, account, concurrency)
}

func TestAntigravityTLSProfileNilSafe(t *testing.T) {
	account := &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth, Extra: map[string]any{"enable_tls_fingerprint": true}}
	require.Nil(t, (*AntigravityGatewayService)(nil).resolveTLSProfile(account))
	require.Nil(t, (&AntigravityGatewayService{}).resolveTLSProfile(account))
	svc := &AntigravityGatewayService{tlsFPProfileService: &TLSFingerprintProfileService{}}
	require.NotNil(t, svc.resolveTLSProfile(account))
	account.Extra["enable_tls_fingerprint"] = false
	require.Nil(t, svc.resolveTLSProfile(account))
}

func TestAntigravityAllTransportsResolveTLSProfile(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, path := range []string{"inference_and_continuation", "passthrough"} {
			t.Run(path+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				upstream := &antigravityFingerprintUpstream{}
				account := antigravityThoughtAccount(map[string]any{"enable_tls_fingerprint": enabled})
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
				svc.tlsFPProfileService = &TLSFingerprintProfileService{}
				if path == "passthrough" {
					account.Credentials["base_url"] = "https://upstream.invalid"
					account.Credentials["api_key"] = "fixture"
					upstream.responses = []*http.Response{{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":1}}`))}}
					body, c, _ := newAntigravityThoughtClaudeRequest(false)
					_, err := svc.ForwardUpstream(context.Background(), c, account, body)
					require.NoError(t, err)
					require.Len(t, upstream.profiles, 1)
				} else {
					upstream.responses = []*http.Response{antigravityThoughtStreamResponse(antigravityEmptyStopLine), antigravityCompatSuccessResponse()}
					body, c, _ := newAntigravityThoughtClaudeRequest(false)
					_, err := svc.Forward(context.Background(), c, account, body, false)
					require.NoError(t, err)
					require.Len(t, upstream.profiles, 2, "initial inference and legal continuation use the same TLS route")
				}
				for _, profile := range upstream.profiles {
					if enabled {
						require.NotNil(t, profile)
					} else {
						require.Nil(t, profile)
					}
				}
			})
		}
	}
}

const antigravityBare429Fixture = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted"}}`

func hardeningErrorResponse(status int, header http.Header, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAntigravityContentSuspect429Classification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		body   string
		want   bool
	}{
		{"bare", 429, nil, antigravityBare429Fixture, true},
		{"empty", 429, nil, "", true},
		{"retry_after", 429, http.Header{"Retry-After": {"1"}}, antigravityBare429Fixture, false},
		{"unparsed_retry_after", 429, http.Header{"Retry-After": {"invalid"}}, antigravityBare429Fixture, false},
		{"quota", 429, nil, `{"error":{"message":"QUOTA_EXHAUSTED"}}`, false},
		{"rate", 429, nil, `{"error":{"message":"RATE_LIMIT_EXCEEDED"}}`, false},
		{"retry_info", 429, nil, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1s"}]}}`, false},
		{"incomplete_retry_info", 429, nil, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo"}]}}`, false},
		{"error_info_without_model", 429, nil, `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`, false},
		{"server_error", 503, nil, antigravityBare429Fixture, false},
		{"auth", 401, nil, antigravityBare429Fixture, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isAntigravityContentSuspect429(tc.status, tc.header, []byte(tc.body)))
		})
	}
}

func TestAntigravityBare429RetriesOnceThenFailover(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(strconv.FormatBool(recover), func(t *testing.T) {
			second := hardeningErrorResponse(429, nil, antigravityBare429Fixture)
			if recover {
				second = antigravityCompatSuccessResponse()
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{hardeningErrorResponse(429, nil, antigravityBare429Fixture), second}}
			svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
			body, c, _ := newAntigravityThoughtClaudeRequest(false)
			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			require.Equal(t, 2, upstream.callCount)
			if recover {
				require.NoError(t, err)
				return
			}
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.ContentSuspect429)
			require.Equal(t, 429, failover.StatusCode)
			require.Equal(t, antigravityBare429Fixture, string(failover.ResponseBody))
		})
	}
}

func TestAntigravityExplicit429RetainsRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		header     http.Header
	}{
		{"retry_after", antigravityBare429Fixture, http.Header{"Retry-After": {"1"}}},
		{"quota", `{"error":{"message":"QUOTA_EXHAUSTED"}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{}
			for i := 0; i < antigravityMaxRetries; i++ {
				upstream.responses = append(upstream.responses, hardeningErrorResponse(429, tc.header, tc.body))
			}
			svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
			body, c, _ := newAntigravityThoughtClaudeRequest(false)
			_, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.False(t, failover.ContentSuspect429)
			require.Equal(t, antigravityMaxRetries, upstream.callCount)
		})
	}
}

func TestAntigravityClaudeTargetUsesScopedNativeSession(t *testing.T) {
	account := antigravityThoughtAccount(nil)
	account.Credentials["model_mapping"] = map[string]any{"client-alias": "claude-sonnet-4-5"}
	body := []byte(`{"model":"client-alias","max_tokens":64,"metadata":{"user_id":"session_123e4567-e89b-12d3-a456-426614174000"},"messages":[{"role":"user","content":"fixture"}]}`)
	key := &APIKey{ID: 11}
	first := forwardWireSessionFixture(t, body, "", "", key, account)
	n, err := strconv.ParseInt(first.Request.SessionID, 10, 64)
	require.NoError(t, err)
	require.Negative(t, n)
	account.ID++
	second := forwardWireSessionFixture(t, body, "", "", key, account)
	require.NotEqual(t, first.Request.SessionID, second.Request.SessionID)
	wire, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "session_123e4567")
}

func TestTLSFingerprintEligibilityPreservesExistingPlatforms(t *testing.T) {
	for _, tc := range []struct {
		platform, kind string
		want           bool
	}{
		{PlatformAntigravity, AccountTypeOAuth, true},
		{PlatformAntigravity, AccountTypeUpstream, true},
		{PlatformAnthropic, AccountTypeOAuth, true},
		{PlatformAnthropic, AccountTypeSetupToken, true},
		{PlatformAnthropic, AccountTypeAPIKey, false},
		{PlatformGemini, AccountTypeOAuth, false},
		{PlatformOpenAI, AccountTypeOAuth, false},
	} {
		t.Run(tc.platform+"/"+tc.kind, func(t *testing.T) {
			account := &Account{Platform: tc.platform, Type: tc.kind, Extra: map[string]any{"enable_tls_fingerprint": true}}
			require.Equal(t, tc.want, account.IsTLSFingerprintEnabled())
			account.Extra["enable_tls_fingerprint"] = false
			require.False(t, account.IsTLSFingerprintEnabled())
			account.Extra = nil
			require.False(t, account.IsTLSFingerprintEnabled())
		})
	}
}
