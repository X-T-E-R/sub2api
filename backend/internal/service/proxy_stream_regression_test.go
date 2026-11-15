package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type proxyReadErrorBody struct{ err error }

func (b proxyReadErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b proxyReadErrorBody) Close() error             { return nil }

func TestProxyCircuitDetachedClientCancellationAndReadErrors(t *testing.T) {
	for _, detach := range []string{"stream", "always", "nested"} {
		for _, outcome := range []string{"socket", "http2", "protocol", "canceled", "deadline"} {
			t.Run(detach+"/"+outcome, func(t *testing.T) {
				clientCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, cleanup := detachStreamUpstreamContext(clientCtx, true)
				defer cleanup()
				if detach == "always" {
					ctx, cleanup = detachUpstreamContext(clientCtx)
					defer cleanup()
				} else if detach == "nested" {
					ctx, cleanup = detachUpstreamContext(ctx)
					defer cleanup()
				}
				err := error(&net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF})
				switch outcome {
				case "http2":
					err = errors.New("http2: client connection lost")
				case "protocol":
					err = errors.New("provider stream parse error")
				case "canceled":
					cancel()
				case "deadline":
					err = context.DeadlineExceeded
				}
				require.NoError(t, ctx.Err(), "usage collection must remain detached")
				id := int64(7)
				account := &Account{ID: 1, ProxyID: &id}
				for _, boundary := range []string{"do", "body"} {
					circuit := testSharedProxyCircuit()
					if boundary == "do" {
						circuit.observeTransport(ctx, account, nil, err)
					} else {
						resp := &http.Response{StatusCode: 200, Body: proxyReadErrorBody{err: err}, Header: http.Header{"Content-Type": {"text/event-stream"}}}
						circuit.observeTransport(ctx, account, resp, nil)
						_, gotErr := io.ReadAll(resp.Body)
						require.ErrorIs(t, gotErr, err)
					}
					require.Equal(t, outcome == "socket" || outcome == "http2", circuit.quarantined(context.Background(), account), boundary)
				}
			})
		}
	}
}

func TestProxyCircuitAnthropicPassthroughProviderErrorsAreNeutral(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reader := range []string{"apikey", "transformed"} {
		for _, frame := range []string{"event: error\ndata: {\"error\":{\"type\":\"overloaded_error\"}}", "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}"} {
			t.Run(reader+"/"+frame[:10], func(t *testing.T) {
				circuit := testSharedProxyCircuit()
				id := int64(8)
				account := &Account{ID: 1, Platform: PlatformAnthropic, ProxyID: &id}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				payload := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"test\",\"usage\":{\"input_tokens\":12}}}\n\n" + frame + "\n\n"
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}
				circuit.observeTransport(c.Request.Context(), account, resp, nil)
				svc := &GatewayService{cfg: &config.Config{}, proxyStreamCircuit: circuit}
				if reader == "apikey" {
					_, _ = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(c.Request.Context(), resp, c, account, time.Now(), "test")
				} else {
					_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "test", "test", false)
				}
				require.Empty(t, circuit.entries, "semantic provider SSE errors must not count as disconnects")
			})
		}
	}
}

func TestProxyCircuitOpenAIStickySpilloverAndWaitFailOpen(t *testing.T) {
	for _, mode := range []string{"legacy", "legacy_batch", "advanced"} {
		for _, backupFree := range []bool{true, false} {
			for _, stickyID := range []int64{0, 1, 2} {
				if backupFree && stickyID == 2 {
					continue
				}
				t.Run(fmt.Sprintf("%s/backup_free_%t/sticky_%d", mode, backupFree, stickyID), func(t *testing.T) {
					bad, good := int64(7), int64(8)
					circuit := testSharedProxyCircuit()
					circuit.recordFailure(bad, time.Now())
					accounts := []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 1, ProxyID: &bad}, {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2, Concurrency: 1, ProxyID: &good}}
					cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
					if stickyID > 0 {
						cache.sessionBindings["openai:bound"] = stickyID
					}
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "legacy_batch"
					cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
					svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, openaiProxyStreamCircuit: circuit, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: true, 2: backupFree}})}
					svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService(fmt.Sprint(mode == "advanced"))
					selected, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "bound", "", nil, OpenAIUpstreamTransportAny, false)
					require.NoError(t, err)
					require.True(t, selected.Acquired, "free quarantined capacity must beat a saturated backup wait plan")
					want := int64(1)
					if backupFree {
						want = 2
					}
					require.Equal(t, want, selected.Account.ID)
					selected.ReleaseFunc()
					if stickyID == 1 {
						require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), nil, "bound", selected.Account.ID))
						require.EqualValues(t, 1, cache.sessionBindings["openai:bound"])
						entry := circuit.entries[bad]
						entry.blockedUntil = time.Now().Add(-time.Second)
						circuit.entries[bad] = entry
						selected, _, err = svc.SelectAccountWithScheduler(context.Background(), nil, "", "bound", "", nil, OpenAIUpstreamTransportAny, false)
						require.NoError(t, err)
						require.EqualValues(t, 1, selected.Account.ID, "expired quarantine must restore the original prompt cache")
						selected.ReleaseFunc()
					}
				})
			}
		}
	}
}

func TestProxyCircuitGatewayStickySpilloverAndWaitFailOpen(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, platform := range []string{PlatformAnthropic, PlatformAntigravity, PlatformGemini} {
			for _, backupFree := range []bool{true, false} {
				for _, stickyID := range []int64{0, 1, 2} {
					t.Run(fmt.Sprintf("%s/batch_%t/backup_free_%t/sticky_%d", platform, batch, backupFree, stickyID), func(t *testing.T) {
						bad, good := int64(7), int64(8)
						circuit := testSharedProxyCircuit()
						circuit.recordFailure(bad, time.Now())
						repo := &proxySelectionRepo{accounts: []Account{{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 1, ProxyID: &bad}, {ID: 2, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2, Concurrency: 1, ProxyID: &good}}}
						cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
						if stickyID > 0 {
							cache.sessionBindings["bound"] = stickyID
						}
						cfg := &config.Config{}
						cfg.Gateway.Scheduling.LoadBatchEnabled = batch
						cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
						svc := &GatewayService{accountRepo: repo, cache: cache, cfg: cfg, proxyStreamCircuit: circuit, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: true, 2: backupFree}})}
						ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, platform)
						selected, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "bound", "", nil, "", 0)
						require.NoError(t, err)
						require.True(t, selected.Acquired)
						want := int64(1)
						if backupFree {
							want = 2
						}
						require.Equal(t, want, selected.Account.ID)
						selected.ReleaseFunc()
						if stickyID == 1 {
							require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, nil, "bound", selected.Account.ID))
							require.EqualValues(t, 1, cache.sessionBindings["bound"])
							circuit.recordSuccess(bad)
							selected, err = svc.SelectAccountWithLoadAwareness(ctx, nil, "bound", "", nil, "", 0)
							require.NoError(t, err)
							require.EqualValues(t, 1, selected.Account.ID)
							selected.ReleaseFunc()
						}
					})
				}
			}
		}
	}
}

func TestProxyCircuitOpenAICleanEOFDoesNotQuarantine(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "responses", true: "passthrough"}[passthrough], func(t *testing.T) {
			id := int64(9)
			account := &Account{ID: 1, Platform: PlatformOpenAI, ProxyID: &id}
			circuit := testSharedProxyCircuit()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))}
			circuit.observeTransport(c.Request.Context(), account, resp, nil)
			svc := &OpenAIGatewayService{cfg: &config.Config{}, openaiProxyStreamCircuit: circuit}
			var err error
			if passthrough {
				_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "test", "test")
			} else {
				_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "test", "test")
			}
			require.Error(t, err, "missing terminal remains an upstream protocol failure")
			require.Empty(t, circuit.entries, "clean EOF must not quarantine the transport")
		})
	}
}

func TestProxyCircuitGeminiStickySpillover(t *testing.T) {
	id := int64(7)
	circuit := testSharedProxyCircuit()
	circuit.recordFailure(id, time.Now())
	repo := &proxySelectionRepo{accounts: []Account{{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, ProxyID: &id}, {ID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2}}}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"gemini:bound": 1}}
	svc := &GeminiMessagesCompatService{accountRepo: repo, cache: cache, cfg: &config.Config{}, proxyStreamCircuit: circuit}
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformGemini)
	selected, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "bound", "", nil)
	require.NoError(t, err)
	require.EqualValues(t, 2, selected.ID)
	require.EqualValues(t, 1, cache.sessionBindings["gemini:bound"])
	entry := circuit.entries[id]
	entry.blockedUntil = time.Now().Add(-time.Second)
	circuit.entries[id] = entry
	selected, err = svc.SelectAccountForModelWithExclusions(ctx, nil, "bound", "", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, selected.ID)
}

func TestProxyCircuitCanceledDetachedCompletionIsNeutral(t *testing.T) {
	id := int64(7)
	account := &Account{ID: 1, ProxyID: &id}
	circuit := testSharedProxyCircuit()
	circuit.recordFailure(id, time.Now())
	clientCtx, cancel := context.WithCancel(context.Background())
	ctx, cleanup := detachStreamUpstreamContext(clientCtx, true)
	defer cleanup()
	cancel()
	svc := &OpenAIGatewayService{openaiProxyStreamCircuit: circuit}
	svc.clearOpenAIProxyStreamDisconnect(ctx, account)
	require.True(t, circuit.quarantined(context.Background(), account), "a canceled stream must not erase another request's circuit observations")
	svc.clearOpenAIProxyStreamDisconnect(context.Background(), account)
	require.False(t, circuit.quarantined(context.Background(), account))
}

type proxySessionLimitCache struct {
	SessionLimitCache
	seen []string
}

func (c *proxySessionLimitCache) RegisterSession(_ context.Context, accountID int64, sessionID string, _ int, _ time.Duration) (bool, error) {
	c.seen = append(c.seen, sessionID)
	return accountID != 1, nil
}

func TestProxyCircuitFailOpenPreservesSessionAdmission(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			bad := int64(7)
			circuit := testSharedProxyCircuit()
			circuit.recordFailure(bad, time.Now())
			repo := &proxySelectionRepo{accounts: []Account{{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 1, ProxyID: &bad, Extra: map[string]any{"max_sessions": 1}}, {ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 0, Concurrency: 1}}}
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = batch
			cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
			sessions := &proxySessionLimitCache{}
			svc := &GatewayService{accountRepo: repo, cache: &schedulerTestGatewayCache{}, cfg: cfg, proxyStreamCircuit: circuit, sessionLimitCache: sessions, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: true, 2: false}})}
			ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformAnthropic)
			selected, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "real-session", "", nil, "", 0)
			require.NoError(t, err)
			require.False(t, selected.Acquired)
			require.EqualValues(t, 2, selected.WaitPlan.AccountID)
			require.Contains(t, sessions.seen, "real-session", "quarantine bypass must still check the real session identity")
		})
	}
}

func TestProxyCircuitAbandonedCapacityProbeKeepsOriginalSticky(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
		t.Run(platform, func(t *testing.T) {
			bad := int64(7)
			circuit := testSharedProxyCircuit()
			circuit.recordFailure(bad, time.Now())
			accounts := []Account{{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 1, ProxyID: &bad}, {ID: 2, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 2, Concurrency: 1}}
			key := "bound"
			if platform == PlatformOpenAI {
				key = "openai:bound"
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{key: 2}}
			concurrency := NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: false}})
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
			if platform == PlatformOpenAI {
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, openaiProxyStreamCircuit: circuit, concurrencyService: concurrency, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("false")}
				selected, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "bound", "", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.EqualValues(t, 2, selected.WaitPlan.AccountID)
				require.EqualValues(t, 2, cache.sessionBindings[key], "unacquired proxy probe must not migrate the conversation")
				require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), nil, "bound", 2))
			} else {
				svc := &GatewayService{accountRepo: &proxySelectionRepo{accounts: accounts}, cache: cache, cfg: cfg, proxyStreamCircuit: circuit, concurrencyService: concurrency}
				ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, platform)
				selected, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "bound", "", nil, "", 0)
				require.NoError(t, err)
				require.EqualValues(t, 2, selected.WaitPlan.AccountID)
				require.EqualValues(t, 2, cache.sessionBindings[key], "unacquired proxy probe must not migrate the conversation")
				require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, nil, "bound", 2))
			}
			require.EqualValues(t, 2, cache.sessionBindings[key], "the original wait's post-slot bind must still work")
		})
	}
}

func TestProxyCircuitFailOpenCreatesFirstStickyBinding(t *testing.T) {
	for _, mode := range []string{"openai_legacy", "openai_advanced", "anthropic"} {
		t.Run(mode, func(t *testing.T) {
			proxyID := int64(7)
			circuit := testSharedProxyCircuit()
			circuit.recordFailure(proxyID, time.Now())
			platform := PlatformOpenAI
			if mode == "anthropic" {
				platform = PlatformAnthropic
			}
			accounts := []Account{{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, ProxyID: &proxyID}}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
			concurrency := NewConcurrencyService(schedulerTestConcurrencyCache{})
			if platform == PlatformOpenAI {
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: &config.Config{}, openaiProxyStreamCircuit: circuit, concurrencyService: concurrency, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(fmt.Sprint(mode == "openai_advanced"))}
				selected, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "new-session", "", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.True(t, selected.Acquired)
				selected.ReleaseFunc()
				require.EqualValues(t, 1, cache.sessionBindings["openai:new-session"])
			} else {
				svc := &GatewayService{accountRepo: &proxySelectionRepo{accounts: accounts}, cache: cache, cfg: &config.Config{}, proxyStreamCircuit: circuit, concurrencyService: concurrency}
				ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, platform)
				selected, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "new-session", "", nil, "", 0)
				require.NoError(t, err)
				require.True(t, selected.Acquired)
				selected.ReleaseFunc()
				require.EqualValues(t, 1, cache.sessionBindings["new-session"])
			}
		})
	}
}
