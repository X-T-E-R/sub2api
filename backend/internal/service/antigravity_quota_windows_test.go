//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

type quotaRedirectTransport func(*http.Request) (*http.Response, error)

func (f quotaRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAntigravitySummaryUsesEffectiveModelBase(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	prod, daily := antigravity.BaseURLs[0], antigravity.BaseURLs[1]
	var summaryURL string
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{}`
		headers := make(http.Header)
		switch r.URL.String() {
		case prod + "/v1internal:fetchAvailableModels":
			status = 307
			headers.Set("Location", daily+"/v1internal:fetchAvailableModels")
		case daily + "/v1internal:fetchAvailableModels":
			require.Equal(t, "POST", r.Method)
			body = `{"models":{"gemini-pro":{"quotaInfo":{"remainingFraction":0.8}}}}`
		case daily + "/v1internal:retrieveUserQuotaSummary":
			summaryURL = r.URL.String()
			require.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
			data, _ := io.ReadAll(r.Body)
			require.JSONEq(t, `{"project":"shared"}`, string(data))
			body = `{"groups":[{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.3}]}]}`
		default:
			if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
				body = `{"paidTier":{"id":"g1-pro-tier"}}`
			} else {
				status = 404
			}
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: headers, Request: r}, nil
	})
	account := &Account{ID: 900, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic", "project_id": "shared"}}
	result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), account, "")
	require.NoError(t, err)
	require.Equal(t, daily+"/v1internal:retrieveUserQuotaSummary", summaryURL)
	require.Equal(t, 0.3, result.UsageInfo.AntigravityWindows["claude_5h"].RemainingFraction)
}

func TestExplicitAntigravityWindows(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	var summary antigravity.UserQuotaSummary
	require.NoError(t, json.Unmarshal([]byte(`{"groups":[{"buckets":[
	{"bucketId":"3p-5h","remainingFraction":0,"resetTime":"2026-09-04T12:00:00Z"},
	{"bucketId":"claude:weekly","remainingFraction":0.23},
	{"bucketId":"gemini:5h","remainingFraction":0.42,"resetTime":"2026-09-07T12:00:00Z"},
	{"bucketId":"gemini-weekly","remainingFraction":1},
	{"bucketId":"claude-high","remainingFraction":1}
	]}]}`), &summary))
	info := &UsageInfo{}
	applyAntigravitySummary(info, &summary, now)
	require.Len(t, info.AntigravityWindows, 4)
	require.Equal(t, antigravityObservationAvailable, info.AntigravityWindowState)
	require.Equal(t, 0.0, info.AntigravityWindows["claude_5h"].RemainingFraction)
	require.Equal(t, "2026-09-04T12:00:00Z", info.AntigravityWindows["claude_5h"].ResetTime)
	require.Equal(t, 0.42, info.AntigravityWindows["gemini_5h"].RemainingFraction, "a reset >5h must not replenish quota")
	require.Equal(t, "2026-09-07T12:00:00Z", info.AntigravityWindows["gemini_5h"].ResetTime)
	require.Equal(t, "claude:weekly", info.AntigravityWindows["claude_weekly"].SourceBucketID)
	require.Equal(t, now, info.AntigravityWindows["claude_weekly"].ObservedAt)
	require.Nil(t, info.FiveHour)

	for _, fraction := range []*float64{nil, aqfFloatPtr(-0.1), aqfFloatPtr(1.01), aqfFloatPtr(math.NaN()), aqfFloatPtr(math.Inf(1))} {
		summary.Groups[0].Buckets[0].RemainingFraction = fraction
		applyAntigravitySummary(info, &summary, now)
		require.NotContains(t, info.AntigravityWindows, "claude_5h")
		require.Equal(t, antigravityObservationPartial, info.AntigravityWindowState)
	}
	summary.Groups[0].Buckets[0].RemainingFraction = aqfFloatPtr(0.7)
	summary.Groups[0].Buckets[0].ResetTime = "invalid"
	applyAntigravitySummary(info, &summary, now)
	require.Empty(t, info.AntigravityWindows["claude_5h"].ResetTime)
	require.Equal(t, antigravityObservationPartial, info.AntigravityWindowState)
	summary.Groups[0].Buckets = append(summary.Groups[0].Buckets, antigravity.QuotaSummaryBucket{BucketID: "claude:5h", RemainingFraction: aqfFloatPtr(0.9)})
	applyAntigravitySummary(info, &summary, now)
	require.NotContains(t, info.AntigravityWindows, "claude_5h", "duplicate aliases have no authoritative winner")
}

func TestAntigravitySummaryCacheRetentionAndScope(t *testing.T) {
	mode := "success"
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			if mode == "all-error" {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, `{"models":{"gemini-pro-low":{"quotaInfo":{"remainingFraction":0.8}}}}`)
		case "/v1internal:loadCodeAssist":
			fmt.Fprint(w, `{"paidTier":{"id":"g1-pro-tier","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"0"},{"creditType":"OTHER","creditAmount":"12.340"}]}}`)
		case "/v1internal:retrieveUserQuotaSummary":
			if mode == "error" {
				w.WriteHeader(503)
				return
			}
			if mode == "partial" {
				fmt.Fprint(w, `{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.9}]}]}`)
				return
			}
			fraction := "0.2"
			if r.Header.Get("Authorization") == "Bearer second" {
				fraction = "0.7"
			}
			fmt.Fprintf(w, `{"groups":[{"buckets":[{"bucketId":"3p-5h","remainingFraction":%s}]}]}`, fraction)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAntigravityUsageBaseURL(t, server.URL)
	account := &Account{ID: 801, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "first", "project_id": "shared"}}
	service := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil)}
	first, err := service.getAntigravityUsage(context.Background(), account, true)
	require.NoError(t, err)
	w := first.AntigravityWindows["claude_5h"]
	require.Equal(t, 0.2, w.RemainingFraction)
	mode = "error"
	failed, err := service.getAntigravityUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, antigravityObservationUnavailable, failed.AntigravityWindowState)
	require.Equal(t, "PRO", failed.SubscriptionTier)
	require.Equal(t, 20, failed.AntigravityQuota["gemini-pro-low"].Utilization)
	require.Equal(t, "0", failed.AICredits[0].AmountText)
	require.Equal(t, "12.340", failed.AICredits[1].AmountText)
	require.Equal(t, w.ObservedAt, failed.AntigravityWindows["claude_5h"].ObservedAt)
	require.True(t, failed.AntigravityWindows["claude_5h"].Stale)
	require.False(t, w.Stale, "merge must not mutate prior response")
	mode = "partial"
	partial, err := service.getAntigravityUsage(context.Background(), account, true)
	require.NoError(t, err)
	require.True(t, partial.AntigravityWindows["claude_5h"].Stale)
	require.False(t, partial.AntigravityWindows["gemini_5h"].Stale)
	require.Equal(t, 0.9, partial.AntigravityWindows["gemini_5h"].RemainingFraction)

	second := &Account{ID: 802, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "second", "project_id": "shared"}}
	require.Empty(t, service.getPassiveAntigravityUsage(second).AntigravityWindows)
	mode = "success"
	other, err := service.getAntigravityUsage(context.Background(), second, true)
	require.NoError(t, err)
	require.Equal(t, 0.7, other.AntigravityWindows["claude_5h"].RemainingFraction)
	require.NotContains(t, other.AntigravityWindows, "gemini_5h")
	before := requests
	for _, field := range []string{"project_id", "access_token", "refresh_token"} {
		original := account.Credentials[field]
		account.Credentials[field] = "changed-" + field
		require.Empty(t, service.getPassiveAntigravityUsage(account).AntigravityWindows)
		account.Credentials[field] = original
	}
	require.Equal(t, before, requests, "passive cache reads never query providers")
	account.Credentials["project_id"] = "new-project"
	changed, err := service.getAntigravityUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.NotContains(t, changed.AntigravityWindows, "gemini_5h", "new scope cannot merge earlier windows")
	require.False(t, changed.AntigravityWindows["claude_5h"].Stale)
	mode = "all-error"
	degraded, err := service.getAntigravityUsage(context.Background(), second, true)
	require.NoError(t, err)
	require.True(t, degraded.AntigravityWindows["claude_5h"].Stale)
	require.Equal(t, other.AntigravityWindows["claude_5h"].ObservedAt, degraded.AntigravityWindows["claude_5h"].ObservedAt)
}
