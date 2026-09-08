//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAntigravityQuotaAcquisitionResolvesProjectBeforeQuota(t *testing.T) {
	for _, project := range []string{`"resolved-project"`, `{"id":"resolved-project"}`} {
		t.Run(project, func(t *testing.T) {
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				require.Equal(t, "Bearer synthetic-account", r.Header.Get("Authorization"))
				var payload map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				switch r.URL.Path {
				case "/v1internal:loadCodeAssist":
					require.Equal(t, "FULL_ELIGIBILITY_CHECK", payload["mode"])
					require.Equal(t, "old-project", payload["cloudaicompanionProject"])
					_, _ = w.Write([]byte(`{"cloudaicompanionProject":` + project + `,"paidTier":{"id":"g1-pro-tier","availableCredits":[{"creditType":"PROMPT","creditAmount":"12"}]}}`))
				case "/v1internal:fetchAvailableModels":
					if payload["project"] != "resolved-project" {
						http.Error(w, "wrong project", http.StatusBadRequest)
						return
					}
					_, _ = w.Write([]byte(`{"models":{"claude":{"quotaInfo":{"remainingFraction":0.4}}}}`))
				case "/v1internal:retrieveUserQuotaSummary":
					require.Equal(t, "resolved-project", payload["project"])
					_, _ = w.Write([]byte(`{"groups":[]}`))
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAntigravityUsageBaseURL(t, server.URL)
			account := &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-account", "project_id": "old-project"}}
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), account, "")
			require.NoError(t, err)
			require.Equal(t, []string{"/v1internal:loadCodeAssist", "/v1internal:fetchAvailableModels", "/v1internal:retrieveUserQuotaSummary"}, calls)
			require.Equal(t, 60, result.UsageInfo.AntigravityQuota["claude"].Utilization)
			require.Equal(t, "PRO", result.UsageInfo.SubscriptionTier)
			require.Len(t, result.UsageInfo.AICredits, 1)
			require.Equal(t, "old-project", account.GetCredential("project_id"))
		})
	}
}

type quotaRefreshRepo struct {
	AccountRepository
	mu      sync.Mutex
	account *Account
}

func (r *quotaRefreshRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), nil
}

func (r *quotaRefreshRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account.Credentials = shallowCopyMap(credentials)
	return nil
}

func TestAntigravityQuotaActiveRefreshAndPassiveScope(t *testing.T) {
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	var refreshCalls, loadCalls, modelCalls, otherCalls atomic.Int32
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		body := `{}`
		switch {
		case r.URL.String() == antigravity.TokenURL:
			refreshCalls.Add(1)
			body = `{"access_token":"new-synthetic-token","refresh_token":"rotated-synthetic-refresh","expires_in":3600}`
		case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
			loadCalls.Add(1)
			require.Equal(t, "Bearer new-synthetic-token", r.Header.Get("Authorization"))
			// No project deliberately exercises both no-onboarding guards.
			body = `{"paidTier":{"id":"g1-pro-tier"}}`
		case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
			modelCalls.Add(1)
			require.Equal(t, "Bearer new-synthetic-token", r.Header.Get("Authorization"))
			body = `{"models":{"claude":{"quotaInfo":{"remainingFraction":0}}}}`
		case strings.HasSuffix(r.URL.Path, ":retrieveUserQuotaSummary"):
			body = `{"groups":[]}`
		default:
			otherCalls.Add(1)
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	account := antigravityIsolationAccount(908, "expired-synthetic-token")
	account.Status = StatusActive
	account.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	account.Credentials["refresh_token"] = "old-synthetic-refresh"
	delete(account.Credentials, "project_id")
	repo := &quotaRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	oauth := &AntigravityOAuthService{}
	provider := NewAntigravityTokenProvider(repo, nil, oauth)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), NewAntigravityTokenRefresher(oauth))
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), antigravityQuotaFetcher: ProvideAntigravityQuotaFetcher(nil, nil, provider)}
	passive, err := svc.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, antigravityObservationUnavailable, passive.AntigravityQuotaState)
	require.Zero(t, refreshCalls.Load())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			usage, err := svc.getAntigravityUsage(context.Background(), account, false)
			require.NoError(t, err)
			require.Equal(t, antigravityObservationAvailable, usage.AntigravityQuotaState)
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, refreshCalls.Load())
	require.Equal(t, loadCalls.Load(), modelCalls.Load(), "only quota discovery may call loadCodeAssist")
	require.Zero(t, otherCalls.Load())
	latest, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "rotated-synthetic-refresh", latest.GetCredential("refresh_token"))
	require.Empty(t, latest.GetCredential("project_id"))
	passive, err = svc.GetPassiveUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 100, passive.AntigravityQuota["claude"].Utilization)
	calls := modelCalls.Load()
	_, err = svc.getAntigravityUsage(context.Background(), latest, false)
	require.NoError(t, err)
	require.Equal(t, calls, modelCalls.Load(), "fresh post-refresh scope must hit cache")
	require.EqualValues(t, 1, refreshCalls.Load())
	// The old credential scope must not see the newly cached observation.
	require.Equal(t, antigravityObservationUnavailable, svc.getPassiveAntigravityUsage(account).AntigravityQuotaState)
}

func TestAntigravityQuotaTokenRejectsMismatchedSnapshot(t *testing.T) {
	account := antigravityIsolationAccount(901, "selected-token")
	other := antigravityIsolationAccount(902, "other-token")
	cache := newAntigravityIsolationCache()
	cache.tokens[AntigravityTokenCacheKey(account)] = "selected-token"
	provider := NewAntigravityTokenProvider(&quotaRefreshRepo{account: other}, cache, nil)
	token, snapshot, err := provider.GetAccessTokenForQuota(context.Background(), account)
	require.Error(t, err)
	require.Empty(t, token)
	require.Nil(t, snapshot)
	// A credential change after the cache read must not relabel the old token.
	provider.accountRepo = &quotaRefreshRepo{account: antigravityIsolationAccount(901, "changed-token")}
	token, snapshot, err = provider.GetAccessTokenForQuota(context.Background(), account)
	require.Error(t, err)
	require.Empty(t, token)
	require.Nil(t, snapshot)
}

func TestAntigravityQuotaRetentionBindsActualProject(t *testing.T) {
	for _, nextProject := range []string{"P1", "P2", "discovery-error"} {
		t.Run(nextProject, func(t *testing.T) {
			project, summaryFails := "P1", false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1internal:loadCodeAssist":
					if project == "discovery-error" {
						w.WriteHeader(403)
						return
					}
					_, _ = w.Write([]byte(`{"cloudaicompanionProject":"` + project + `"}`))
				case "/v1internal:fetchAvailableModels":
					var payload map[string]string
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					want := project
					if project == "discovery-error" {
						want = "P0"
					}
					require.Equal(t, want, payload["project"])
					_, _ = w.Write([]byte(`{"models":{}}`))
				case "/v1internal:retrieveUserQuotaSummary":
					if summaryFails {
						w.WriteHeader(503)
						return
					}
					_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.2}]}]}`))
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			}))
			defer server.Close()
			withAntigravityUsageBaseURL(t, server.URL)
			account := antigravityIsolationAccount(911, "synthetic")
			account.Credentials["project_id"] = "P0"
			svc := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil)}
			initial, err := svc.getAntigravityUsage(context.Background(), account, true)
			require.NoError(t, err)
			require.Len(t, initial.AntigravityWindows, 1)
			project, summaryFails = nextProject, true
			next, err := svc.getAntigravityUsage(context.Background(), account, true)
			require.NoError(t, err)
			if nextProject == "P1" {
				require.Len(t, next.AntigravityWindows, 1)
				require.True(t, next.AntigravityWindows["claude_5h"].Stale)
			} else {
				require.Empty(t, next.AntigravityWindows)
			}
			require.Equal(t, "P0", account.GetCredential("project_id"))
			passive := svc.getPassiveAntigravityUsage(account)
			require.Len(t, passive.AntigravityWindows, len(next.AntigravityWindows))
			for key, window := range next.AntigravityWindows {
				require.Equal(t, window, passive.AntigravityWindows[key])
			}
		})
	}
}

func TestAntigravityQuotaFailureAfterRotationDoesNotRetainOldWindows(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		for _, failure := range []string{"http", "network"} {
			t.Run(failure+map[bool]string{false: "-same-token", true: "-rotated-token"}[rotate], func(t *testing.T) {
				oldTransport := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = oldTransport })
				failing := false
				http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
					status, body := 200, `{}`
					switch {
					case r.URL.String() == antigravity.TokenURL:
						body = `{"access_token":"rotated-token","refresh_token":"rotated-refresh","expires_in":3600}`
					case strings.HasSuffix(r.URL.Path, ":loadCodeAssist"):
						body = `{"cloudaicompanionProject":"P1"}`
					case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
						if failing {
							if failure == "network" {
								return nil, errors.New("synthetic network failure")
							}
							status = 500
						} else {
							body = `{"models":{}}`
						}
					case strings.HasSuffix(r.URL.Path, ":retrieveUserQuotaSummary"):
						body = `{"groups":[{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.2}]}]}`
					default:
						t.Errorf("unexpected endpoint %s", r.URL.Path)
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
				})
				account := antigravityIsolationAccount(912, "original-token")
				account.Status = StatusActive
				account.Credentials["refresh_token"] = "original-refresh"
				repo := &quotaRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
				oauth := &AntigravityOAuthService{}
				provider := NewAntigravityTokenProvider(repo, nil, oauth)
				provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), NewAntigravityTokenRefresher(oauth))
				svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache(), antigravityQuotaFetcher: ProvideAntigravityQuotaFetcher(nil, nil, provider)}
				initial, err := svc.getAntigravityUsage(context.Background(), account, true)
				require.NoError(t, err)
				require.Len(t, initial.AntigravityWindows, 1)
				if rotate {
					repo.account.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
					account, err = repo.GetByID(context.Background(), account.ID)
					require.NoError(t, err)
				}
				failing = true
				next, err := svc.getAntigravityUsage(context.Background(), account, true)
				require.NoError(t, err)
				if rotate {
					require.Empty(t, next.AntigravityWindows)
				} else {
					require.Len(t, next.AntigravityWindows, 1)
					require.True(t, next.AntigravityWindows["claude_5h"].Stale)
				}
				passive, err := svc.GetPassiveUsage(context.Background(), account.ID)
				require.NoError(t, err)
				require.Len(t, passive.AntigravityWindows, len(next.AntigravityWindows))
				for key, window := range next.AntigravityWindows {
					require.Equal(t, window, passive.AntigravityWindows[key])
				}
			})
		}
	}
}
