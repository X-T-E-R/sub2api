//go:build unit

package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestModelQuotaRedirectDestinationValidation(t *testing.T) {
	for _, destination := range []string{
		"http://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com:444/v1internal:fetchAvailableModels",
		"https://cloudcode-pa.googleapis.com.untrusted.example/v1internal:fetchAvailableModels",
		"https://user@cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	} {
		t.Run(destination, func(t *testing.T) {
			calls := 0
			client := &Client{httpClient: &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != BaseURLs[0]+"/v1internal:fetchAvailableModels" && r.URL.String() != BaseURLs[1]+"/v1internal:fetchAvailableModels" {
					t.Fatalf("untrusted redirect reached transport: %s", r.URL)
				}
				return &http.Response{StatusCode: 307, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": {destination}}, Request: r}, nil
			})}}
			_, _, err := client.FetchAvailableModels(context.Background(), "synthetic", "", 4096)
			if err == nil || calls != len(BaseURLs) {
				t.Fatalf("untrusted redirect reached transport: calls=%d err=%v", calls, err)
			}
			parsed, _ := url.Parse(destination)
			if _, err := allowedModelQuotaBase(parsed); err == nil {
				t.Fatal("untrusted final response destination accepted")
			}
		})
	}
}

func TestQuotaSummaryRequestContract(t *testing.T) {
	for _, project := range []string{"shared-project", ""} {
		for _, token := range []string{"account-a", "account-b"} {
			client := &Client{httpClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				want := `{}`
				if project != "" {
					want = `{"project":"shared-project"}`
				}
				if req.Method != "POST" || req.URL.String() != BaseURLs[0]+"/v1internal:retrieveUserQuotaSummary" || string(body) != want {
					t.Fatalf("unexpected request %s %s %s", req.Method, req.URL, body)
				}
				if req.Header.Get("Authorization") != "Bearer "+token || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("User-Agent") == "" {
					t.Fatal("request headers do not match account")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"groups":[{"buckets":[{"bucketId":"3p-5h","remainingFraction":0,"resetTime":"2026-09-05T12:00:00Z"}]}]}`)), Header: make(http.Header)}, nil
			})}}
			result, err := client.RetrieveUserQuotaSummary(context.Background(), token, project, BaseURLs[0], 4096)
			if err != nil || len(result.Groups) != 1 || result.Groups[0].Buckets[0].RemainingFraction == nil || *result.Groups[0].Buckets[0].RemainingFraction != 0 {
				t.Fatalf("explicit zero missing: %+v %v", result, err)
			}
		}
	}
}

func TestQuotaSummaryRejectsFailuresAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		status         int
		limit          int64
		transportError bool
	}{
		{"oversize", strings.Repeat("x", 33), 200, 32, false},
		{"bad-json", `{`, 200, 32, false},
		{"missing-groups", `{}`, 200, 32, false},
		{"null", `null`, 200, 32, false},
		{"unauthorized", `{}`, 401, 32, false},
		{"forbidden", `{}`, 403, 32, false},
		{"unsupported", `{}`, 404, 32, false},
		{"redirect", `{}`, 307, 32, false},
		{"network", `{}`, 200, 32, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &Client{httpClient: &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if tc.transportError {
					return nil, errors.New("synthetic network error")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{"Location": {"https://untrusted.example/steal"}}, Request: req}, nil
			})}}
			result, err := client.RetrieveUserQuotaSummary(context.Background(), "synthetic-token", "", BaseURLs[0], tc.limit)
			if err == nil || result != nil || calls != 1 {
				t.Fatalf("expected bounded failure; got %v, calls %d", err, calls)
			}
			_, err = client.RetrieveUserQuotaSummary(context.Background(), "synthetic-token", "", "https://untrusted.example", tc.limit)
			if err == nil || calls != 1 {
				t.Fatal("unsupported base reached transport")
			}
			_, err = client.RetrieveUserQuotaSummary(context.Background(), "synthetic-token", "", BaseURLs[0], 0)
			if err == nil || calls != 1 {
				t.Fatal("invalid read limit reached transport")
			}
		})
	}
}
