package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- probe payload builders -------------------------------------------------

func TestCreateTestPayloadKeepsLegacyTextUnlessPromptGiven(t *testing.T) {
	legacy, err := createTestPayload("claude-sonnet-4-5")
	require.NoError(t, err)
	require.Equal(t, "hi", gjson.Get(marshalPayload(t, legacy), "messages.0.content.0.text").String(), "context-free callers keep the legacy probe text")

	for _, prompt := range []string{"", "   "} {
		blank, err := createTestPayload("claude-sonnet-4-5", prompt)
		require.NoError(t, err)
		require.Equal(t, "hi", gjson.Get(marshalPayload(t, blank), "messages.0.content.0.text").String())
	}

	custom, err := createTestPayload("claude-sonnet-4-5", "Say hello in one short sentence.")
	require.NoError(t, err)
	body := marshalPayload(t, custom)
	require.Equal(t, "Say hello in one short sentence.", gjson.Get(body, "messages.0.content.0.text").String())
	// The Claude Code envelope the upstream expects must survive the change.
	require.Equal(t, "ephemeral", gjson.Get(body, "messages.0.content.0.cache_control.type").String())
	require.Equal(t, "claude-sonnet-4-5", gjson.Get(body, "model").String())
	require.Equal(t, claudeCodeSystemPrompt, gjson.Get(body, "system.0.text").String())
	require.True(t, gjson.Get(body, "stream").Bool())
	require.Equal(t, int64(1024), gjson.Get(body, "max_tokens").Int())
	require.NotEmpty(t, gjson.Get(body, "metadata.user_id").String())
}

func TestCreateOpenAITestPayloadCarriesPromptAndOAuthStoreFlag(t *testing.T) {
	legacy := createOpenAITestPayload("gpt-5.4", false)
	require.Equal(t, "hi", gjson.Get(marshalPayload(t, legacy), "input.0.content.0.text").String())
	require.False(t, gjson.Get(marshalPayload(t, legacy), "store").Exists())

	custom := createOpenAITestPayload("gpt-5.4", true, "Answer with the single word: pong.")
	body := marshalPayload(t, custom)
	require.Equal(t, "Answer with the single word: pong.", gjson.Get(body, "input.0.content.0.text").String())
	require.Equal(t, "input_text", gjson.Get(body, "input.0.content.0.type").String())
	require.True(t, gjson.Get(body, "store").Exists(), "OAuth ChatGPT requests must carry store")
	require.False(t, gjson.Get(body, "store").Bool(), "OAuth ChatGPT requests require store: false")
	require.Equal(t, "gpt-5.4", gjson.Get(body, "model").String())
	require.NotEmpty(t, gjson.Get(body, "instructions").String())
	require.True(t, gjson.Get(body, "stream").Bool())
}

func TestGrokTestResponsesPayloadUsesPromptAndModel(t *testing.T) {
	body := marshalPayload(t, grokTestResponsesPayload("grok-4.6", "Say hello in one short sentence."))
	require.Equal(t, "grok-4.6", gjson.Get(body, "model").String())
	require.Equal(t, "Say hello in one short sentence.", gjson.Get(body, "input").String())
	require.True(t, gjson.Get(body, "stream").Bool())

	empty := marshalPayload(t, grokTestResponsesPayload("  ", ""))
	require.Equal(t, grokQuotaDefaultModel, gjson.Get(empty, "model").String())
	require.Equal(t, "hi", gjson.Get(empty, "input").String())
}

func marshalPayload(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return string(raw)
}

func TestAccountTestEffectivePromptFallsBackToTheBuiltInDefault(t *testing.T) {
	c, _ := newAccountTestPayloadContext("", "")
	require.Equal(t, DefaultModelTestPrompt, accountTestEffectivePrompt(c))

	// A media test publishes its own explicit prompt, including the empty one the
	// media branches resolve with their own defaults.
	c, _ = newAccountTestPayloadContext("Answer with the single word: pong.", "")
	require.Equal(t, "Answer with the single word: pong.", accountTestEffectivePrompt(c))
}

// --- Bedrock native response -------------------------------------------------

func TestBedrockNativeResponseText(t *testing.T) {
	text, err := bedrockNativeResponseText([]byte(`{"model":"anthropic.claude-sonnet-4-5-20250929-v1:0","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn"}`))
	require.NoError(t, err)
	require.Equal(t, "pong", text)

	text, err = bedrockNativeResponseText([]byte(`{"content":[{"type":"thinking","thinking":"let me think"},{"type":"text","text":"first"},{"type":"text","text":"second"}],"stop_reason":"end_turn"}`))
	require.NoError(t, err)
	require.Equal(t, "firstsecond", text, "thinking blocks are skipped, text blocks are joined")

	text, err = bedrockNativeResponseText([]byte(`{"content":[{"type":"text","text":"  pong  "}],"stop_reason":"end_turn"}`))
	require.NoError(t, err)
	require.Equal(t, "  pong  ", text, "the upstream text is reported verbatim")

	failures := []struct {
		name string
		body string
		want string
	}{
		{name: "empty content", body: `{"content":[],"stop_reason":"end_turn"}`, want: "no text content"},
		{name: "blank text block", body: `{"content":[{"type":"text","text":"   "}],"stop_reason":"end_turn"}`, want: "no text content"},
		{name: "thought only", body: `{"content":[{"type":"thinking","thinking":"hmm"}],"stop_reason":"end_turn"}`, want: "only thinking content"},
		{name: "missing stop_reason", body: `{"content":[{"type":"text","text":"pong"}]}`, want: "no stop_reason"},
		{name: "null stop_reason", body: `{"content":[{"type":"text","text":"pong"}],"stop_reason":null}`, want: "no stop_reason"},
		{name: "blank stop_reason", body: `{"content":[{"type":"text","text":"pong"}],"stop_reason":"  "}`, want: "no stop_reason"},
		{name: "not json", body: `not json`, want: "failed to parse response"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bedrockNativeResponseText([]byte(tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// --- Bedrock probe end to end ------------------------------------------------

type accountTestPayloadUpstream struct {
	responses []*http.Response
	requests  []*http.Request
}

func (u *accountTestPayloadUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, errors.New("unexpected Do call")
}

func (u *accountTestPayloadUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.requests = append(u.requests, req)
	if len(u.responses) == 0 {
		return nil, errors.New("no mocked response")
	}
	resp := u.responses[0]
	u.responses = u.responses[1:]
	return resp, nil
}

func newAccountTestPayloadContext(prompt, requestedModel string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/77/test", nil)
	if prompt != "" {
		c.Set(accountTestPromptKey, prompt)
	}
	if requestedModel != "" {
		c.Set(accountTestRequestedKey, requestedModel)
	}
	return c, rec
}

func bedrockAPIKeyTestAccount() *Account {
	return &Account{
		ID:          77,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeBedrock,
		Concurrency: 1,
		Credentials: map[string]any{
			"auth_mode":  "apikey",
			"api_key":    "bedrock-test-key",
			"aws_region": "us-east-1",
		},
	}
}

func bedrockTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestBedrockAccountTestSendsGlobalPromptAndReportsNativeMetadata(t *testing.T) {
	const (
		requestedModel              = "claude-sonnet-4-5"
		echoedModel                 = "anthropic.claude-sonnet-4-5-20250929-v1:0"
		echoedModelWithRegionPrefix = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	)
	c, rec := newAccountTestPayloadContext("Say hello in one short sentence.", requestedModel)
	upstream := &accountTestPayloadUpstream{responses: []*http.Response{bedrockTestResponse(http.StatusOK,
		`{"model":"`+echoedModel+`","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn"}`)}}
	svc := &AccountTestService{httpUpstream: upstream}

	require.NoError(t, svc.testBedrockAccountConnection(c, c.Request.Context(), bedrockAPIKeyTestAccount(), requestedModel))
	require.Len(t, upstream.requests, 1)

	body, err := io.ReadAll(upstream.requests[0].Body)
	require.NoError(t, err)
	require.Equal(t, "Say hello in one short sentence.", gjson.GetBytes(body, "messages.0.content.0.text").String(), "an ordinary Bedrock test must send the global prompt")
	require.False(t, gjson.GetBytes(body, "stream").Exists(), "the Bedrock invoke body is non-streaming")
	require.False(t, gjson.GetBytes(body, "output_config").Exists(), "Bedrock rejects output_config, so the probe must never send it")
	require.Contains(t, upstream.requests[0].URL.Path, "invoke")

	stream := rec.Body.String()
	require.Contains(t, stream, `"type":"test_start"`)
	require.Contains(t, stream, `"stream_mode":"native_non_stream"`, "a non-SSE probe must be labelled as such")
	require.Contains(t, stream, `"requested_model":"`+requestedModel+`"`)
	require.Contains(t, stream, `"upstream_sent_model":"`+echoedModelWithRegionPrefix+`"`, "the sent model is the resolved Bedrock model ID, not what the admin asked for")
	require.Contains(t, stream, `"effective_prompt":"Say hello in one short sentence."`)
	require.Contains(t, stream, `"upstream_response_model":"`+echoedModel+`"`, "the response model comes from the raw upstream body")
	// Conflict is optional on the wire: omitted means no conflict. Ordinary
	// events must not emit false and clear an earlier observed conflict.
	sawMetadata := false
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event TestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event.Type == "test_metadata" {
			sawMetadata = true
			require.Equal(t, echoedModel, event.UpstreamResponseModel)
			require.False(t, event.UpstreamResponseModelConflict)
		}
	}
	require.True(t, sawMetadata, "the raw upstream declaration must be reported")
	require.Contains(t, stream, "pong")
	require.Contains(t, stream, `"success":true`)
}

func TestBedrockAccountTestFailsInsteadOfReportingPlaceholderSuccess(t *testing.T) {
	failures := []struct {
		name string
		body string
		want string
	}{
		{name: "empty response", body: `{"content":[],"stop_reason":"end_turn"}`, want: "no text content"},
		{name: "thought only", body: `{"content":[{"type":"thinking","thinking":"hmm"}],"stop_reason":"end_turn"}`, want: "only thinking content"},
		{name: "incomplete response", body: `{"content":[{"type":"text","text":"pong"}]}`, want: "no stop_reason"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newAccountTestPayloadContext("Say hello in one short sentence.", "claude-sonnet-4-5")
			upstream := &accountTestPayloadUpstream{responses: []*http.Response{bedrockTestResponse(http.StatusOK, tc.body)}}
			svc := &AccountTestService{httpUpstream: upstream}

			err := svc.testBedrockAccountConnection(c, c.Request.Context(), bedrockAPIKeyTestAccount(), "claude-sonnet-4-5")
			require.ErrorContains(t, err, tc.want)

			stream := rec.Body.String()
			require.Contains(t, stream, `"type":"error"`)
			require.NotContains(t, stream, `"success":true`, "an HTTP 200 alone is not a passing probe")
			require.NotContains(t, stream, "(empty response)")
		})
	}
}
