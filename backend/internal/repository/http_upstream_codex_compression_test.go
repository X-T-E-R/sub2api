package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

const codexCompressionTestURL = "https://chatgpt.com/backend-api/codex/responses"

func TestCodexRequestCompressionRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":"` + strings.Repeat("read code and run tests ", 500) + `","stream":true}`)
	req, err := http.NewRequest(http.MethodPost, codexCompressionTestURL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", "obsolete-length")
	req.TransferEncoding = []string{"chunked"}
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	t.Cleanup(decoder.Close)
	var calls int
	base := roundTripFunc(func(wire *http.Request) (*http.Response, error) {
		calls++
		require.NotSame(t, req, wire)
		require.Equal(t, "zstd", wire.Header.Get("Content-Encoding"))
		require.Equal(t, "application/json", wire.Header.Get("Content-Type"))
		require.Empty(t, wire.Header.Get("Content-Length"))
		require.Empty(t, wire.TransferEncoding)
		compressed, readErr := io.ReadAll(wire.Body)
		require.NoError(t, readErr)
		require.NoError(t, wire.Body.Close())
		require.Equal(t, int64(len(compressed)), wire.ContentLength)
		require.Less(t, len(compressed), len(body))
		decoded, decodeErr := decoder.DecodeAll(compressed, nil)
		require.NoError(t, decodeErr)
		require.Equal(t, body, decoded)
		replay, replayErr := wire.GetBody()
		require.NoError(t, replayErr)
		replayed, replayErr := io.ReadAll(replay)
		require.NoError(t, replayErr)
		require.NoError(t, replay.Close())
		require.Equal(t, compressed, replayed)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody, Request: wire}, nil
	})
	client := &http.Client{Transport: base}
	wrapped := httpClientForUpstreamRequest(client, req)
	require.NotSame(t, client, wrapped)
	resp, err := wrapped.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, calls)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.Equal(t, "obsolete-length", req.Header.Get("Content-Length"))
	require.Equal(t, int64(len(body)), req.ContentLength)
	original, err := req.GetBody()
	require.NoError(t, err)
	defer original.Close()
	originalBody, err := io.ReadAll(original)
	require.NoError(t, err)
	require.Equal(t, body, originalBody)
}

func TestCodexRequestCompressionScope(t *testing.T) {
	for _, test := range []struct{ name, method, target, encoding string }{
		{"apikey", http.MethodPost, "https://api.openai.com/v1/responses", ""},
		{"grok", http.MethodPost, "https://cli-chat-proxy.grok.com/v1/responses", ""},
		{"compact", http.MethodPost, codexCompressionTestURL + "/compact", ""},
		{"models", http.MethodGet, "https://chatgpt.com/backend-api/codex/models", ""},
		{"websocket", http.MethodGet, codexCompressionTestURL, ""},
		{"different-host", http.MethodPost, "https://chatgpt.com.example/backend-api/codex/responses", ""},
		{"existing-zstd", http.MethodPost, codexCompressionTestURL, "zstd"},
		{"existing-gzip", http.MethodPost, codexCompressionTestURL, "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(test.method, test.target, strings.NewReader("unchanged"))
			require.NoError(t, err)
			if test.encoding != "" {
				req.Header.Set("Content-Encoding", test.encoding)
			}
			require.False(t, isCodexRequestCompressionCandidate(req))
			client := &http.Client{}
			require.Same(t, client, httpClientForUpstreamRequest(client, req))
		})
	}
}

type codexCompressionErrorBody struct{ closed bool }

var errCodexCompressionRead = errors.New("test request read failed")

func (*codexCompressionErrorBody) Read([]byte) (int, error) { return 0, errCodexCompressionRead }
func (b *codexCompressionErrorBody) Close() error           { b.closed = true; return nil }

func TestCodexRequestCompressionReadFailureDoesNotSendOrRetry(t *testing.T) {
	body := &codexCompressionErrorBody{}
	req, err := http.NewRequest(http.MethodPost, codexCompressionTestURL, body)
	require.NoError(t, err)
	transport := &codexRequestCompressionTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("partial request must not reach upstream")
		return nil, nil
	})}
	_, err = transport.RoundTrip(req)
	require.ErrorIs(t, err, errCodexCompressionRead)
	require.True(t, body.closed)
	require.Empty(t, req.Header.Get("Content-Encoding"))
}

func TestCodexRequestCompressionCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	body := &codexCompressionErrorBody{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexCompressionTestURL, body)
	require.NoError(t, err)
	transport := &codexRequestCompressionTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled request must not reach upstream")
		return nil, nil
	})}
	_, err = transport.RoundTrip(req)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, body.closed)
}

func TestCodexRequestCompressionPreservesRedirectPolicy(t *testing.T) {
	req, err := http.NewRequestWithContext(service.WithHTTPUpstreamRedirectsDisabled(t.Context()),
		http.MethodPost, codexCompressionTestURL, strings.NewReader(`{}`))
	require.NoError(t, err)
	client := &http.Client{}
	wrapped := httpClientForUpstreamRequest(client, req)
	require.ErrorIs(t, wrapped.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	require.Nil(t, client.CheckRedirect)
	_, ok := wrapped.Transport.(*codexRequestCompressionTransport)
	require.True(t, ok)
}

func TestCodexRequestCompressionDoesNotRetryUpstreamFailure(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		req, err := http.NewRequest(http.MethodPost, codexCompressionTestURL, strings.NewReader(`{}`))
		require.NoError(t, err)
		calls := 0
		client := httpClientForUpstreamRequest(&http.Client{Transport: roundTripFunc(func(wire *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, "zstd", wire.Header.Get("Content-Encoding"))
			require.NoError(t, wire.Body.Close())
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody, Request: wire}, nil
		})}, req)
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, status, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 1, calls)
	}
}

func TestCodexRequestCompressionEncoderConcurrent(t *testing.T) {
	encoder, err := codexRequestEncoder()
	require.NoError(t, err)
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			body := bytes.Repeat([]byte{byte(i)}, 8192)
			compressed := encoder.EncodeAll(body, nil)
			decoder, decodeErr := zstd.NewReader(nil)
			if decodeErr != nil {
				t.Error(decodeErr)
				return
			}
			defer decoder.Close()
			decoded, decodeErr := decoder.DecodeAll(compressed, nil)
			if decodeErr != nil || !bytes.Equal(decoded, body) {
				t.Errorf("concurrent compression failed: %v", decodeErr)
			}
		})
	}
	workers.Wait()
}

type codexCompressionCloseTransport struct{ closed bool }

func (*codexCompressionCloseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (t *codexCompressionCloseTransport) CloseIdleConnections() { t.closed = true }

func TestCodexRequestCompressionPreservesConnectionCleanup(t *testing.T) {
	base := &codexCompressionCloseTransport{}
	req, err := http.NewRequest(http.MethodPost, codexCompressionTestURL, strings.NewReader(`{}`))
	require.NoError(t, err)
	client := httpClientForUpstreamRequest(&http.Client{Transport: base}, req)
	client = httpClientWithGrokAccessDeniedFallback(client)
	client.CloseIdleConnections()
	require.True(t, base.closed)
}

func TestHTTPUpstreamDoCompressesCodexRequestsOnWire(t *testing.T) {
	body := `{"model":"gpt-6-astra","instructions":"Read the files and fix the test","stream":true}`
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	t.Cleanup(decoder.Close)
	var observedBody []byte
	var observedEncoding string
	var observedLength int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedEncoding = r.Header.Get("Content-Encoding")
		observedLength = r.ContentLength
		observedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	t.Cleanup(server.Close)
	localURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	localTransport := server.Client().Transport
	svc := NewHTTPUpstream(nil).(*httpUpstreamService)
	const accountID int64 = 4001
	profile := service.HTTPUpstreamProfileOpenAI
	mode := svc.resolveProtocolMode(profile, directProxyKey, nil)
	settings := svc.applyProfilePoolSettings(svc.resolvePoolSettings(svc.getIsolationMode(), 1), profile)
	key := buildCacheKey(svc.getIsolationMode(), directProxyKey, accountID, mode)
	svc.clients[key] = &upstreamClientEntry{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			local := req.Clone(req.Context())
			local.URL.Host = localURL.Host
			return localTransport.RoundTrip(local)
		})},
		proxyKey: directProxyKey, poolKey: buildPoolKey(settings, mode), protocolMode: mode,
	}
	req, err := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(t.Context(), profile), http.MethodPost, codexCompressionTestURL, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := svc.Do(req, "", accountID, 1)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "zstd", observedEncoding)
	require.Equal(t, int64(len(observedBody)), observedLength)
	decoded, err := decoder.DecodeAll(observedBody, nil)
	require.NoError(t, err)
	require.Equal(t, body, string(decoded))
	require.Contains(t, string(responseBody), "response.completed")
	require.Zero(t, svc.clients[key].inFlight)
}
