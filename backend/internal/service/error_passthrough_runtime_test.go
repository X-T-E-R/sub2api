package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyErrorPassthroughRule_NoBoundService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusUnprocessableEntity,
		[]byte(`{"error":{"message":"invalid schema"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.False(t, matched)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "upstream_error", errType)
	assert.Equal(t, "Upstream request failed", errMsg)
}

func TestGatewayHandleErrorResponse_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 11, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Invalid schema for field messages", errField["message"])
}

func TestOpenAIHandleErrorResponse_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Invalid schema for field messages", errField["message"])
}

func TestOpenAIHandleErrorResponse_ContextWindow502KeepsMessageWithoutFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"upstream_error","code":null}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 14, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Your input exceeds the context window of this model. Please adjust your input and try again.", errField["message"])
}

func TestGeminiWriteGeminiMappedError_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	respBody := []byte(`{"error":{"code":422,"message":"Invalid schema for field messages","status":"INVALID_ARGUMENT"}}`)
	account := &Account{ID: 13, Platform: PlatformGemini, Type: AccountTypeAPIKey}

	err := svc.writeGeminiMappedError(c, account, http.StatusUnprocessableEntity, "req-2", respBody)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errField["type"])
	assert.Equal(t, "Invalid schema for field messages", errField["message"])
}

func TestGatewayHandleErrorResponse_AppliesRuleFor422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "上游请求失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.Equal(t, http.StatusTeapot, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "上游请求失败", errField["message"])
}

func TestOpenAIHandleErrorResponse_AppliesRuleFor422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "OpenAI上游失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusTeapot, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "OpenAI上游失败", errField["message"])
}

func TestGeminiWriteGeminiMappedError_AppliesRuleFor422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "Gemini上游失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GeminiMessagesCompatService{}
	respBody := []byte(`{"error":{"code":422,"message":"Invalid schema for field messages","status":"INVALID_ARGUMENT"}}`)
	account := &Account{ID: 3, Platform: PlatformGemini, Type: AccountTypeAPIKey}

	err := svc.writeGeminiMappedError(c, account, http.StatusUnprocessableEntity, "req-1", respBody)
	require.Error(t, err)
	assert.Equal(t, http.StatusTeapot, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Gemini上游失败", errField["message"])
}

func TestApplyErrorPassthroughRule_SkipMonitoringSetsContextKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	rule := newNonFailoverPassthroughRule(http.StatusBadRequest, "prompt is too long", http.StatusBadRequest, "上下文超限")
	rule.SkipMonitoring = true

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	_, _, _, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"prompt is too long"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.True(t, matched)
	v, exists := c.Get(OpsSkipPassthroughKey)
	assert.True(t, exists, "OpsSkipPassthroughKey should be set when skip_monitoring=true")
	boolVal, ok := v.(bool)
	assert.True(t, ok, "value should be bool")
	assert.True(t, boolVal)
}

func TestApplyErrorPassthroughRule_NoSkipMonitoringDoesNotSetContextKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	rule := newNonFailoverPassthroughRule(http.StatusBadRequest, "prompt is too long", http.StatusBadRequest, "上下文超限")
	rule.SkipMonitoring = false

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	_, _, _, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"prompt is too long"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.True(t, matched)
	_, exists := c.Get(OpsSkipPassthroughKey)
	assert.False(t, exists, "OpsSkipPassthroughKey should NOT be set when skip_monitoring=false")
}

// ---- ResponseCommittedKey: service 层写完错误响应后标记，handler 层检查跳过兜底写入 ----

func TestHandleErrorResponse_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"temperature: range: 0..1"}}`))),
		Header:     http.Header{},
	}
	account := &Account{ID: 100, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c), "non-failover error path must mark response committed")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
}

func TestHandleErrorResponse_PassthroughRuleSetsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{
		newNonFailoverPassthroughRule(http.StatusBadRequest, "temperature", http.StatusBadRequest, "参数错误"),
	})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"temperature: range: 0..1"}}`))),
		Header:     http.Header{},
	}
	account := &Account{ID: 200, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c), "passthrough rule path must mark response committed")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok, "payload[\"error\"] should be map[string]any")
	assert.Equal(t, "参数错误", errField["message"])
}

func TestOpenAIHandleErrorResponse_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"message":"rate limit exceeded"}}`))),
		Header:     http.Header{},
	}
	account := &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c), "OpenAI non-failover path must mark response committed")
}

func TestGeminiWriteGeminiMappedError_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	body := []byte(`{"error":{"message":"invalid field"}}`)
	account := &Account{ID: 102, Platform: PlatformGemini, Type: AccountTypeAPIKey}

	err := svc.writeGeminiMappedError(c, account, http.StatusBadRequest, "req-99", body)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c), "Gemini path must mark response committed")
	assert.Equal(t, "req-99", rec.Header().Get("x-request-id"))
}

func TestAntigravityWriteMappedClaudeError_DefaultPassthroughAcrossStatusesAndMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name           string
		upstreamStatus int
		upstreamReqID  string
		body           string
		wantStatus     int
		wantType       string
		wantCode       string
		wantMsg        string
	}{
		{
			name:           "400_non_whitelist_schema_error_preserved",
			upstreamStatus: http.StatusBadRequest,
			upstreamReqID:  "req-ag-400",
			body:           `{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"tool_choice\" at 'request': Cannot find field.","status":"INVALID_ARGUMENT"}}`,
			wantStatus:     http.StatusBadRequest,
			wantType:       "invalid_request_error",
			wantCode:       "400",
			wantMsg:        `Invalid JSON payload received. Unknown name "tool_choice" at 'request': Cannot find field.`,
		},
		{
			name:           "400_v1internal_wrapped_error_preserved",
			upstreamStatus: http.StatusBadRequest,
			upstreamReqID:  "req-ag-v1int",
			body:           `{"response":{"error":{"code":400,"message":"FunctionDeclaration parameters schema must be of type OBJECT","status":"INVALID_ARGUMENT"}}}`,
			wantStatus:     http.StatusBadRequest,
			wantType:       "invalid_request_error",
			wantCode:       "400",
			wantMsg:        "FunctionDeclaration parameters schema must be of type OBJECT",
		},
		{
			name:           "401_preserves_sanitized_reason",
			upstreamStatus: http.StatusUnauthorized,
			upstreamReqID:  "req-ag-401",
			body:           `{"error":{"code":401,"message":"Request had invalid authentication credentials. Bearer secret-jwt-token-value","status":"UNAUTHENTICATED"}}`,
			wantStatus:     http.StatusBadGateway,
			wantType:       "authentication_error",
			wantCode:       "401",
			wantMsg:        "Request had invalid authentication credentials. Bearer ***",
		},
		{
			name:           "403_preserves_reason",
			upstreamStatus: http.StatusForbidden,
			upstreamReqID:  "req-ag-403",
			body:           `{"error":{"code":403,"message":"Cloud Code Private API has not been used in project 123456 before or it is disabled.","status":"PERMISSION_DENIED"}}`,
			wantStatus:     http.StatusBadGateway,
			wantType:       "permission_error",
			wantCode:       "403",
			wantMsg:        "Cloud Code Private API has not been used in project 123456 before or it is disabled.",
		},
		{
			name:           "429_preserves_reason",
			upstreamStatus: http.StatusTooManyRequests,
			upstreamReqID:  "req-ag-429",
			body:           `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota) for model claude-sonnet-4-5.","status":"RESOURCE_EXHAUSTED"}}`,
			wantStatus:     http.StatusTooManyRequests,
			wantType:       "rate_limit_error",
			wantCode:       "429",
			wantMsg:        "Resource has been exhausted (e.g. check quota) for model claude-sonnet-4-5.",
		},
		{
			name:           "529_preserves_reason",
			upstreamStatus: 529,
			upstreamReqID:  "req-ag-529",
			body:           `{"error":{"code":529,"message":"Model capacity exhausted for region us-central1","status":"UNAVAILABLE"}}`,
			wantStatus:     http.StatusServiceUnavailable,
			wantType:       "overloaded_error",
			wantCode:       "529",
			wantMsg:        "Model capacity exhausted for region us-central1",
		},
		{
			name:           "500_preserves_reason_and_mapped_type",
			upstreamStatus: http.StatusInternalServerError,
			upstreamReqID:  "req-ag-500",
			body:           `{"error":{"code":500,"message":"Internal backend crash id=trace-9988","status":"INTERNAL"}}`,
			wantStatus:     http.StatusBadGateway,
			wantType:       "api_error",
			wantCode:       "500",
			wantMsg:        "Internal backend crash id=trace-9988",
		},
		{
			name:           "empty_body_falls_back_to_default",
			upstreamStatus: http.StatusBadRequest,
			upstreamReqID:  "req-ag-empty",
			body:           ``,
			wantStatus:     http.StatusBadRequest,
			wantType:       "invalid_request_error",
			wantMsg:        "Invalid request",
		},
		{
			name:           "html_body_falls_back_to_default_json",
			upstreamStatus: http.StatusBadGateway,
			upstreamReqID:  "req-ag-html",
			body:           `<html><head><title>502 Bad Gateway</title></head><body><h1>502 Bad Gateway</h1></body></html>`,
			wantStatus:     http.StatusBadGateway,
			wantType:       "upstream_error",
			wantMsg:        "Upstream request failed",
		},
	}

	svc := &AntigravityGatewayService{}
	account := &Account{ID: 77, Name: "ag-acc", Platform: PlatformAntigravity, Type: AccountTypeOAuth}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)

			err := svc.writeMappedClaudeError(c, account, tc.upstreamStatus, tc.upstreamReqID, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.upstreamReqID, rec.Header().Get("x-request-id"))

			var payload map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
			assert.Equal(t, "error", payload["type"])
			errField, ok := payload["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.wantType, errField["type"])
			assert.Equal(t, tc.wantMsg, errField["message"])
			if tc.wantCode != "" {
				assert.Equal(t, tc.wantCode, fmt.Sprint(errField["code"]))
			}
		})
	}
}

func TestAntigravityWriteMappedCompatError_PreservesFieldsAndSanitizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &AntigravityGatewayService{}
	account := &Account{ID: 78, Name: "ag-compat", Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	body := []byte(`{"error":{"code":"invalid_schema","param":"tools[0].function.parameters","type":"invalid_request_error","message":"Invalid schema at https://generativelanguage.googleapis.com/v1beta/models?key=AIzaSySecretKey12345678901234567890&alt=json"}}`)

	err := svc.writeMappedAntigravityCompatError(c, account, http.StatusBadRequest, "req-compat-1", body)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "req-compat-1", rec.Header().Get("x-request-id"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errField["type"])
	assert.Equal(t, "invalid_schema", errField["code"])
	assert.Equal(t, "tools[0].function.parameters", errField["param"])
	assert.Equal(t, "Invalid schema at https://generativelanguage.googleapis.com/v1beta/models?key=***&alt=json", errField["message"])
	assert.NotContains(t, rec.Body.String(), "AIzaSySecretKey")
}

func TestSanitizeUpstreamErrorMessageAndBody_RedactsCredentialsPreservesContext(t *testing.T) {
	raw := `Post "https://user:pass123@api.example.com/v1/messages?key=AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz123456&access_token=ya29.secret&model=claude-sonnet-4-5": Invalid token Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c or api_key sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345 or sk-proj-abcdefghijklmnopqrstuvwxyz012345 or xai-secretkey123456 for trace_id=req_987654321`
	sanitized := SanitizeUpstreamErrorMessage(raw)

	assert.NotContains(t, sanitized, "pass123")
	assert.NotContains(t, sanitized, "AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz123456")
	assert.NotContains(t, sanitized, "ya29.secret")
	assert.NotContains(t, sanitized, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c")
	assert.NotContains(t, sanitized, "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345")
	assert.NotContains(t, sanitized, "sk-proj-abcdefghijklmnopqrstuvwxyz012345")
	assert.NotContains(t, sanitized, "xai-secretkey123456")

	// Normal diagnostic fields and internal IDs must remain intact
	assert.Contains(t, sanitized, "api.example.com/v1/messages")
	assert.Contains(t, sanitized, "model=claude-sonnet-4-5")
	assert.Contains(t, sanitized, "trace_id=req_987654321")
}

func TestGatewayHandleErrorResponse_400HTMLBodyReturnsValidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(bytes.NewReader([]byte(`<html><body><h1>400 Bad Request</h1></body></html>`))),
		Header:     http.Header{"X-Request-Id": []string{"req-html-400"}},
	}
	account := &Account{ID: 105, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "req-html-400", rec.Header().Get("x-request-id"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "HTML upstream error body must not be written raw as invalid JSON")
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errField["type"])
	assert.Equal(t, "Invalid request", errField["message"])
}

func TestGeminiNativeUpstreamError_SanitizesJSONAndHandlesHTMLFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &GeminiMessagesCompatService{}
	account := &Account{ID: 106, Name: "gemini-native", Platform: PlatformGemini, Type: AccountTypeAPIKey}

	t.Run("same_protocol_json_preserves_details_and_sanitizes_key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		rawJSON := []byte(`{"error":{"code":400,"message":"Invalid argument at ?key=AIzaSySecretKey12345678901234567890","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"tools[0]","description":"unknown field"}]}]}}`)
		resp := &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}

		err := svc.writeGeminiNativeUpstreamError(c, account, resp, rawJSON, "req-gem-native-1", false)
		require.Error(t, err)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "req-gem-native-1", rec.Header().Get("x-request-id"))
		assert.NotContains(t, rec.Body.String(), "AIzaSySecretKey12345678901234567890")
		assert.Contains(t, rec.Body.String(), `"field":"tools[0]"`)
		assert.Contains(t, rec.Body.String(), `"INVALID_ARGUMENT"`)
	})

	t.Run("html_body_falls_back_to_google_json_error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		htmlBody := []byte(`<!DOCTYPE html><html><body>502 Bad Gateway</body></html>`)
		resp := &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
		}

		err := svc.writeGeminiNativeUpstreamError(c, account, resp, htmlBody, "req-gem-html", false)
		require.Error(t, err)
		assert.Equal(t, http.StatusBadGateway, rec.Code)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
		errField, ok := payload["error"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "Bad Gateway", errField["message"])
	})
}

func newNonFailoverPassthroughRule(statusCode int, keyword string, respCode int, customMessage string) *model.ErrorPassthroughRule {
	return &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "non-failover-rule",
		Enabled:         true,
		Priority:        1,
		ErrorCodes:      []int{statusCode},
		Keywords:        []string{keyword},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: false,
		ResponseCode:    &respCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
	}
}

func TestSameProtocolError_503And529PreserveUnknownFieldsDetailsParamAndLargeInt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("gateway_handleErrorResponse_529_preserves_unknown_fields_details_param_and_64bit_int", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		svc := &GatewayService{}
		rawBody := `{"type":"error","request_id":"req-body-529","error":{"type":"overloaded_error","code":"capacity_shed_529","message":"Cluster overloaded for key=AIzaSySecretKey12345678901234567890","param":"max_tokens","vendor_meta":{"cluster_id":9007199254740993,"region":"us-east-5"},"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_OVERLOADED","quota_id":18446744073709551615}]}}`
		resp := &http.Response{
			StatusCode: 529,
			Body:       io.NopCloser(bytes.NewReader([]byte(rawBody))),
			Header: http.Header{
				"Anthropic-Request-Id": []string{"anth-req-529"},
				"Traceparent":          []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			},
		}
		account := &Account{ID: 301, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

		_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
		require.Error(t, err)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "anth-req-529", rec.Header().Get("anthropic-request-id"))
		assert.Equal(t, "anth-req-529", rec.Header().Get("x-request-id"))
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", rec.Header().Get("traceparent"))
		assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", rec.Header().Get("x-trace-id"))

		wire := rec.Body.String()
		assert.NotContains(t, wire, "AIzaSySecretKey12345678901234567890")
		assert.Contains(t, wire, "Cluster overloaded for key=AIza***")
		// 64-bit integers beyond float64 precision must remain exact in JSON output
		assert.Contains(t, wire, `"cluster_id":9007199254740993`)
		assert.Contains(t, wire, `"quota_id":18446744073709551615`)
		assert.Equal(t, "max_tokens", gjson.Get(wire, "error.param").String())
		assert.Equal(t, "capacity_shed_529", gjson.Get(wire, "error.code").String())
		assert.Equal(t, "MODEL_OVERLOADED", gjson.Get(wire, "error.details.0.reason").String())
		assert.Equal(t, "us-east-5", gjson.Get(wire, "error.vendor_meta.region").String())
		assert.Equal(t, "req-body-529", gjson.Get(wire, "request_id").String())
	})

	t.Run("gateway_handleRetryExhaustedError_503_preserves_unknown_fields_details_param_and_64bit_int", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		svc := &GatewayService{}
		rawBody := `{"error":{"type":"service_unavailable","code":"upstream_503","message":"Backend shard 9007199254740993 unavailable","param":"thinking.budget_tokens","retry_token":9007199254740993,"details":[{"shard_id":9007199254740995,"retryable":true}]}}`
		resp := &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(bytes.NewReader([]byte(rawBody))),
			Header: http.Header{
				"X-Amzn-Requestid": []string{"amzn-req-503"},
				"X-B3-Traceid":     []string{"b3-trace-503"},
			},
		}
		account := &Account{ID: 302, Platform: PlatformAnthropic, Type: AccountTypeOAuth}

		_, err := svc.handleRetryExhaustedError(context.Background(), resp, c, account)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.False(t, errors.As(err, &failoverErr))
		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Equal(t, "amzn-req-503", rec.Header().Get("x-amzn-requestid"))
		assert.Equal(t, "amzn-req-503", rec.Header().Get("x-request-id"))
		assert.Equal(t, "b3-trace-503", rec.Header().Get("x-b3-traceid"))
		assert.Equal(t, "b3-trace-503", rec.Header().Get("x-trace-id"))

		wire := rec.Body.String()
		assert.Contains(t, wire, `"retry_token":9007199254740993`)
		assert.Contains(t, wire, `"shard_id":9007199254740995`)
		assert.Equal(t, "thinking.budget_tokens", gjson.Get(wire, "error.param").String())
		assert.Equal(t, "upstream_503", gjson.Get(wire, "error.code").String())
	})

	t.Run("openai_handleErrorResponse_503_preserves_unknown_fields_details_param_and_64bit_int", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		svc := &OpenAIGatewayService{}
		rawBody := `{"error":{"message":"Engine overloaded","type":"server_error","code":"engine_overloaded","param":"reasoning.effort","sequence_id":9007199254740993,"details":[{"node_id":9007199254740999,"reason":"CAPACITY"}]}}`
		resp := &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(bytes.NewReader([]byte(rawBody))),
			Header: http.Header{
				"Xai-Request-Id":       []string{"xai-req-503"},
				"X-Cloud-Trace-Context": []string{"105445aa7843bc8bf206b12000100000/1;o=1"},
			},
		}
		account := &Account{ID: 303, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
		require.Error(t, err)
		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Equal(t, "xai-req-503", rec.Header().Get("xai-request-id"))
		assert.Equal(t, "xai-req-503", rec.Header().Get("x-request-id"))
		assert.Equal(t, "105445aa7843bc8bf206b12000100000/1;o=1", rec.Header().Get("x-cloud-trace-context"))
		assert.Equal(t, "105445aa7843bc8bf206b12000100000/1;o=1", rec.Header().Get("x-trace-id"))

		wire := rec.Body.String()
		assert.Contains(t, wire, `"sequence_id":9007199254740993`)
		assert.Contains(t, wire, `"node_id":9007199254740999`)
		assert.Equal(t, "reasoning.effort", gjson.Get(wire, "error.param").String())
		assert.Equal(t, "engine_overloaded", gjson.Get(wire, "error.code").String())
		assert.Equal(t, "CAPACITY", gjson.Get(wire, "error.details.0.reason").String())
	})
}

func TestCrossProtocolAntigravity_PreservesResponseErrorDetailsParamCustomTypeAndRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &AntigravityGatewayService{}
	account := &Account{ID: 309, Name: "ag-cross", Platform: PlatformAntigravity, Type: AccountTypeOAuth}

	rawBody := []byte(`{"response":{"error":{"code":400,"type":"custom_tool_schema_error","status":"INVALID_ARGUMENT","message":"Invalid tool parameter at ?key=AIzaSySecretKey12345678901234567890","param":"tools[1].input_schema","requestId":"agy-req-9988","traceId":"agy-trace-7766","vendor_ext":{"trace_seq":9007199254740995},"details":[{"@type":"type.googleapis.com/google.rpc.BadRequest","violationId":9007199254740997,"fieldViolations":[{"field":"tools[1].input_schema","description":"unsupported keyword"}]}]}}}`)

	t.Run("writeMappedClaudeError_preserves_response_error_diagnostics", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		err := svc.writeMappedClaudeError(c, account, http.StatusBadRequest, "", rawBody)
		require.Error(t, err)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "agy-req-9988", rec.Header().Get("x-request-id"))
		assert.Equal(t, "agy-trace-7766", rec.Header().Get("x-trace-id"))

		wire := rec.Body.String()
		assert.NotContains(t, wire, "AIzaSySecretKey12345678901234567890")
		assert.Contains(t, wire, "Invalid tool parameter at ?key=***")
		assert.Equal(t, "error", gjson.Get(wire, "type").String())
		assert.Equal(t, "custom_tool_schema_error", gjson.Get(wire, "error.type").String())
		assert.Equal(t, "INVALID_ARGUMENT", gjson.Get(wire, "error.status").String())
		assert.Equal(t, "tools[1].input_schema", gjson.Get(wire, "error.param").String())
		assert.Equal(t, "agy-req-9988", gjson.Get(wire, "error.requestId").String())
		assert.Equal(t, "agy-trace-7766", gjson.Get(wire, "error.traceId").String())
		assert.Equal(t, "unsupported keyword", gjson.Get(wire, "error.details.0.fieldViolations.0.description").String())
		assert.Contains(t, wire, `"violationId":9007199254740997`)
		assert.Contains(t, wire, `"trace_seq":9007199254740995`)
	})

	t.Run("writeMappedAntigravityCompatError_preserves_response_error_diagnostics", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		err := svc.writeMappedAntigravityCompatError(c, account, http.StatusBadRequest, "", rawBody)
		require.Error(t, err)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "agy-req-9988", rec.Header().Get("x-request-id"))
		assert.Equal(t, "agy-trace-7766", rec.Header().Get("x-trace-id"))

		wire := rec.Body.String()
		assert.Equal(t, "custom_tool_schema_error", gjson.Get(wire, "error.type").String())
		assert.Equal(t, "tools[1].input_schema", gjson.Get(wire, "error.param").String())
		assert.Equal(t, "agy-req-9988", gjson.Get(wire, "error.requestId").String())
		assert.Equal(t, "agy-trace-7766", gjson.Get(wire, "error.traceId").String())
		assert.Contains(t, wire, `"violationId":9007199254740997`)
		assert.Contains(t, wire, `"trace_seq":9007199254740995`)
	})
}
