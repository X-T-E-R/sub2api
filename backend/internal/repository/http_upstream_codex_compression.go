package repository

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var codexRequestEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithEncoderCRC(false),
		zstd.WithZeroFrames(true),
	)
})

func isCodexRequestCompressionCandidate(req *http.Request) bool {
	// Native Codex compresses HTTP Responses, not compact, models or WS.
	return req != nil && req.URL != nil && req.Method == http.MethodPost &&
		strings.EqualFold(req.URL.Scheme, "https") && strings.EqualFold(req.URL.Hostname(), "chatgpt.com") &&
		req.URL.Path == "/backend-api/codex/responses" &&
		req.Body != nil && req.Body != http.NoBody && strings.TrimSpace(req.Header.Get("Content-Encoding")) == ""
}

type codexRequestCompressionTransport struct {
	base http.RoundTripper
}

func (t *codexRequestCompressionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCodexRequestCompressionCandidate(req) {
		return t.base.RoundTrip(req)
	}
	// RoundTripper owns the input body even when preparation fails. Keep the
	// caller's JSON headers/GetBody intact for protocol repair and HTTP retries.
	defer req.Body.Close()
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	encoder, err := codexRequestEncoder()
	if err != nil {
		return nil, fmt.Errorf("create codex request compressor: %w", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read codex request for compression: %w", err)
	}
	compressed := encoder.EncodeAll(body, nil)
	wire := req.Clone(req.Context())
	wire.Body = io.NopCloser(bytes.NewReader(compressed))
	wire.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	wire.ContentLength = int64(len(compressed))
	wire.TransferEncoding = nil
	wire.Header.Del("Content-Length")
	wire.Header.Set("Content-Encoding", "zstd")
	return t.base.RoundTrip(wire)
}

func (t *codexRequestCompressionTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
