package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAntigravityNativeEmptyStreamUsesSameAccountRecovery(t *testing.T) {
	for _, policyOn := range []bool{false, true} {
		for _, line := range []string{"", ": keepalive\n\n", "data: [DONE]\n\n", "data: null\n\n", "data: []\n\n"} {
			t.Run(fmt.Sprintf("A=%t/%q", policyOn, line), func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: policyOn}, upstream)
				body := []byte(`{"model":"client-alias","contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
				_, err := svc.ForwardGemini(context.Background(), c, antigravityThoughtAccount(nil), "gemini-3.1-pro-high", "streamGenerateContent", true, body, false)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, failover.RetryableOnSameAccount)
				require.Empty(t, recorder.Body.String(), "no fabricated successful empty stream")
				require.False(t, c.Writer.Written())
				require.Equal(t, 1, upstream.callCount)
			})
		}
	}
}

func TestAntigravityNativeEmptyStreamAfterOutputDoesNotReplay(t *testing.T) {
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", nil)
	_, err := c.Writer.WriteString(":\n\n")
	require.NoError(t, err)
	result, err := svc.handleGeminiStreamingResponse(c, antigravityThoughtStreamResponse(""), time.Now())
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "empty_stream_response")
}

func TestAntigravityNativeEmptyStreamCancellationDoesNotRetry(t *testing.T) {
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", nil)
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	result, err := svc.handleGeminiStreamingResponse(c, antigravityThoughtStreamResponse(""), time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.Empty(t, recorder.Body.String())
}
