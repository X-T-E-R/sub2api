package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

const wireSessionImageRequest = `{"model":"gemini-4-argon","max_tokens":256,"output_config":{"effort":"high"},"system":[{"type":"text","text":"stable system"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}]}`

func forwardWireSessionFixture(t *testing.T, body []byte, header, session string, key *APIKey, account *Account) antigravity.V1InternalRequest {
	t.Helper()
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: true}, upstream)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	if header != "" {
		c.Request.Header.Set(header, session)
	}
	if key != nil {
		c.Set("api_key", key)
	}
	_, err := svc.Forward(context.Background(), c, account, body, true)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.callCount)
	var wire antigravity.V1InternalRequest
	require.NoError(t, json.Unmarshal(upstream.requestBodies[0], &wire))
	return wire
}

func wireSessionFixtureAccount() *Account {
	account := antigravityThoughtAccount(nil)
	account.Credentials["model_mapping"] = map[string]any{"gemini-4-argon": "gemini-4-argon-high"}
	return account
}

func TestAntigravityWireSessionHeaderMultiRound(t *testing.T) {
	group := int64(8)
	key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
	account := wireSessionFixtureAccount()
	body := []byte(wireSessionImageRequest)
	first := forwardWireSessionFixture(t, body, "session_id", "image-conversation", key, account)
	for _, header := range clientSessionIDHeaders {
		next := forwardWireSessionFixture(t, body, header, "image-conversation", key, account)
		require.Equal(t, first.Request.SessionID, next.Request.SessionID, "explicit image-only session must survive header aliases and repeated conversion")
		require.Equal(t, first.Request, next.Request)
		require.NotEqual(t, first.RequestID, next.RequestID, "request identity stays per HTTP request")
	}

	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal(body, &req))
	req.Messages = append(req.Messages,
		antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"received-thinking"},{"type":"tool_use","id":"call-1","name":"lookup","signature":"received-tool","input":{"key":"value"}}]`)},
		antigravity.ClaudeMessage{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call-1","content":"result"}]`)})
	var previous = first
	for round := 0; round < 3; round++ {
		body, err := json.Marshal(req)
		require.NoError(t, err)
		next := forwardWireSessionFixture(t, body, "X-Session-Id", "image-conversation", key, account)
		require.Equal(t, first.Request.SessionID, next.Request.SessionID)
		require.Equal(t, "gemini-4-argon-high", next.Model)
		require.Equal(t, first.Request.SystemInstruction, next.Request.SystemInstruction)
		require.Equal(t, first.Request.Tools, next.Request.Tools)
		require.Equal(t, first.Request.GenerationConfig, next.Request.GenerationConfig)
		require.Equal(t, previous.Request.Contents, next.Request.Contents[:len(previous.Request.Contents)], "already-sent image/signed tool history stays a prefix")
		require.Equal(t, "received-thinking", next.Request.Contents[1].Parts[0].ThoughtSignature)
		require.Equal(t, "received-tool", next.Request.Contents[1].Parts[1].ThoughtSignature)
		require.Equal(t, "call-1", next.Request.Contents[1].Parts[1].FunctionCall.ID)
		require.Equal(t, "call-1", next.Request.Contents[2].Parts[0].FunctionResponse.ID)
		previous = next
		req.Messages = append(req.Messages,
			antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`"answer"`)},
			antigravity.ClaudeMessage{Role: "user", Content: json.RawMessage(`"next user"`)})
	}
}

func TestAntigravityWireSessionExplicitScopeIsolation(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		name := "header"
		body := []byte(wireSessionImageRequest)
		if metadata {
			name = "metadata"
			var req antigravity.ClaudeRequest
			require.NoError(t, json.Unmarshal(body, &req))
			req.Metadata = &antigravity.ClaudeMetadata{UserID: "session_123e4567-e89b-12d3-a456-426614174000"}
			var err error
			body, err = json.Marshal(req)
			require.NoError(t, err)
		}
		t.Run(name, func(t *testing.T) {
			group := int64(8)
			key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
			account := wireSessionFixtureAccount()
			wire := func(session string) string {
				return forwardWireSessionFixture(t, body, "session_id", session, key, account).Request.SessionID
			}
			base := wire("conversation")
			require.Equal(t, base, wire("conversation"))
			key.ID++
			require.NotEqual(t, base, wire("conversation"), "API key isolation")
			key.ID--
			group++
			require.NotEqual(t, base, wire("conversation"), "group isolation")
			group--
			account.ID++
			require.NotEqual(t, base, wire("conversation"), "account isolation")
			account.ID--
			if metadata {
				require.Equal(t, base, wire("ignored-header"), "same metadata priority as scheduler")
				var req antigravity.ClaudeRequest
				require.NoError(t, json.Unmarshal(body, &req))
				req.Metadata.UserID = "session_223e4567-e89b-12d3-a456-426614174000"
				var err error
				body, err = json.Marshal(req)
				require.NoError(t, err)
			}
			require.NotEqual(t, base, wire("another-conversation"), "session isolation")
		})
	}
}

func TestAntigravityWireSessionStructuredMetadataUsesConversationNotDevice(t *testing.T) {
	group := int64(8)
	key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
	account := wireSessionFixtureAccount()
	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(wireSessionImageRequest), &req))
	var first antigravity.V1InternalRequest
	for i, device := range []string{"device-a", "device-b"} {
		req.Metadata = &antigravity.ClaudeMetadata{UserID: `{"device_id":"` + device + `","session_id":"conversation"}`}
		body, err := json.Marshal(req)
		require.NoError(t, err)
		next := forwardWireSessionFixture(t, body, "", "", key, account)
		if i == 0 {
			first = next
		} else {
			require.Equal(t, first.Request, next.Request, "rotating non-session metadata must not change final wire identity or prompt")
		}
	}
}

func TestAntigravityWireSessionZeroOutputContinueKeepsPreparedIdentity(t *testing.T) {
	group := int64(8)
	key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
	account := wireSessionFixtureAccount()
	body := []byte(wireSessionImageRequest)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(antigravityEmptyStopLine),
		antigravityCompatSuccessResponse(),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: true}, upstream)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	c.Set("api_key", key)
	c.Request.Header.Set("session_id", "image-conversation")
	_, err := svc.Forward(context.Background(), c, account, body, true)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount)
	var first, continued antigravity.V1InternalRequest
	require.NoError(t, json.Unmarshal(upstream.requestBodies[0], &first))
	require.NoError(t, json.Unmarshal(upstream.requestBodies[1], &continued))
	require.Equal(t, first.RequestID, continued.RequestID)
	require.Equal(t, antigravityWireSessionID(c, account, &antigravity.ClaudeRequest{}), first.Request.SessionID)
	require.Equal(t, first.Request.SessionID, continued.Request.SessionID)
	require.Equal(t, first.Model, continued.Model)
	require.Equal(t, first.Request.SystemInstruction, continued.Request.SystemInstruction)
	require.Equal(t, first.Request.Tools, continued.Request.Tools)
	require.Equal(t, first.Request.GenerationConfig, continued.Request.GenerationConfig)
	require.Equal(t, first.Request.Contents[0].Parts[0], continued.Request.Contents[0].Parts[0])
	require.Equal(t, "Continue.", continued.Request.Contents[0].Parts[1].Text)
}

func TestAntigravityWireSessionSignatureRectifierKeepsPreparedIdentity(t *testing.T) {
	group := int64(8)
	key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
	account := wireSessionFixtureAccount()
	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(wireSessionImageRequest), &req))
	req.Thinking = &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 64}
	req.Messages = append(req.Messages,
		antigravity.ClaudeMessage{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"bad-signature"},{"type":"text","text":"answer"}]`)},
		antigravity.ClaudeMessage{Role: "user", Content: json.RawMessage(`"next user"`)})
	body, err := json.Marshal(req)
	require.NoError(t, err)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Corrupted thought signature."}}`))},
		antigravityCompatSuccessResponse(),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: true}, upstream)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
	c.Set("api_key", key)
	c.Request.Header.Set("session_id", "image-conversation")
	_, err = svc.Forward(context.Background(), c, account, body, true)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount)
	var first, retry antigravity.V1InternalRequest
	require.NoError(t, json.Unmarshal(upstream.requestBodies[0], &first))
	require.NoError(t, json.Unmarshal(upstream.requestBodies[1], &retry))
	require.Equal(t, first.Request.SessionID, retry.Request.SessionID)
	require.Equal(t, first.Model, retry.Model)
	require.Equal(t, first.Project, retry.Project)
	require.NotEqual(t, first.Request.Contents, retry.Request.Contents, "the existing signature rectifier actually ran")
}

func TestAntigravityWireSessionLegacyCallerContract(t *testing.T) {
	account := wireSessionFixtureAccount()
	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(wireSessionImageRequest), &req))
	req.Metadata = &antigravity.ClaudeMetadata{UserID: "legacy-user-id"}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	wire := forwardWireSessionFixture(t, body, "session_id", "header-without-auth", nil, account)
	require.Equal(t, "legacy-user-id", wire.Request.SessionID, "existing direct callers need no authenticated HTTP scope")
	group := int64(8)
	key := &APIKey{ID: 11, UserID: 1, GroupID: &group}
	wire = forwardWireSessionFixture(t, body, "", "", key, account)
	require.Equal(t, "legacy-user-id", wire.Request.SessionID, "arbitrary user ID alone is not upgraded to a conversation")
}
