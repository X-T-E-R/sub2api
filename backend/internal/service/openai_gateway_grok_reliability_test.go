//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type grokFreshRetryUpstream struct {
	regularCalls  int
	freshCalls    int
	regularBodies [][]byte
	freshBodies   [][]byte
	regularReq    *http.Request
	freshReq      *http.Request
	regularResp   *http.Response
	freshResp     *http.Response
	regularErr    error
	freshErr      error
}

func recordGrokReliabilityRequest(req *http.Request) []byte {
	if req == nil || req.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

func (u *grokFreshRetryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.regularCalls++
	u.regularReq = req
	u.regularBodies = append(u.regularBodies, recordGrokReliabilityRequest(req))
	return u.regularResp, u.regularErr
}

func (u *grokFreshRetryUpstream) DoFresh(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.freshCalls++
	u.freshReq = req
	u.freshBodies = append(u.freshBodies, recordGrokReliabilityRequest(req))
	return u.freshResp, u.freshErr
}

func (u *grokFreshRetryUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func grokReliabilityTestAccount() *Account {
	return &Account{
		ID:          8801,
		Name:        "grok-reliability",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-token", "base_url": "https://api.x.ai/v1"},
		Extra:       map[string]any{"openai_responses_supported": true},
	}
}

func grokReliabilityResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func TestStripGrokNonReplayableEncryptedInputPreservesPlaintextSummary(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction_summary","summary":[{"type":"summary_text","text":"compact plaintext"}],"encrypted_content":"compact-blob"},{"type":"reasoning","summary":[{"type":"summary_text","text":"reasoning plaintext"}],"encrypted_content":"reasoning-blob"},{"type":"message","role":"user","content":"continue"}]}`)

	got, changed, err := stripGrokNonReplayableEncryptedInput(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "reasoning", gjson.GetBytes(got, "input.0.type").String())
	require.Equal(t, "reasoning plaintext", gjson.GetBytes(got, "input.0.summary.0.text").String())
	require.False(t, gjson.GetBytes(got, "input.0.encrypted_content").Exists())
	require.Equal(t, "message", gjson.GetBytes(got, "input.1.type").String())
}

func TestGrokInvalidCompactionBlobResponseTriggersFallbackClassifier(t *testing.T) {
	body := []byte(`{"code":"invalid-argument","err":"Could not decode the compaction blob. Ensure it is unmodified from the compact response."}`)
	require.True(t, isGrokInvalidEncryptedContentResponse(http.StatusBadRequest, body))
}

func TestForwardGrokResponsesBareEOFRetriesOnceOnFreshConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &grokFreshRetryUpstream{
		regularErr: &url.Error{Op: "Post", URL: "https://cli-chat-proxy.grok.com/v1/responses", Err: io.EOF},
		freshResp: grokReliabilityResponse(http.StatusOK,
			`{"id":"resp_fresh","object":"response","model":"grok-4.5","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`),
	}
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{httpUpstream: upstream, accountRepo: repo}

	result, err := svc.forwardGrokResponses(context.Background(), c, grokReliabilityTestAccount(), body, "grok", false, time.Now())
	require.NoError(t, err)
	require.Equal(t, "resp_fresh", result.ResponseID)
	require.Equal(t, 1, upstream.regularCalls)
	require.Equal(t, 1, upstream.freshCalls)
	require.Equal(t, upstream.regularBodies[0], upstream.freshBodies[0])
	require.NotSame(t, upstream.regularReq, upstream.freshReq)
	require.Empty(t, repo.tempUnschedCalls)
}

func TestForwardGrokResponsesBareEOFFreshRetryExhaustionFailsOverWithoutEviction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	eofErr := &url.Error{Op: "Post", URL: "https://cli-chat-proxy.grok.com/v1/responses", Err: io.EOF}
	upstream := &grokFreshRetryUpstream{regularErr: eofErr, freshErr: eofErr}
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{httpUpstream: upstream, accountRepo: repo}

	result, err := svc.forwardGrokResponses(context.Background(), c, grokReliabilityTestAccount(), body, "grok", false, time.Now())
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Contains(t, failoverErr.TransportError, "EOF")
	require.Equal(t, 1, upstream.regularCalls)
	require.Equal(t, 1, upstream.freshCalls)
	require.Equal(t, upstream.regularBodies[0], upstream.freshBodies[0])
	require.Empty(t, repo.tempUnschedCalls)
}

func TestForwardGrokResponsesRealHTTP502DoesNotUseFreshTransportRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &grokFreshRetryUpstream{regularResp: grokReliabilityResponse(http.StatusBadGateway, `{"error":"real upstream 502"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	result, err := svc.forwardGrokResponses(context.Background(), c, grokReliabilityTestAccount(), body, "grok", false, time.Now())
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.TransportError)
	require.Equal(t, 1, upstream.regularCalls)
	require.Zero(t, upstream.freshCalls)
}

func TestForwardAsAnthropicGrokStripsConvertedEncryptedThinkingBeforeFirstRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"foreign-signature"},{"type":"text","text":"previous answer"}]},{"role":"user","content":"continue"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstream := &grokFreshRetryUpstream{regularResp: grokReliabilityResponse(http.StatusBadRequest, `{"code":"bad-request","error":"stop after first request"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: rawChatCompletionsTestConfig()}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, grokReliabilityTestAccount(), body, "", "")
	require.Error(t, err)
	require.Equal(t, 1, upstream.regularCalls)
	require.False(t, gjson.GetBytes(upstream.regularBodies[0], `input.#(type=="reasoning")`).Exists())
	require.Equal(t, "previous answer", gjson.GetBytes(upstream.regularBodies[0], `input.#(role=="assistant").content.0.text`).String())
}

func TestForwardAsAnthropicGrokBareEOFRetriesOnceOnFreshConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"grok","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstream := &grokFreshRetryUpstream{
		regularErr: &url.Error{Op: "Post", URL: "https://cli-chat-proxy.grok.com/v1/responses", Err: io.EOF},
		freshResp:  grokReliabilityResponse(http.StatusBadGateway, `{"error":"real upstream 502 after fresh retry"}`),
	}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: rawChatCompletionsTestConfig()}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, grokReliabilityTestAccount(), body, "", "")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 1, upstream.regularCalls)
	require.Equal(t, 1, upstream.freshCalls)
	require.Equal(t, upstream.regularBodies[0], upstream.freshBodies[0])
}

func TestForwardAsAnthropicNonGrokLeavesConvertedEncryptedThinkingUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"foreign-signature"}]},{"role":"user","content":"continue"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: grokReliabilityResponse(http.StatusBadRequest, `{"error":"stop after first request"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: rawChatCompletionsTestConfig()}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{"openai_responses_supported": true}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
	require.Error(t, err)
	require.Equal(t, "foreign-signature", gjson.GetBytes(upstream.lastBody, `input.#(type=="reasoning").encrypted_content`).String())
}

func TestGrokFreshConnectionRetryClassifierRejectsPersistentTransportErrors(t *testing.T) {
	require.False(t, isGrokFreshConnectionRetryableTransportError(errors.New("dial tcp: connect: connection refused")))
}

func TestSanitizeOpenAITransportErrorMessageRemovesCredentials(t *testing.T) {
	got := sanitizeOpenAITransportErrorMessage(`Post "https://user:secret@example.test/path?access_token=token-value": EOF Bearer bearer-value`)
	require.NotContains(t, got, "user:secret")
	require.NotContains(t, got, "token-value")
	require.NotContains(t, got, "bearer-value")
	require.Contains(t, got, "EOF")
}
