package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Upstream comments are transport activity, not downstream output. In the
// Messages conversion they are intentionally filtered before the client wire.
func TestAntigravityMessagesKeepaliveUsesDownstreamActivity(t *testing.T) {
	for _, comments := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream_comments=%v", comments), func(t *testing.T) {
			pr, pw := io.Pipe()
			t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
			response := antigravityCompatSuccessResponse()
			finalBody, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			response.Body = pr
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{response}}
			svc := newAntigravityCompatService(config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1,
			}, upstream)
			body := []byte(`{"model":"client-alias","stream":true,"messages":[{"role":"user","content":"question"}]}`)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			producer := make(chan error, 1)
			go func() {
				defer pw.Close()
				timer := time.NewTimer(1250 * time.Millisecond)
				defer timer.Stop()
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-timer.C:
						_, err := pw.Write(finalBody)
						producer <- err
						return
					case <-ticker.C:
						if comments {
							if _, err := io.WriteString(pw, ": upstream keepalive\n\n"); err != nil {
								producer <- err
								return
							}
						}
					}
				}
			}()
			result, err := svc.Forward(t.Context(), c, antigravityMessagesMappedTestAccount("gemini-4-argon"), body, false)
			require.NoError(t, <-producer)
			require.NoError(t, err)
			require.Equal(t, 1, upstream.callCount)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, "request-3757", recorder.Header().Get("X-Request-Id"))
			require.True(t, strings.HasPrefix(recorder.Body.String(), "event: ping\n"), "slow first token must remain live even when upstream comments are filtered")
			require.Contains(t, recorder.Body.String(), "event: message_stop")
			require.NotContains(t, recorder.Body.String(), "upstream keepalive")
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
		})
	}
}

func TestAntigravityMessagesSlowHeadersDoNotCommitSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			body := []byte(`{"model":"client-alias","stream":true,"messages":[{"role":"user","content":"question"}]}`)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			response := antigravityCompatSuccessResponse()
			if status != http.StatusOK {
				response.StatusCode = status
				response.Body = io.NopCloser(bytes.NewBufferString(`{"error":{"message":"fixture rejection"}}`))
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{response}, onCall: func(_ *http.Request, _ *queuedHTTPUpstreamStub) {
				time.Sleep(1100 * time.Millisecond) // longer than the configured heartbeat interval
				require.False(t, c.Writer.Written(), "unknown upstream status must remain uncommitted")
			}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1}, upstream)
			result, err := svc.Forward(t.Context(), c, antigravityMessagesMappedTestAccount("gemini-4-argon"), body, false)
			if status == http.StatusOK {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, status, recorder.Code)
			} else {
				var refusal *UpstreamFailoverError
				require.ErrorAs(t, err, &refusal)
				require.Nil(t, result)
				require.Equal(t, status, refusal.StatusCode)
				require.Contains(t, string(refusal.ResponseBody), "fixture rejection")
				require.False(t, c.Writer.Written(), "the handler still owns the HTTP refusal")
			}
			require.NotContains(t, recorder.Body.String(), "event: ping")
			require.Equal(t, 1, upstream.callCount)
		})
	}
}

func TestAntigravityMessagesCanceledBeforeHeadersDoesNotReplay(t *testing.T) {
	body := []byte(`{"model":"client-alias","stream":true,"messages":[{"role":"user","content":"question"}]}`)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	upstream := &queuedHTTPUpstreamStub{errors: []error{context.Canceled}, onCall: func(_ *http.Request, _ *queuedHTTPUpstreamStub) { cancel() }}
	svc := newAntigravityCompatService(config.GatewayConfig{StreamKeepaliveInterval: 1}, upstream)
	result, err := svc.Forward(ctx, c, antigravityMessagesMappedTestAccount("gemini-4-argon"), body, false)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, 1, upstream.callCount)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.JSONEq(t, `{"type":"error","error":{"type":"client_disconnected","message":"Client disconnected before upstream response"}}`, recorder.Body.String())
}
