//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

type completionBindingCache struct {
	*identityBindings
	requestCtx             context.Context
	postCompletionBinds    int
	postCompletionDeadline time.Duration
}

func (s *completionBindingCache) SetSessionAccountID(ctx context.Context, group int64, key string, account int64, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.requestCtx != nil && s.requestCtx.Err() != nil {
		s.postCompletionBinds++
		if deadline, ok := ctx.Deadline(); ok {
			s.postCompletionDeadline = time.Until(deadline)
		}
	}
	return s.identityBindings.SetSessionAccountID(ctx, group, key, account, ttl)
}

type terminalCancelWriter struct {
	gin.ResponseWriter
	cancel context.CancelFunc
}

func (w *terminalCancelWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if strings.Contains(string(p), "message_stop") {
		w.cancel()
	}
	return n, err
}

func (w *terminalCancelWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }

type completionFailoverUpstream struct {
	task88IdentityUpstream
	failFirstAccount bool
}

func (s *completionFailoverUpstream) Do(req *http.Request, proxy string, account int64, concurrency int) (*http.Response, error) {
	if s.failFirstAccount && account == 74 {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		s.bodies = append(s.bodies, body)
		s.accounts = append(s.accounts, account)
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-4-argon-high"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"15s"}]}}`))}, nil
	}
	return s.task88IdentityUpstream.Do(req, proxy, account, concurrency)
}

func (s *completionFailoverUpstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, account, concurrency)
}

func TestGatewayStickyCompletionSurvivesCancellationAndFailover(t *testing.T) {
	for _, failover := range []bool{false, true} {
		name := "refresh"
		if failover {
			name = "failover winner"
		}
		t.Run(name, func(t *testing.T) {
			f, _, usage := newTask88IdentityFixture(t)
			cache := &completionBindingCache{identityBindings: f.cache}
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{f.a, f.b}}, nil, nil, nil, nil)
			f.h.gatewayService = service.NewGatewayService(nil, &fakeGroupRepo{group: f.group}, usage, nil, nil, nil, nil,
				cache, f.h.cfg, snapshot, service.NewConcurrencyService(f.slots), service.NewBillingService(f.h.cfg, nil), nil,
				f.h.billingCacheService, nil, nil, service.NewDeferredService(nil, nil, 0), nil, nil, nil, f.digest, nil, nil, nil, nil, nil, nil, nil, nil)
			upstream := &completionFailoverUpstream{failFirstAccount: failover}
			settings := service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}})
			f.h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil,
				service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil), nil, upstream, settings, nil, nil)
			body, err := sjson.SetBytes(task88IdentityBody(t, task88AgentOne, 2), "stream", true)
			require.NoError(t, err)
			body, err = sjson.SetBytes(body, "thinking", map[string]any{"type": "enabled", "budget_tokens": 1024})
			require.NoError(t, err)
			c, recorder := f.requestContext("/v1/messages", body)
			parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), service.PlatformAnthropic)
			require.NoError(t, err)
			setGatewaySessionContext(c, f.apiKey, parsed)
			binding := identityBindingKey{group: f.group.ID, key: f.h.gatewayService.GenerateSessionHash(parsed)}
			cache.owners[binding] = 74
			requestCtx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			cache.requestCtx = requestCtx
			c.Request = c.Request.WithContext(requestCtx)
			c.Writer = &terminalCancelWriter{ResponseWriter: c.Writer, cancel: cancel}
			f.h.Messages(c)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.ErrorIs(t, requestCtx.Err(), context.Canceled)
			require.Equal(t, 1, cache.postCompletionBinds)
			require.Greater(t, cache.postCompletionDeadline, time.Duration(0))
			require.LessOrEqual(t, cache.postCompletionDeadline, 3*time.Second)
			winner := int64(74)
			if failover {
				winner = 73
			}
			require.Equal(t, winner, cache.owners[binding])
			require.Len(t, usage.created, 1)
			if failover {
				require.Equal(t, []int64{74, 73}, upstream.accounts)
				require.Equal(t, 1, *usage.created[0].AccountSwitchCount)
				require.NotContains(t, string(upstream.bodies[1]), "received-thinking")
				require.Contains(t, string(upstream.bodies[1]), antigravity.DummyThoughtSignature)
			}
		})
	}
}
