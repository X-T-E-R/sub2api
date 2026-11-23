package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestAntigravity403ValidationURLPassthrough(t *testing.T) {
	effortTestSettings(t)

	validationURL := "https://accounts.google.com/v3/signin/continue?plt=test-plt-123&token=verify-token-xyz"
	raw403 := `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com","metadata":{"validation_url":"` + validationURL + `"}}]}}`

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(raw403)),
			},
		},
	}
	gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	svc := &AccountTestService{antigravityGatewayService: gateway}
	account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort:high}"})

	c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	c.Set(accountTestPromptKey, "hi")
	c.Set(accountTestRequestedKey, "gemini-custom")

	err := svc.testAntigravityAccountConnection(c, account, "gemini-custom")
	require.Error(t, err)

	_, errMsg := parseTestSSEOutput(recorder.Body.String())
	require.NotEmpty(t, errMsg)
	require.NotContains(t, errMsg, "(failover)")
	require.Contains(t, errMsg, "403")
	require.Contains(t, errMsg, "PERMISSION_DENIED")
	require.Contains(t, errMsg, "VALIDATION_REQUIRED")
	require.Contains(t, errMsg, "Verify your account to continue.")
	require.Contains(t, errMsg, "validation_url: "+validationURL)
	var eventValidationURL string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			if v := gjson.Get(strings.TrimPrefix(line, "data: "), "validation_url").String(); v != "" {
				eventValidationURL = v
			}
		}
	}
	require.Equal(t, validationURL, eventValidationURL)
}

func TestAccountTestAntigravity403AppealURLPassthrough(t *testing.T) {
	effortTestSettings(t)

	appealURL := "https://support.google.com/accounts/contact/disabled2?p=antigravity"
	raw403 := `{"error":{"code":403,"message":"Your account has been suspended.","status":"PERMISSION_DENIED","details":[{"reason":"ACCOUNT_SUSPENDED","metadata":{"appeal_url":"` + appealURL + `"}}]}}`

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(raw403)),
			},
		},
	}
	gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	svc := &AccountTestService{antigravityGatewayService: gateway}
	account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort:high}"})

	c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	c.Set(accountTestPromptKey, "hi")
	c.Set(accountTestRequestedKey, "gemini-custom")

	err := svc.testAntigravityAccountConnection(c, account, "gemini-custom")
	require.Error(t, err)

	_, errMsg := parseTestSSEOutput(recorder.Body.String())
	require.Contains(t, errMsg, "403")
	require.Contains(t, errMsg, "Your account has been suspended.")
	require.Contains(t, errMsg, "appeal_url: "+appealURL)
	var eventAppealURL string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			if v := gjson.Get(strings.TrimPrefix(line, "data: "), "appeal_url").String(); v != "" {
				eventAppealURL = v
			}
		}
	}
	require.Equal(t, appealURL, eventAppealURL)
}

func TestAccountTestForwardWriterPreservesNonSSEErrorBody(t *testing.T) {
	t.Run("structured json error", func(t *testing.T) {
		c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		writer := &accountTestForwardWriter{
			ResponseWriter: c.Writer,
			service:        &AccountTestService{},
			admin:          c,
			headers:        make(http.Header),
		}
		writer.WriteHeader(http.StatusForbidden)
		_, err := writer.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"Account requires verification"}}`))
		require.NoError(t, err)

		msg := writer.errorDetail().message
		require.NotEqual(t, "Forward returned HTTP 403", msg)
		require.Contains(t, msg, "Forward returned 403 (permission_error): Account requires verification")
	})

	t.Run("google 403 validation_url json", func(t *testing.T) {
		c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		writer := &accountTestForwardWriter{
			ResponseWriter: c.Writer,
			service:        &AccountTestService{},
			admin:          c,
			headers:        make(http.Header),
		}
		url := "https://accounts.google.com/v3/signin/continue?plt=xyz"
		writer.WriteHeader(http.StatusForbidden)
		_, err := writer.Write([]byte(`{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"` + url + `"}}]}}`))
		require.NoError(t, err)

		msg := writer.errorDetail().message
		require.Contains(t, msg, "Verify your account to continue.")
		require.Contains(t, msg, "validation_url: "+url)
	})

	t.Run("plain text fallback and empty fallback", func(t *testing.T) {
		c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		writer := &accountTestForwardWriter{
			ResponseWriter: c.Writer,
			service:        &AccountTestService{},
			admin:          c,
			headers:        make(http.Header),
		}
		writer.WriteHeader(http.StatusBadGateway)
		_, err := writer.Write([]byte("upstream proxy rejected connection"))
		require.NoError(t, err)
		require.Equal(t, "Forward returned 502: upstream proxy rejected connection", writer.errorDetail().message)

		emptyWriter := &accountTestForwardWriter{
			ResponseWriter: c.Writer,
			service:        &AccountTestService{},
			admin:          c,
			headers:        make(http.Header),
		}
		emptyWriter.WriteHeader(http.StatusForbidden)
		require.Equal(t, "Forward returned HTTP 403", emptyWriter.errorDetail().message)
	})
}

func TestAccountTestObservedUpstreamCapturesNon200ErrorBody(t *testing.T) {
	raw403 := `{"error":{"code":403,"message":"Project billing disabled","status":"PERMISSION_DENIED","details":[{"reason":"BILLING_DISABLED"}]}}`
	stub := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(raw403)),
			},
		},
	}
	c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	state := &accountTestRawStreamState{
		service:  &AccountTestService{},
		admin:    c,
		observer: &upstreamResponseModelObserver{},
	}
	observed := &accountTestObservedUpstream{HTTPUpstream: stub, state: state}

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1internal:streamGenerateContent", nil)
	resp, err := observed.Do(req, "", 1, 1)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	formatted := buildAntigravityAccountTestError(
		&UpstreamFailoverError{StatusCode: http.StatusForbidden},
		state,
		nil,
	).message
	require.NotContains(t, formatted, "(failover)")
	require.Contains(t, formatted, "Upstream returned 403 (PERMISSION_DENIED / BILLING_DISABLED): Project billing disabled")
}

func TestAccountTestErrorFormatting_RedactsCredentialsAndPreservesValidationURL(t *testing.T) {
	validationURL := "https://accounts.google.com/v3/signin/continue?plt=abc&token=keep-this-token"
	anthropicKey := "sk-" + "ant-api03-" + strings.Repeat("a", 24)
	googleKey := "AIza" + "Sy" + strings.Repeat("B", 33)
	body := `[{"error":{"code":403,"message":"Verify your account to continue. Bearer super-secret-jwt-123 ` + anthropicKey + ` ` + googleKey + ` https://alice:secretpass@proxy.example/path?api_key=leak","status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"` + strings.ReplaceAll(validationURL, "&", `\u0026`) + `"}}]}}]`

	formatted := formatAccountTestUpstreamError("API", http.StatusForbidden, []byte(body))
	require.Contains(t, formatted, "API returned 403 (PERMISSION_DENIED / VALIDATION_REQUIRED):")
	require.Contains(t, formatted, "Verify your account to continue.")
	require.Contains(t, formatted, "validation_url: "+validationURL)
	require.NotContains(t, formatted, "super-secret-jwt-123")
	require.NotContains(t, formatted, anthropicKey)
	require.NotContains(t, formatted, googleKey)
	require.NotContains(t, formatted, "secretpass")
	require.NotContains(t, formatted, "api_key=leak")
}

func TestAccountTestErrorFormatting_DoesNotReintroduceCredentialsFromProseFallbackOrQualifiers(t *testing.T) {
	t.Run("prose fallback URL with api_key and token is fully redacted", func(t *testing.T) {
		qualifierKey := "sk-" + "ant-api03-" + strings.Repeat("q", 24)
		body := `{"error":{"code":"` + qualifierKey + `","message":"Please verify credentials at https://api.example.com/v1/check?api_key=secret-key-123&token=secret-tok-456"}}`
		formatted := formatAccountTestUpstreamError("API", http.StatusForbidden, []byte(body))
		require.NotContains(t, formatted, "secret-key-123")
		require.NotContains(t, formatted, "secret-tok-456")
		require.NotContains(t, formatted, qualifierKey)
		require.Contains(t, formatted, "api_key=***")
		require.Contains(t, formatted, "token=***")
		require.Contains(t, formatted, "sk-ant-***")
	})

	t.Run("structured validation_url preserves trailing dot and verification token while redacting api_key", func(t *testing.T) {
		body := `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://accounts.google.com/v3/signin/continue?plt=abc.&token=verify-me&api_key=leaked-key"}}]}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.NotContains(t, detail.message, "leaked-key")
		require.Contains(t, detail.message, "plt=abc.")
		require.Contains(t, detail.message, "token=verify-me")
		require.Contains(t, detail.message, "api_key=***")

		// Verify SSE event exposes the exact validation_url verbatim including trailing dot.
		c, rec := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		_ = (&AccountTestService{}).sendFormattedErrorAndEnd(c, detail)
		var eventURL string
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				eventURL = gjson.Get(strings.TrimPrefix(line, "data: "), "validation_url").String()
			}
		}
		require.Equal(t, "https://accounts.google.com/v3/signin/continue?plt=abc.&token=verify-me&api_key=***", eventURL)
	})

	t.Run("prose URL never receives structured-link exemption when nested metadata also exists", func(t *testing.T) {
		body := `{"error":{"message":"Verify at https://api.example.com/prose?token=prose-secret-999"},"response":{"error":{"details":[{"metadata":{"validation_url":"https://accounts.google.com/v3/signin/continue?plt=nested-ok."}}]}}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.NotContains(t, detail.message, "prose-secret-999")
		require.Contains(t, detail.message, "token=***")
		require.Equal(t, "https://accounts.google.com/v3/signin/continue?plt=nested-ok.", detail.validationURL)
	})

	t.Run("structured validation_url still populates SSE field when repeated inside error.message", func(t *testing.T) {
		url := "https://accounts.google.com/v3/signin/continue?plt=repeat-dot."
		body := `{"error":{"code":403,"message":"Verify your account at ` + url + `","status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"` + url + `"}}]}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.Equal(t, url, detail.validationURL)
		require.Equal(t, 1, strings.Count(detail.message, url), "display text should deduplicate repeated URL")

		c, rec := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		_ = (&AccountTestService{}).sendFormattedErrorAndEnd(c, detail)
		var eventURL string
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				eventURL = gjson.Get(strings.TrimPrefix(line, "data: "), "validation_url").String()
			}
		}
		require.Equal(t, url, eventURL)
	})
}

type mixedRetryUpstreamStub struct {
	step int
}

func (u *mixedRetryUpstreamStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.step++
	if u.step == 1 {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stale transient 500"}}`)),
		}, nil
	}
	return nil, errors.New("dial tcp 192.0.2.10:443: connection reset by peer")
}

func (u *mixedRetryUpstreamStub) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestAccountTestObservedUpstreamClearsStaleHTTPBodyOnRetryTransportError(t *testing.T) {
	c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	state := &accountTestRawStreamState{
		service:  &AccountTestService{},
		admin:    c,
		observer: &upstreamResponseModelObserver{},
	}
	observed := &accountTestObservedUpstream{HTTPUpstream: &mixedRetryUpstreamStub{}, state: state}
	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1internal:streamGenerateContent", nil)

	// Attempt 1: HTTP 500 with body.
	resp1, err1 := observed.Do(req, "", 1, 1)
	require.NoError(t, err1)
	_, err := io.ReadAll(resp1.Body)
	require.NoError(t, err)
	require.NoError(t, resp1.Body.Close())

	// Attempt 2: transport failure must replace attempt 1's stale 500 body.
	resp2, err2 := observed.Do(req, "", 1, 1)
	require.Error(t, err2)
	require.Nil(t, resp2)

	formatted := buildAntigravityAccountTestError(
		errors.New("Upstream request failed after retries"),
		state,
		nil,
	).message
	require.NotContains(t, formatted, "stale transient 500")
	require.Contains(t, formatted, "connection reset by peer")
}

func TestAccountTestGeminiAndOpenAIImagePreserveUpstreamErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("gemini non-200 array error unescapes validation_url", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

		raw403 := `[{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://accounts.google.com/v3/signin/continue?a=1\u0026b=2"}}]}}]`
		upstream := &httpUpstreamRecorder{
			resp: &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(raw403)),
			},
		}
		svc := &AccountTestService{
			httpUpstream: upstream,
			cfg:          &config.Config{},
		}
		account := &Account{
			ID:          11,
			Name:        "gemini-key",
			Platform:    PlatformGemini,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "test-gemini-api-key"},
		}

		err := svc.testGeminiAccountConnection(c, account, "gemini-2.5-flash", "hi")
		require.Error(t, err)
		_, errMsg := parseTestSSEOutput(rec.Body.String())
		require.Contains(t, errMsg, "API returned 403 (PERMISSION_DENIED / VALIDATION_REQUIRED): Verify your account to continue.")
		require.Contains(t, errMsg, "validation_url: https://accounts.google.com/v3/signin/continue?a=1&b=2")
	})

	t.Run("openai image oauth preserves plain-text upstream body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

		upstream := &httpUpstreamRecorder{
			resp: &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader("upstream firewall blocked request")),
			},
		}
		svc := &AccountTestService{httpUpstream: upstream}
		account := &Account{
			ID:          12,
			Name:        "openai-oauth",
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"access_token": "tok"},
		}

		err := svc.testOpenAIImageOAuth(c, context.Background(), account, "gpt-image-2", "draw a cat")
		require.Error(t, err)
		_, errMsg := parseTestSSEOutput(rec.Body.String())
		require.Equal(t, "Responses API returned 403: upstream firewall blocked request", errMsg)
	})
}

func TestAccountTestErrorFormatting_ValidatesStructuredURLsAndSeparatesProseFallback(t *testing.T) {
	t.Run("rejects unsafe scheme and empty-host structured metadata URLs", func(t *testing.T) {
		body := `{"error":{"code":403,"message":"Verify your account.","details":[{"metadata":{"validation_url":"javascript:alert(1)"}},{"metadata":{"validation_url":"https://"}},{"metadata":{"appeal_url":"ftp://example.com/appeal"}}]}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.Empty(t, detail.validationURL)
		require.Empty(t, detail.appealURL)
		require.NotContains(t, detail.message, "javascript:alert(1)")
	})

	t.Run("skips invalid structured URL to valid http(s) entry", func(t *testing.T) {
		validURL := "https://accounts.google.com/v3/signin/continue?plt=valid-entry"
		body := `{"error":{"code":403,"message":"Verify your account.","details":[{"metadata":{"validation_url":"javascript:alert(1)"}},{"metadata":{"validation_url":"` + validURL + `"}}]}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.Equal(t, validURL, detail.validationURL)
	})

	t.Run("prose fallback regex match with trailing punctuation is never emitted as structured validationURL", func(t *testing.T) {
		body := `{"error":{"code":403,"message":"Please verify your account (https://accounts.google.com/v3/signin/continue?plt=prose123)."}}`
		detail := buildAccountTestUpstreamError("Upstream", http.StatusForbidden, []byte(body))
		require.Empty(t, detail.validationURL)
		require.Empty(t, detail.appealURL)
		require.Contains(t, detail.message, "https://accounts.google.com/v3/signin/continue?plt=prose123")
	})
}

func TestAccountTestSiblingStreamsAndCancelPreserveContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("openai stream abnormal terminal and failed events sanitize secrets and keep structured validation_url", func(t *testing.T) {
		validationURL := "https://accounts.google.com/v3/signin/continue?plt=openai-dot."
		secret := "sk-" + "ant-api03-" + strings.Repeat("x", 24)
		streamBody := strings.Join([]string{
			`data: {"type":"response.incomplete","response":{"error":{"message":"Verify account ` + secret + `","details":[{"metadata":{"validation_url":"` + validationURL + `"}}]}}}`,
			"",
		}, "\n")

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

		err := (&AccountTestService{}).processOpenAIStream(c, strings.NewReader(streamBody))
		require.Error(t, err)
		_, errMsg := parseTestSSEOutput(rec.Body.String())
		require.Contains(t, errMsg, "OpenAI stream ended incomplete before response.completed")
		require.NotContains(t, errMsg, secret)
		require.Contains(t, errMsg, "validation_url: "+validationURL)
		var eventURL string
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				if v := gjson.Get(strings.TrimPrefix(line, "data: "), "validation_url").String(); v != "" {
					eventURL = v
				}
			}
		}
		require.Equal(t, validationURL, eventURL)
	})

	t.Run("openai chat completions inline error keeps structured validation_url", func(t *testing.T) {
		validationURL := "https://accounts.google.com/v3/signin/continue?plt=chat-dot."
		streamBody := `data: {"error":{"message":"Verify account","details":[{"metadata":{"validation_url":"` + validationURL + `"}}]}}` + "\n\n"

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

		err := (&AccountTestService{}).processOpenAIChatCompletionsStream(c, strings.NewReader(streamBody))
		require.Error(t, err)
		var eventURL string
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			if strings.HasPrefix(line, "data: ") {
				if v := gjson.Get(strings.TrimPrefix(line, "data: "), "validation_url").String(); v != "" {
					eventURL = v
				}
			}
		}
		require.Equal(t, validationURL, eventURL)
	})

	t.Run("grok video missing request_id sanitizes echoed secrets", func(t *testing.T) {
		secret := "sk-" + "ant-api03-" + strings.Repeat("v", 24)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

		upstream := &httpUpstreamRecorder{
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad token ` + secret + `"}}`)),
			},
		}
		svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
		account := &Account{ID: 15, Platform: PlatformGrok, Type: AccountTypeAPIKey}

		err := svc.testGrokVideoGeneration(c, context.Background(), account, "tok", "grok-imagine-video", "a wave", AccountTestOptions{})
		require.Error(t, err)
		_, errMsg := parseTestSSEOutput(rec.Body.String())
		require.Contains(t, errMsg, "Grok video create response missing request_id")
		require.NotContains(t, errMsg, secret)
	})

	t.Run("raw stream state clears stale failureDetail when later failure overwrites failure", func(t *testing.T) {
		c, _ := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		state := &accountTestRawStreamState{
			service:  &AccountTestService{},
			admin:    c,
			observer: &upstreamResponseModelObserver{},
		}
		state.observe([]byte(`data: {"error":{"message":"verify","details":[{"metadata":{"validation_url":"https://accounts.google.com/v3/signin/continue?plt=stale"}}]}}`+"\n"), nil)
		require.NotEmpty(t, state.failureDetail.validationURL)

		state.observe(nil, errors.New("unexpected EOF"))
		require.Empty(t, state.failureDetail.validationURL)
		require.Empty(t, state.failureDetail.appealURL)
		require.ErrorContains(t, state.failure, "unexpected EOF")
	})

	t.Run("antigravity test reports cancellation instead of stale retry error when context is canceled", func(t *testing.T) {
		effortTestSettings(t)

		ctx, cancel := context.WithCancel(context.Background())
		var attempt int
		upstream := &cancelAfterFirst500UpstreamStub{
			onDo: func() (*http.Response, error) {
				attempt++
				if attempt == 1 {
					cancel()
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stale retry 500"}}`)),
					}, nil
				}
				return nil, context.Canceled
			},
		}
		gateway := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		svc := &AccountTestService{antigravityGatewayService: gateway}
		account := effortTestAccount(map[string]string{"gemini-custom": "gemini-custom-{effort:high}"})

		c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
		c.Request = c.Request.WithContext(ctx)
		c.Set(accountTestPromptKey, "hi")
		c.Set(accountTestRequestedKey, "gemini-custom")

		err := svc.testAntigravityAccountConnection(c, account, "gemini-custom")
		require.Error(t, err)
		_, errMsg := parseTestSSEOutput(recorder.Body.String())
		require.Contains(t, errMsg, "Test canceled:")
		require.NotContains(t, errMsg, "stale retry 500")
	})
}

type cancelAfterFirst500UpstreamStub struct {
	onDo func() (*http.Response, error)
}

func (u *cancelAfterFirst500UpstreamStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.onDo()
}

func (u *cancelAfterFirst500UpstreamStub) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}
