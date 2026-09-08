package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
)

type QuotaSummaryBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName,omitempty"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime,omitempty"`
}

type UserQuotaSummary struct {
	Groups []struct {
		Buckets []QuotaSummaryBucket `json:"buckets"`
	} `json:"groups"`
}

// RetrieveUserQuotaSummary reads optional explicit quota windows from the same
// endpoint that supplied model quotas. It never onboards or changes an account.
func (c *Client) RetrieveUserQuotaSummary(ctx context.Context, accessToken, projectID, baseURL string, bodyLimit int64) (*UserQuotaSummary, error) {
	if c == nil || c.httpClient == nil || bodyLimit <= 0 {
		return nil, errors.New("quota summary client or body limit is invalid")
	}
	if !slices.Contains(BaseURLs, baseURL) || (c.quotaBaseURL != "" && baseURL != c.quotaBaseURL) {
		return nil, errors.New("unsupported quota summary base URL")
	}
	payload := struct {
		Project string `json:"project,omitempty"`
	}{Project: projectID}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := NewAPIRequestWithURL(ctx, baseURL, "retrieveUserQuotaSummary", accessToken, body)
	if err != nil {
		return nil, err
	}
	client := c.fetchAvailableModelsHTTPClient()
	// Optional quota reads do not forward credentials or rewrite POST on redirects.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := servertiming.Do(client, req)
	if err != nil {
		return nil, fmt.Errorf("quota summary request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("quota summary read: %w", err)
	}
	if int64(len(data)) > bodyLimit {
		return nil, errors.New("quota summary response exceeds read limit")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota summary HTTP %d", resp.StatusCode)
	}
	var result UserQuotaSummary
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("quota summary decode: %w", err)
	}
	if result.Groups == nil {
		return nil, errors.New("quota summary groups missing")
	}
	return &result, nil
}
