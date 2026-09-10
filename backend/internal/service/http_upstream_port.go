package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}

// HTTPUpstreamFreshConnection is an optional transport capability for retries
// that must not reuse any cached or idle connection. Keeping this separate from
// HTTPUpstream preserves existing callers and test doubles that do not need the
// stronger connection contract.
type HTTPUpstreamFreshConnection interface {
	DoFresh(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)
}

// HTTPUpstreamPoolEntryToken identifies the exact cached client entry used by
// an upstream response. Callers should treat the fields as opaque and pass the
// token back unchanged to HTTPUpstreamPoolReset.
//
// The cache key and generation are both required: a cache key can be reused
// after an entry is evicted, so the generation prevents a late reset from
// deleting a replacement entry.
type HTTPUpstreamPoolEntryToken struct {
	CacheKey   string
	Generation uint64
}

type httpUpstreamPoolEntryTokenContextKey struct{}

// WithHTTPUpstreamPoolEntryToken attaches a pool token to a request context.
// Repository implementations use this when they return a cached-client
// response; service callers can retrieve it from the response's Request.
func WithHTTPUpstreamPoolEntryToken(ctx context.Context, token HTTPUpstreamPoolEntryToken) context.Context {
	return context.WithValue(ctx, httpUpstreamPoolEntryTokenContextKey{}, token)
}

// HTTPUpstreamPoolEntryTokenFromRequest returns the token attached to req.
func HTTPUpstreamPoolEntryTokenFromRequest(req *http.Request) (HTTPUpstreamPoolEntryToken, bool) {
	if req == nil {
		return HTTPUpstreamPoolEntryToken{}, false
	}
	token, ok := req.Context().Value(httpUpstreamPoolEntryTokenContextKey{}).(HTTPUpstreamPoolEntryToken)
	return token, ok && token.CacheKey != "" && token.Generation != 0
}

// HTTPUpstreamPoolEntryTokenFromResponse returns the cached-entry token for a
// response produced by Do or DoWithTLS. Fresh one-shot requests have no token.
func HTTPUpstreamPoolEntryTokenFromResponse(resp *http.Response) (HTTPUpstreamPoolEntryToken, bool) {
	if resp == nil {
		return HTTPUpstreamPoolEntryToken{}, false
	}
	return HTTPUpstreamPoolEntryTokenFromRequest(resp.Request)
}

// HTTPUpstreamPoolReset is an optional capability for resetting one exact
// cached pool entry. Implementations close idle transport connections; active
// response streams remain untouched while their old entry drains.
type HTTPUpstreamPoolReset interface {
	ResetIdleConnectionPool(token HTTPUpstreamPoolEntryToken, cooldown time.Duration) bool
}
