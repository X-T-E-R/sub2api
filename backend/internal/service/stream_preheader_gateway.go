package service

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

func (s *GatewayService) doStreamUpstream(c *gin.Context, req *http.Request, proxy string, account *Account, profile *tlsfingerprint.Profile, model string) (*http.Response, error, bool) {
	return requestStreamPreheader(c, req, account, func(request *http.Request) (*http.Response, error) {
		return s.httpUpstream.DoWithTLS(request, proxy, account.ID, account.Concurrency, profile)
	}, func(resp *http.Response, body []byte) []byte {
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(c.Request.Context(), account, resp.StatusCode, resp.Header, body, model)
		}
		return body
	})
}

func (s *GeminiMessagesCompatService) doStreamUpstream(c *gin.Context, req *http.Request, proxy string, account *Account, model string) (*http.Response, error, bool) {
	return requestStreamPreheader(c, req, account, func(request *http.Request) (*http.Response, error) {
		return s.httpUpstream.Do(request, proxy, account.ID, account.Concurrency)
	}, func(resp *http.Response, body []byte) []byte {
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(c.Request.Context(), account, resp.StatusCode, resp.Header, body, model)
		}
		return body
	})
}
