package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestContentSuspect429AllowsOneAccountSwitch(t *testing.T) {
	fs := NewFailoverState(10, false)
	suspect := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, ContentSuspect429: true, ResponseBody: []byte(`{"error":{"message":"fixture"}}`)}
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), nil, 60, service.PlatformAntigravity, 3, suspect))
	require.Equal(t, 1, fs.SwitchCount)
	require.Contains(t, fs.FailedAccountIDs, int64(60))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), nil, 62, service.PlatformAntigravity, 3, suspect))
	require.Equal(t, 1, fs.SwitchCount, "a third account is never selected")
	require.Same(t, suspect, fs.LastFailoverErr, "original 429 mapping is retained")
}

func TestContentSuspect429CountsDifferentAccountsAndResetsOnOtherErrors(t *testing.T) {
	fs := NewFailoverState(10, false)
	suspect := &service.UpstreamFailoverError{StatusCode: 429, ContentSuspect429: true}
	ctx := context.Background()
	// No cross-account limit is spent by repeated reports from one account.
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, 60, "", 0, suspect))
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, 60, "", 0, suspect))
	require.Equal(t, 1, fs.contentSuspect429Count)
	// Genuine quota and server/auth failures break the consecutive-suspect chain.
	for _, status := range []int{429, 500, 502, 503, 401, 403} {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, int64(status), "", 0, &service.UpstreamFailoverError{StatusCode: status}))
		require.Zero(t, fs.contentSuspect429Count)
	}
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(ctx, nil, 62, "", 0, suspect))
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(ctx, nil, 63, "", 0, suspect))
}

func TestRealQuota429RetainsFullPoolFailover(t *testing.T) {
	fs := NewFailoverState(10, false)
	quota := &service.UpstreamFailoverError{StatusCode: 429}
	for id := int64(1); id <= 10; id++ {
		require.Equal(t, FailoverContinue, fs.HandleFailoverError(context.Background(), nil, id, "", 0, quota))
	}
	require.Equal(t, 10, fs.SwitchCount)
	require.Equal(t, FailoverExhausted, fs.HandleFailoverError(context.Background(), nil, 11, "", 0, quota))
}
