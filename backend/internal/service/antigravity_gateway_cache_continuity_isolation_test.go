package service

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Exercise the real admin caller while production Forward's observer initializer
// is held inside Once.Do. Copying that service would copy a locked Once and the
// admin Forward would never reach the stub transport, even after release.
func TestAntigravityCacheContinuityAccountTestDoesNotCopyInitializingOnce(t *testing.T) {
	effortTestSettings(t)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	productionObserver, _, productionEvents := continuityFixture(t)
	entered, release, initDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseInitializer := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseInitializer()
	go func() {
		defer close(initDone)
		gateway.cacheContinuityOnce.Do(func() {
			close(entered)
			<-release
			gateway.cacheContinuity = productionObserver
		})
	}()
	<-entered

	account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort:high}"})
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	c.Set("api_key", &APIKey{ID: 20, UserID: 1})
	c.Request.Header.Set("session_id", "offline-test-session")
	c.Set(accountTestPromptKey, "offline test prompt")
	service := &AccountTestService{antigravityGatewayService: gateway}
	done := make(chan error, 1)
	go func() { done <- service.testAntigravityAccountConnection(c, account, "gemini-custom") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("admin Forward blocked on production continuity initialization")
	}
	select {
	case <-initDone:
		t.Fatal("production initializer must still be held until admin completes")
	default:
	}
	require.Equal(t, 1, upstream.callCount)
	require.Contains(t, recorder.Body.String(), `"success":true`)
	require.Contains(t, recorder.Body.String(), `"text":"ok"`)
	require.Same(t, upstream, gateway.httpUpstream, "only the isolated service wraps transport")
	releaseInitializer()
	<-initDone
	require.Same(t, productionObserver, gateway.cacheContinuity)
	require.Empty(t, *productionEvents, "admin test must not contaminate production continuity state")
}
