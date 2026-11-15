package service

import (
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	"github.com/gin-gonic/gin"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (resp *http.Response, transportErr error) {
	defer func() {
		s.getOpenAIProxyStreamCircuit().observeTransport(request.Context(), account, resp, transportErr)
	}()
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// doOpenAIStreamUpstream applies the opt-in preheader heartbeat around exactly
// one OpenAI transport attempt. The send closure intentionally delegates to
// doOpenAIUpstream so OAuth plugins retain ownership of the real transport.
// Callers must stop their normal error/retry/failover path when terminal is true.
func (s *OpenAIGatewayService) doOpenAIStreamUpstream(c *gin.Context, req *http.Request, proxyURL string, account *Account) (*http.Response, error, bool) {
	return requestStreamPreheader(c, req, account,
		func(request *http.Request) (*http.Response, error) {
			return s.doOpenAIUpstream(request, proxyURL, account)
		}, func(resp *http.Response, body []byte) []byte {
			return s.prepareOpenAIStreamUpstreamError(c, account, resp, body)
		})
}

// Redact before health observers, Ops or client framing consume the late error.
func (s *OpenAIGatewayService) prepareOpenAIStreamUpstreamError(c *gin.Context, account *Account, resp *http.Response, body []byte) []byte {
	body = s.redactAgentIdentitySensitiveBody(c.Request.Context(), account, body)
	if telemetryAttempt := codexTelemetryAttemptFromContext(c); telemetryAttempt != nil {
		telemetryAttempt.headers(resp.Header, codextelemetry.HTTPHeaders, resp.StatusCode)
	}
	ctx := c.Request.Context()
	model := ""
	if value, ok := c.Get(OpsUpstreamModelKey); ok {
		model, _ = value.(string)
	}
	if account.Platform == PlatformGrok {
		s.handleGrokAccountUpstreamError(withGrokTeamRateLimitModel(ctx, model), account, resp.StatusCode, resp.Header, body)
		if model != "" && shouldMarkGrokTeamModelRateLimit(resp.StatusCode, body) {
			markGrokTeamModelRateLimit(account, model, resolveGrokTeamRateLimitUntil(time.Now().Add(grokTeamRateLimitDefaultTTL), time.Now()))
		}
		return body
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, model)
	return body
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
