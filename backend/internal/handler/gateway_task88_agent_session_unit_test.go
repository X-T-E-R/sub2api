//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Task88's producer contract: kimi_code + agent_session uses an explicit
// session_<agent UUID> on the wire, not the registry's unadorned UUID.
const task88AgentOne = "session_11111111-1111-4111-8111-111111111111"
const task88AgentTwo = "session_22222222-2222-4222-8222-222222222222"

type task88IdentityUpstream struct {
	bodies   [][]byte
	accounts []int64
}

func (s *task88IdentityUpstream) Do(req *http.Request, _ string, account int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.bodies = append(s.bodies, body)
	s.accounts = append(s.accounts, account)
	return handlerThoughtStream(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":80,"candidatesTokenCount":3}}}`), nil
}

func (s *task88IdentityUpstream) DoWithTLS(req *http.Request, proxy string, account int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, account, concurrency)
}

func newTask88IdentityFixture(t *testing.T) (*identityFixture, *task88IdentityUpstream, *thoughtUsageLogRepoStub) {
	t.Helper()
	f := newIdentityFixture(t)
	f.group.Platform = service.PlatformAntigravity
	for _, account := range []*service.Account{f.a, f.b} {
		account.Platform, account.Type = service.PlatformAntigravity, service.AccountTypeOAuth
		account.Credentials = map[string]any{
			"access_token": "offline-token", "project_id": "project-" + strconv.FormatInt(account.ID, 10),
			"model_mapping": map[string]any{"gemini-4-argon": "gemini-4-argon-high"},
		}
		account.Extra = map[string]any{
			service.AntigravitySuppressThoughtOnlyStopExtraKey: true,
			service.AntigravityContinueThoughtOnlyExtraKey:     false,
		}
	}
	settings := service.NewSettingService(handlerThoughtSettingRepo{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}})
	previous := service.ResolveAntigravityThoughtPolicy(context.Background(), nil)
	t.Cleanup(func() {
		require.NoError(t, settings.SetAntigravityThoughtPolicySettings(context.Background(), service.AntigravityThoughtPolicySettings(previous)))
	})
	require.NoError(t, settings.SetAntigravityThoughtPolicySettings(context.Background(), service.AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true, ContinueZeroOutput: true}))
	upstream := &task88IdentityUpstream{}
	f.h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil,
		service.NewAntigravityTokenProvider(nil, handlerThoughtTokenCache{}, nil), nil, upstream, settings, nil, nil)
	usage := &thoughtUsageLogRepoStub{}
	snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{f.a, f.b}}, nil, nil, nil, nil)
	f.h.gatewayService = service.NewGatewayService(nil, &fakeGroupRepo{group: f.group}, usage, nil, nil, nil, nil,
		f.cache, f.h.cfg, snapshot, service.NewConcurrencyService(f.slots), service.NewBillingService(f.h.cfg, nil), nil,
		f.h.billingCacheService, nil, nil, service.NewDeferredService(nil, nil, 0), nil, nil, nil, f.digest, nil, nil, nil, nil, nil, nil, nil, nil)
	return f, upstream, usage
}

func task88IdentityBody(t *testing.T, session string, round int) []byte {
	t.Helper()
	messages := []any{map[string]any{"role": "user", "content": []any{map[string]any{
		"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aW1hZ2U="},
	}}}}
	if round >= 2 {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan", "signature": "received-thinking"},
				map[string]any{"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{"key": "value"}, "signature": "received-tool"},
			}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "result"}}})
	}
	if round >= 3 {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "user", "content": "next user"})
	}
	body, err := json.Marshal(map[string]any{
		"model": "gemini-4-argon", "max_tokens": 256, "stream": false,
		"output_config": map[string]any{"effort": "high"}, "metadata": map[string]any{"user_id": session},
		"system": "stable system", "messages": messages,
		"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}}}},
	})
	require.NoError(t, err)
	return body
}

func TestGatewayTask88AgentSessionParseScheduleForward(t *testing.T) {
	f, upstream, usage := newTask88IdentityFixture(t)
	forward := func(session string, round int) (identityBindingKey, antigravity.V1InternalRequest) {
		t.Helper()
		body := task88IdentityBody(t, session, round)
		parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), service.PlatformAnthropic)
		require.NoError(t, err)
		require.Equal(t, session, parsed.MetadataUserID)
		require.True(t, parsed.HasExplicitSessionIdentity(), "producer's explicit agent session must not fall back to prompt contents")
		c, recorder := f.requestContext("/v1/messages", body)
		setGatewaySessionContext(c, f.apiKey, parsed)
		expectedHash := f.h.gatewayService.GenerateSessionHash(parsed)
		lookupStart, calls := len(f.cache.lookups), len(upstream.bodies)
		f.h.Messages(c)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, calls+1, len(upstream.bodies), "one real Forward through the stub transport; no continuation or replay")
		require.Equal(t, int64(20), gjson.GetBytes(recorder.Body.Bytes(), "usage.input_tokens").Int())
		require.Equal(t, int64(80), gjson.GetBytes(recorder.Body.Bytes(), "usage.cache_read_input_tokens").Int())
		require.Greater(t, len(f.cache.lookups), lookupStart)
		binding := f.cache.lookups[lookupStart]
		require.Equal(t, expectedHash, binding.key, "actual handler/selector must consume the parsed explicit identity")
		var wire antigravity.V1InternalRequest
		require.NoError(t, json.Unmarshal(upstream.bodies[calls], &wire))
		require.Equal(t, "gemini-4-argon-high", wire.Model)
		require.True(t, strings.HasPrefix(wire.Request.SessionID, "ag-v1:"))
		require.NotEqual(t, session, wire.Request.SessionID, "wire identity carries authenticated account scope")
		require.Len(t, usage.created, calls+1, "handler bills each successful request once")
		return binding, wire
	}

	var firstKey identityBindingKey
	var first, previous antigravity.V1InternalRequest
	for i, round := range []int{1, 2, 3, 3} {
		key, wire := forward(task88AgentOne, round)
		require.Equal(t, int64(74), upstream.accounts[len(upstream.accounts)-1])
		if i == 0 {
			firstKey, first = key, wire
			f.b.Priority = 0 // A lost binding would select the other account.
		} else {
			require.Equal(t, firstKey, key)
			require.Equal(t, first.Request.SessionID, wire.Request.SessionID)
			require.Equal(t, first.Request.SystemInstruction, wire.Request.SystemInstruction)
			require.Equal(t, first.Request.Tools, wire.Request.Tools)
			require.Equal(t, first.Request.GenerationConfig, wire.Request.GenerationConfig)
			require.Equal(t, previous.Request.Contents, wire.Request.Contents[:len(previous.Request.Contents)])
		}
		if round >= 2 {
			require.Equal(t, "received-thinking", wire.Request.Contents[1].Parts[0].ThoughtSignature)
			require.Equal(t, "received-tool", wire.Request.Contents[1].Parts[1].ThoughtSignature)
			require.Equal(t, "call-1", wire.Request.Contents[1].Parts[1].FunctionCall.ID)
			require.Equal(t, "call-1", wire.Request.Contents[2].Parts[0].FunctionResponse.ID)
		}
		previous = wire
	}
	isolatedSessions := map[string]string{"original": first.Request.SessionID}
	agentBindings := map[string]identityBindingKey{task88AgentOne: firstKey}
	for _, change := range []string{"agent", "key", "group"} {
		session := task88AgentOne
		switch change {
		case "agent":
			session = task88AgentTwo
		case "key":
			f.apiKey.ID++
		case "group":
			f.group.ID++
		}
		key, wire := forward(session, 3)
		require.NotEqual(t, firstKey, key, change+" scheduler isolation")
		for scope, id := range isolatedSessions {
			require.NotEqual(t, id, wire.Request.SessionID, change+" wire scope must differ from "+scope)
		}
		isolatedSessions[change] = wire.Request.SessionID
		require.Equal(t, int64(73), upstream.accounts[len(upstream.accounts)-1], "new scope must not inherit 74's binding")
		if change == "agent" {
			agentBindings[session] = key
		} else if change == "key" {
			f.apiKey.ID--
		} else if change == "group" {
			f.group.ID--
		}
	}
	for _, session := range []string{task88AgentOne, task88AgentTwo, task88AgentOne, task88AgentTwo} {
		key, wire := forward(session, 3)
		require.Equal(t, agentBindings[session], key, "interleaved agents keep their own scheduler binding")
		expectedScope, expectedAccount := "original", int64(74)
		if session == task88AgentTwo {
			expectedScope, expectedAccount = "agent", 73
		}
		require.Equal(t, isolatedSessions[expectedScope], wire.Request.SessionID, "interleaved agents keep their own wire identity")
		require.Equal(t, expectedAccount, upstream.accounts[len(upstream.accounts)-1])
	}
	f.a.Schedulable = false
	key, otherAccount := forward(task88AgentOne, 3)
	require.Equal(t, firstKey, key, "same authenticated conversation keeps its scheduler namespace")
	require.Equal(t, int64(73), upstream.accounts[len(upstream.accounts)-1], "normal account eligibility still applies")
	for scope, id := range isolatedSessions {
		require.NotEqual(t, id, otherAccount.Request.SessionID, "account wire scope must differ from "+scope)
	}

	bare := strings.TrimPrefix(task88AgentOne, "session_")
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(task88IdentityBody(t, bare, 1)), service.PlatformAnthropic)
	require.NoError(t, err)
	require.False(t, parsed.HasExplicitSessionIdentity(), "arbitrary bare user/account UUID must not become a conversation")
}
