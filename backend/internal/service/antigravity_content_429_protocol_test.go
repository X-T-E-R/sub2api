package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAntigravityContentSuspect429AllWireProtocols(t *testing.T) {
	for _, protocol := range []string{"gemini", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				hardeningErrorResponse(429, nil, antigravityBare429Fixture),
				hardeningErrorResponse(429, nil, antigravityBare429Fixture),
			}}
			svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
			account := antigravityThoughtAccount(nil)
			var err error
			switch protocol {
			case "gemini":
				body := []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)
				c, _ := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.1-pro-high:generateContent", body)
				_, err = svc.ForwardGemini(context.Background(), c, account, "gemini-3.1-pro-high", "generateContent", false, body, false)
			case "chat":
				body := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"fixture"}]}`)
				c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			case "responses":
				body := []byte(`{"model":"client-alias","input":"fixture"}`)
				c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/responses", body)
				_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
			}
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.ContentSuspect429)
			require.Equal(t, 429, failover.StatusCode)
			require.Equal(t, 2, upstream.callCount)
		})
	}
}
