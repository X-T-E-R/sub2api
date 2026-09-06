//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAntigravityQuotaOriginPolicy(t *testing.T) {
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	for _, tc := range []struct {
		plan, mode string
		daily      bool
	}{
		{"", "", false}, {"free", "", false}, {"pro", "", true}, {"ULTRA", "", true},
		{"pro", "prod", false}, {"pro", "unknown", false}, {"free", "daily", true}, {"free", "sandbox", true},
	} {
		t.Run(tc.plan+"/"+tc.mode, func(t *testing.T) {
			t.Setenv(antigravityForwardBaseURLEnv, tc.mode)
			var calls []string
			wantBase := antigravity.BaseURLs[0]
			if tc.daily {
				wantBase = antigravity.BaseURLs[1]
			}
			http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.String())
				body := `{"cloudaicompanionProject":"synthetic-project","paidTier":{"id":"g1-pro-tier"}}`
				switch {
				case strings.HasSuffix(r.URL.Path, ":fetchAvailableModels"):
					body = `{"models":{"synthetic-gemini":{"quotaInfo":{"remainingFraction":1}}}}`
					if r.URL.Host == strings.TrimPrefix(antigravity.BaseURLs[1], "https://") {
						body = `{"models":{"synthetic-gemini":{"quotaInfo":{"resetTime":"2030-01-01T00:00:00Z"}}}}`
					}
				case strings.HasSuffix(r.URL.Path, ":retrieveUserQuotaSummary"):
					fraction := 1
					if r.URL.Host == strings.TrimPrefix(antigravity.BaseURLs[1], "https://") {
						fraction = 0
					}
					body = fmt.Sprintf(`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":%d,"resetTime":"2030-01-01T00:00:00Z"}]}]}`, fraction)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})
			account := antigravityIsolationAccount(901, "synthetic-token")
			account.Credentials["plan_type"] = tc.plan
			result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), account, "")
			require.NoError(t, err)
			require.Equal(t, []string{wantBase + "/v1internal:loadCodeAssist", wantBase + "/v1internal:fetchAvailableModels", wantBase + "/v1internal:retrieveUserQuotaSummary"}, calls)
			if tc.daily {
				require.Zero(t, result.UsageInfo.AntigravityWindows["gemini_5h"].RemainingFraction)
				require.NotContains(t, result.UsageInfo.AntigravityQuota, "synthetic-gemini", "missing model fraction stays unknown")
			} else {
				require.Equal(t, 1.0, result.UsageInfo.AntigravityWindows["gemini_5h"].RemainingFraction)
			}
		})
	}
}

func TestAntigravityQuotaOriginFailureDoesNotFallback(t *testing.T) {
	for _, action := range []string{"loadCodeAssist", "fetchAvailableModels", "retrieveUserQuotaSummary"} {
		for _, status := range []int{503, 307} {
			t.Run(fmt.Sprintf("%s/%d", action, status), func(t *testing.T) {
				t.Setenv(antigravityForwardBaseURLEnv, "daily")
				var alternateCalls atomic.Int32
				alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { alternateCalls.Add(1); fmt.Fprint(w, `{}`) }))
				defer alternate.Close()
				chosen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, ":"+action) {
						w.Header().Set("Location", alternate.URL+r.URL.Path)
						w.WriteHeader(status)
						return
					}
					switch r.URL.Path {
					case "/v1internal:loadCodeAssist":
						fmt.Fprint(w, `{}`)
					case "/v1internal:fetchAvailableModels":
						fmt.Fprint(w, `{"models":{}}`)
					case "/v1internal:retrieveUserQuotaSummary":
						fmt.Fprint(w, `{"groups":[]}`)
					}
				}))
				defer chosen.Close()
				old := antigravity.BaseURLs
				antigravity.BaseURLs = []string{alternate.URL, chosen.URL}
				t.Cleanup(func() { antigravity.BaseURLs = old })
				result, err := NewAntigravityQuotaFetcher(nil, nil).FetchQuota(context.Background(), antigravityIsolationAccount(901, "synthetic"), "")
				if action == "fetchAvailableModels" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					require.Empty(t, result.UsageInfo.AntigravityWindows)
				}
				require.Zero(t, alternateCalls.Load())
			})
		}
	}
}

func TestAntigravityQuotaOriginInvalidatesObservations(t *testing.T) {
	t.Setenv(antigravityForwardBaseURLEnv, "prod")
	account := antigravityIsolationAccount(901, "synthetic")
	oldScope := antigravityQuotaScope(account)
	info := &UsageInfo{AntigravityWindows: map[string]*AntigravityQuotaWindow{"gemini_5h": {RemainingFraction: 1}}}
	svc := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil)}
	svc.cache.antigravityCache.Store(account.ID, &antigravityUsageCache{scope: oldScope, projectID: account.GetCredential("project_id"), usageInfo: info, timestamp: time.Now()})
	require.Len(t, svc.getPassiveAntigravityUsage(account).AntigravityWindows, 1)
	t.Setenv(antigravityForwardBaseURLEnv, "daily")
	require.NotEqual(t, oldScope, antigravityQuotaScope(account))
	require.Empty(t, svc.getPassiveAntigravityUsage(account).AntigravityWindows)
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	var calls atomic.Int32
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: r}, nil
	})
	usage, err := svc.getAntigravityUsage(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.AntigravityWindows, "old origin must not become stale fallback")
	require.EqualValues(t, 2, calls.Load(), "new origin must bypass fresh old-origin cache")
	account.Credentials["plan_type"] = "pro"
	t.Setenv(antigravityForwardBaseURLEnv, "")
	paidScope := antigravityQuotaScope(account)
	account.Credentials["plan_type"] = "free"
	require.NotEqual(t, paidScope, antigravityQuotaScope(account))
}

func TestAntigravityQuotaOriginSeparatesInflightRequests(t *testing.T) {
	t.Setenv(antigravityForwardBaseURLEnv, "")
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		daily := strings.HasPrefix(r.URL.String(), antigravity.BaseURLs[1]+"/")
		body := `{}`
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			if !daily {
				close(entered)
				<-release
			}
			body = `{"models":{}}`
		case "/v1internal:retrieveUserQuotaSummary":
			fraction := 1
			if daily {
				fraction = 0
			}
			body = fmt.Sprintf(`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":%d}]}]}`, fraction)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	free := antigravityIsolationAccount(901, "synthetic")
	paid := snapshotOAuthRefreshAccount(free)
	paid.Credentials["plan_type"] = "pro"
	svc := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: NewAntigravityQuotaFetcher(nil, nil)}
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); _, _ = svc.getAntigravityUsage(context.Background(), free, true) }()
	// Cleanup runs after release closes, keeping the transport alive until the old flight ends.
	t.Cleanup(func() { <-oldDone })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old origin request did not start")
	}
	newDone := make(chan *UsageInfo, 1)
	go func() { usage, _ := svc.getAntigravityUsage(context.Background(), paid, true); newDone <- usage }()
	select {
	case usage := <-newDone:
		require.NotNil(t, usage)
		require.Zero(t, usage.AntigravityWindows["gemini_5h"].RemainingFraction)
	case <-time.After(5 * time.Second):
		t.Fatal("new origin joined the old origin flight")
	}
}

func TestAntigravityQuotaOriginUsesTokenProviderSnapshot(t *testing.T) {
	t.Setenv(antigravityForwardBaseURLEnv, "")
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		require.True(t, strings.HasPrefix(r.URL.String(), antigravity.BaseURLs[1]+"/"))
		body := `{}`
		if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
			body = `{"models":{}}`
		}
		if strings.HasSuffix(r.URL.Path, ":retrieveUserQuotaSummary") {
			body = `{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0}]}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	old := antigravityIsolationAccount(901, "synthetic")
	latest := snapshotOAuthRefreshAccount(old)
	latest.Credentials["plan_type"] = "pro"
	repo := &quotaRefreshRepo{account: latest}
	provider := NewAntigravityTokenProvider(repo, nil, nil)
	svc := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: ProvideAntigravityQuotaFetcher(nil, nil, provider)}
	usage, err := svc.getAntigravityUsage(context.Background(), old, true)
	require.NoError(t, err)
	require.Zero(t, usage.AntigravityWindows["gemini_5h"].RemainingFraction)
	require.Empty(t, svc.getPassiveAntigravityUsage(old).AntigravityWindows)
	require.Len(t, svc.getPassiveAntigravityUsage(latest).AntigravityWindows, 1)
}

func TestAntigravityQuotaOriginReadmissionAfterSnapshotReplacement(t *testing.T) {
	t.Setenv(antigravityForwardBaseURLEnv, "")
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	http.DefaultTransport = quotaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		daily := strings.HasPrefix(r.URL.String(), antigravity.BaseURLs[1]+"/")
		body := `{}`
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			if daily {
				close(entered)
				<-release
			}
			body = `{"models":{}}`
		case "/v1internal:retrieveUserQuotaSummary":
			fraction := 1
			if daily {
				fraction = 0
			}
			body = fmt.Sprintf(`{"groups":[{"buckets":[{"bucketId":"gemini-5h","remainingFraction":%d}]}]}`, fraction)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	free := antigravityIsolationAccount(901, "synthetic")
	paid := snapshotOAuthRefreshAccount(free)
	paid.Credentials["plan_type"] = "pro"
	repo := &quotaRefreshRepo{account: paid}
	provider := NewAntigravityTokenProvider(repo, nil, nil)
	svc := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: ProvideAntigravityQuotaFetcher(nil, nil, provider)}
	firstDone, secondDone := make(chan *UsageInfo, 1), make(chan *UsageInfo, 1)
	go func() { usage, _ := svc.getAntigravityUsage(context.Background(), free, true); firstDone <- usage }()
	t.Cleanup(func() { <-firstDone })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("daily request did not start")
	}
	repo.mu.Lock()
	repo.account = snapshotOAuthRefreshAccount(free)
	repo.mu.Unlock()
	go func() { usage, _ := svc.getAntigravityUsage(context.Background(), free, true); secondDone <- usage }()
	select {
	case usage := <-secondDone:
		require.NotNil(t, usage)
		require.Equal(t, 1.0, usage.AntigravityWindows["gemini_5h"].RemainingFraction)
	case <-time.After(2 * time.Second):
		t.Cleanup(func() { <-secondDone })
		t.Fatal("free caller joined the daily flight admitted under its old free scope")
	}
}
