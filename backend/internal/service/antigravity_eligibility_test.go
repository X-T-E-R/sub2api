//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAntigravityEligibilityQuotaProjection(t *testing.T) {
	for _, tc := range []struct {
		name, load, tier string
		entries          []AntigravityIneligibleTier
	}{
		{"free with other restricted tier", `{"currentTier":{"id":"free-tier"},"ineligibleTiers":[{"tier":{"id":"g1-ultra-tier"},"reasonCode":"INELIGIBLE_ACCOUNT","reasonMessage":"Tier unavailable"}]}`, "FREE", []AntigravityIneligibleTier{{TierID: "g1-ultra-tier", ReasonCode: "INELIGIBLE_ACCOUNT", ReasonMessage: "Tier unavailable"}}},
		{"paid with other restricted tier", `{"paidTier":"g1-pro-tier","ineligibleTiers":[{"tier":"g1-ultra-tier","reasonCode":"VALIDATION_REQUIRED"}]}`, "PRO", []AntigravityIneligibleTier{{TierID: "g1-ultra-tier", ReasonCode: "VALIDATION_REQUIRED"}}},
		{"no tier with reason", `{"ineligibleTiers":[{"reasonCode":"INELIGIBLE_ACCOUNT"}]}`, "", []AntigravityIneligibleTier{{ReasonCode: "INELIGIBLE_ACCOUNT"}}},
		{"nil and empty entries", `{"currentTier":"free-tier","ineligibleTiers":[null,{}, {"tier":{},"reasonMessage":"  "}]}`, "FREE", nil},
		{"tier without reason", `{"ineligibleTiers":[{"tier":" g1-ultra-tier "}]}`, "", []AntigravityIneligibleTier{{TierID: "g1-ultra-tier"}}},
		{"malformed reason leaves quota usable", `{"currentTier":"free-tier","ineligibleTiers":[{"reasonCode":42}]}`, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1internal:loadCodeAssist":
					_, _ = io.WriteString(w, tc.load)
				case "/v1internal:fetchAvailableModels":
					_, _ = io.WriteString(w, `{"models":{"claude":{"quotaInfo":{"remainingFraction":0.6}}}}`)
				case "/v1internal:retrieveUserQuotaSummary":
					_, _ = io.WriteString(w, `{"groups":[]}`)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAntigravityUsageBaseURL(t, server.URL)
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), &Account{
				Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "synthetic", "project_id": "synthetic-project"},
			}, "")
			require.NoError(t, err)
			info := result.UsageInfo
			require.Equal(t, tc.tier, info.SubscriptionTier)
			require.Equal(t, tc.entries, info.AntigravityIneligibleTiers)
			require.Equal(t, len(tc.entries) > 0, info.AntigravityIneligible)
			require.False(t, info.IsForbidden)
			require.False(t, info.NeedsVerify)
			require.False(t, info.NeedsReauth)
			require.Equal(t, 40, info.AntigravityQuota["claude"].Utilization)
			// Exercise the API/cache JSON boundary, not only the Go struct.
			data, err := json.Marshal(info)
			require.NoError(t, err)
			var restored UsageInfo
			require.NoError(t, json.Unmarshal(data, &restored))
			require.Equal(t, tc.entries, restored.AntigravityIneligibleTiers)
		})
	}
}

func TestAntigravityEligibilityDoesNotMaskForbidden(t *testing.T) {
	for _, tc := range []struct{ body, kind string }{
		{`{"error":{"message":"Access denied"}}`, "forbidden"},
		{`{"error":{"message":"VALIDATION_REQUIRED","details":[{"metadata":{"validation_url":"https://example.test/verify"}}]}}`, "validation"},
		{`{"error":{"message":"Terms of Service violation"}}`, "violation"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
					_, _ = io.WriteString(w, `{"currentTier":"free-tier","ineligibleTiers":[{"tier":"g1-ultra-tier"}]}`)
					return
				}
				http.Error(w, tc.body, http.StatusForbidden)
			}))
			defer server.Close()
			withAntigravityUsageBaseURL(t, server.URL)
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), &Account{
				Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "synthetic"},
			}, "")
			require.NoError(t, err)
			require.True(t, result.UsageInfo.IsForbidden)
			require.Equal(t, tc.kind, result.UsageInfo.ForbiddenType)
			require.Equal(t, tc.kind == "validation", result.UsageInfo.NeedsVerify)
			require.Equal(t, tc.kind == "violation", result.UsageInfo.IsBanned)
			if tc.kind == "validation" {
				require.Equal(t, "https://example.test/verify", result.UsageInfo.ValidationURL)
			}
		})
	}
}

func TestAntigravityEligibilityRefreshPersistsCurrentPlan(t *testing.T) {
	for _, tc := range []struct{ tier, plan string }{
		{"free-tier", "Free"}, {"g1-pro-tier", "Pro"}, {"g1-ultra-tier", "Ultra"},
	} {
		t.Run(tc.plan, func(t *testing.T) {
			oldTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			var calls []string
			http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Path)
				var body string
				switch {
				case r.URL.String() == antigravity.TokenURL:
					body = `{"access_token":"new-synthetic","expires_in":3600}`
				case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
					body = fmt.Sprintf(`{"cloudaicompanionProject":"synthetic-project","currentTier":"%s","ineligibleTiers":[{"tier":"other-tier","reasonCode":"INELIGIBLE_ACCOUNT"}]}`, tc.tier)
				default:
					return nil, fmt.Errorf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})
			account := &Account{ID: 5210, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
				Status: StatusActive, ErrorMessage: "independent validation error", Schedulable: false,
				Credentials: map[string]any{"access_token": "old-synthetic", "refresh_token": "synthetic-refresh", "expires_at": "1", "plan_type": "Abnormal"},
				Extra:       map[string]any{"needs_verify": true, "subscription_status": "historical"},
			}
			repo := &quotaRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			result, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(context.Background(), account, NewAntigravityTokenRefresher(&AntigravityOAuthService{}), time.Minute)
			require.NoError(t, err)
			require.True(t, result.Refreshed)
			stored, err := repo.GetByID(context.Background(), account.ID)
			require.NoError(t, err)
			require.Equal(t, tc.plan, stored.GetCredential("plan_type"))
			require.Equal(t, "new-synthetic", stored.GetCredential("access_token"))
			require.Equal(t, account.Status, stored.Status)
			require.Equal(t, account.ErrorMessage, stored.ErrorMessage)
			require.Equal(t, account.Schedulable, stored.Schedulable)
			require.Equal(t, account.Extra, stored.Extra)
			require.Len(t, calls, 2, "refresh and discovery only; no model, onboarding or privacy calls")
			// An actual account error remains excluded by the existing refresh gate.
			repo.account.Status = StatusError
			repo.account.Credentials["expires_at"] = "1"
			blocked, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(context.Background(), repo.account, NewAntigravityTokenRefresher(&AntigravityOAuthService{}), time.Minute)
			require.NoError(t, err)
			require.False(t, blocked.Refreshed)
			require.Equal(t, StatusError, blocked.Account.Status)
			require.Len(t, calls, 2)
		})
	}
}
