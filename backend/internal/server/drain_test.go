package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDrainKeepsSSEAliveAndLivenessHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	drain := middleware.NewDrainState()
	r := gin.New()
	r.Use(middleware.ClientRequestID(), middleware.RequestLogger(), drain.Middleware())
	routes.RegisterCommonRoutes(r, drain)
	finish := make(chan struct{})
	r.GET("/v1/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: started\n\n")
		c.Writer.Flush()
		<-finish
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	})
	server := httptest.NewServer(r)
	defer server.Close()
	var finishOnce sync.Once
	finishStream := func() { finishOnce.Do(func() { close(finish) }) }
	defer finishStream()
	stream, err := http.Get(server.URL + "/v1/stream")
	require.NoError(t, err)
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: started\n", first)
	draining, count := drain.Snapshot()
	require.False(t, draining)
	require.EqualValues(t, 1, count)
	drain.BeginDrain()
	for _, check := range []struct {
		path   string
		status int
	}{{"/health", 200}, {"/readyz", 503}, {"/antigravity/v1/messages", 503}, {"/v1beta/models", 503}, {"/api/v1/admin/accounts", 503}} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+check.path, nil)
		request.Header.Set("X-Client-Request-ID", "drain.contract:1")
		response, requestErr := http.DefaultClient.Do(request)
		require.NoError(t, requestErr)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		require.Equal(t, check.status, response.StatusCode, "%s: %s", check.path, body)
		require.Equal(t, "drain.contract:1", response.Header.Get("X-Client-Request-ID"))
		if check.path == "/health" {
			require.JSONEq(t, `{"status":"ok","draining":true,"in_flight":1}`, string(body))
		}
	}
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stopped <- server.Config.Shutdown(ctx)
	}()
	select {
	case err := <-stopped:
		t.Fatalf("shutdown severed active SSE: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	finishStream()
	tail, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(tail), "data: [DONE]")
	require.NoError(t, <-stopped)
	draining, count = drain.Snapshot()
	require.True(t, draining)
	require.Zero(t, count)
}

func TestDrainAdmissionRace(t *testing.T) {
	drain := middleware.NewDrainState()
	r := gin.New()
	r.Use(drain.Middleware())
	r.GET("/v1/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/test", nil))
			}
		}()
	}
	drain.BeginDrain()
	wg.Wait()
	draining, count := drain.Snapshot()
	require.True(t, draining)
	require.Zero(t, count)
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/test", nil))
		require.Equal(t, 503, w.Code)
	}
	drain.EndDrain()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/test", nil))
	require.Equal(t, http.StatusNoContent, w.Code)
	draining, count = drain.Snapshot()
	require.False(t, draining)
	require.Zero(t, count)
}
