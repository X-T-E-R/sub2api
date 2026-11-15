//go:build unit

package handler

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type content429Upstream struct {
	task88IdentityUpstream
	secondFails bool
}

func (s *content429Upstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	if account == 74 || s.secondFails && account == 73 {
		s.accounts = append(s.accounts, account)
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted"}}`))}, nil
	}
	return s.task88IdentityUpstream.Do(req, proxy, account, concurrency)
}

func TestMessagesContentSuspect429RecoversOrStopsBeforeThirdAccount(t *testing.T) {
	for _, secondFails := range []bool{false, true} {
		name := "second_account_recovers"
		if secondFails {
			name = "second_account_rejected"
		}
		t.Run(name, func(t *testing.T) {
			f, _, usage := newTask88IdentityFixture(t)
			third := *f.b
			third.ID = 72
			third.Priority = f.b.Priority + 1
			snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{f.a, f.b, &third}}, nil, nil, nil, nil)
			f.h.gatewayService = service.NewGatewayService(nil, &fakeGroupRepo{group: f.group}, usage, nil, nil, nil, nil,
				f.cache, f.h.cfg, snapshot, service.NewConcurrencyService(f.slots), service.NewBillingService(f.h.cfg, nil), nil,
				f.h.billingCacheService, nil, nil, service.NewDeferredService(nil, nil, 0), nil, nil, nil, f.digest, nil, nil, nil, nil, nil, nil, nil, nil)
			upstream := &content429Upstream{secondFails: secondFails}
			settings := service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}})
			f.h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil,
				service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil), nil, upstream, settings, nil, nil, nil)
			body := task88IdentityBody(t, task88AgentOne, 1)
			c, recorder := f.requestContext("/v1/messages", body)
			f.h.Messages(c)
			if secondFails {
				require.Equal(t, 429, recorder.Code, recorder.Body.String())
				require.Equal(t, []int64{74, 74, 73, 73}, upstream.accounts)
			} else {
				require.Equal(t, 200, recorder.Code, recorder.Body.String())
				require.Equal(t, []int64{74, 74, 73}, upstream.accounts)
			}
		})
	}
}
