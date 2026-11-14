package service

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const malformedRecoveryRequest = `{"model":"client-alias","stream":true,"max_tokens":64,"metadata":{"user_id":"recovery-session"},"system":"stable system","tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":"question"}]}`

func cachePrefixOf(body []byte) []string {
	return []string{gjson.GetBytes(body, "request.tools").Raw,
		gjson.GetBytes(body, "request.systemInstruction").Raw,
		gjson.GetBytes(body, "request.contents").Raw}
}

func TestAntigravityMalformedZeroOutputRelaxesOnlyDefaultMode(t *testing.T) {
	for _, choice := range []string{"", `"none"`, `"any"`, `{"type":"tool","name":"lookup"}`} {
		for _, stream := range []bool{true, false} {
			t.Run(choice+boolLiteral(stream), func(t *testing.T) {
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
					antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "MALFORMED_FUNCTION_CALL")),
					antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
				}}
				svc := newMissingTerminalReplayService(t, upstream)
				body, err := sjson.SetBytes([]byte(malformedRecoveryRequest), "stream", stream)
				require.NoError(t, err)
				if choice != "" {
					body, err = sjson.SetRawBytes(body, "tool_choice", []byte(choice))
					require.NoError(t, err)
				}
				c, rec := newAntigravityCompatContext("POST", "/v1/messages", body)
				result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, upstream.callCount)
				require.Contains(t, rec.Body.String(), "answer")
				require.NotContains(t, rec.Body.String(), "antigravity_malformed_function_call")
				first, second := upstream.requestBodies[0], upstream.requestBodies[1]
				require.Equal(t, cachePrefixOf(first), cachePrefixOf(second))
				if choice == "" {
					require.Equal(t, "VALIDATED", gjson.GetBytes(first, antigravityFunctionModePath).String())
					require.Equal(t, "AUTO", gjson.GetBytes(second, antigravityFunctionModePath).String())
					expected, err := antigravityAutoFunctionMode(first)
					require.NoError(t, err)
					require.Equal(t, expected, second, "only mode's bytes may change")
					require.Equal(t, "zero_output_malformed_function_call", antigravityThoughtContinuationFromGin(c).malformedRecovery.autoReason)
				} else {
					require.Equal(t, first, second, "explicit NONE/ANY/allowedFunctionNames unchanged")
				}
			})
		}
	}
}

func TestAntigravityMalformedPartialRetryUsesAutoThenClears(t *testing.T) {
	for _, partial := range []string{
		`[{"thought":true,"text":"plan","thoughtSignature":"sig-plan"}]`,
		`[{"functionCall":{"name":"lookup","args":{"path":"x"}}}]`,
	} {
		t.Run(partial, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(missingTerminalFixture(partial, "MALFORMED_FUNCTION_CALL")),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			svc := newMissingTerminalReplayService(t, upstream)
			body := []byte(malformedRecoveryRequest)
			c, rec := newAntigravityCompatContext("POST", "/v1/messages", body)
			c.Set("api_key", &APIKey{ID: 123})
			result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
			require.True(t, IsAntigravityNoUsableAnswerError(err))
			require.NotNil(t, result)
			require.Equal(t, 1, upstream.callCount, "thinking/tool blocks cannot be grafted onto a new generation")
			require.Contains(t, rec.Body.String(), "antigravity_malformed_function_call")
			require.Positive(t, antigravityThoughtContinuationFromGin(c).delivery.ContentBlockEvents)
			for i, wantMode := range []string{"AUTO", "VALIDATED"} {
				next, _ := newAntigravityCompatContext("POST", "/v1/messages", body)
				next.Set("api_key", &APIKey{ID: 123})
				_, err = svc.Forward(context.Background(), next, antigravityThoughtAccount(nil), body, false)
				require.NoError(t, err)
				require.Equal(t, wantMode, gjson.GetBytes(upstream.requestBodies[i+1], antigravityFunctionModePath).String())
				require.Equal(t, cachePrefixOf(upstream.requestBodies[0]), cachePrefixOf(upstream.requestBodies[i+1]))
			}
			require.Empty(t, svc.malformedFunctionRetries.entries)
		})
	}
}

func TestAntigravityMalformedRetryIdentityExplicitModesAndTTL(t *testing.T) {
	svc := newMissingTerminalReplayService(t, &queuedHTTPUpstreamStub{})
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	svc.malformedFunctionRetries.now = func() time.Time { return now }
	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(malformedRecoveryRequest), &req))
	wire, err := antigravity.TransformClaudeToGeminiWithOptions(&req, "project", "gemini-4-hydrogen", antigravity.TransformOptions{})
	require.NoError(t, err)
	c, _ := newAntigravityCompatContext("POST", "/v1/messages", nil)
	c.Set("api_key", &APIKey{ID: 1})
	_, initial, err := svc.prepareMalformedFunctionRecovery(c, &req, wire)
	require.NoError(t, err)
	initial.finish(&antigravityThoughtGeneration{finishReason: "MALFORMED_FUNCTION_CALL"}, false)
	_, marked, err := svc.prepareMalformedFunctionRecovery(c, &req, wire)
	require.NoError(t, err)
	require.NotEmpty(t, marked.autoReason)

	for _, choice := range []string{`"none"`, `"any"`, `{"type":"tool","name":"lookup"}`} {
		copyReq := req
		copyReq.ToolChoice = json.RawMessage(choice)
		explicit, err := antigravity.TransformClaudeToGeminiWithOptions(&copyReq, "project", "gemini-4-hydrogen", antigravity.TransformOptions{})
		require.NoError(t, err)
		got, state, err := svc.prepareMalformedFunctionRecovery(c, &copyReq, explicit)
		require.NoError(t, err)
		require.Nil(t, state)
		require.Equal(t, explicit, got)
	}
	for _, change := range []string{"key", "session", "history", "model"} {
		copyReq := req
		next, _ := newAntigravityCompatContext("POST", "/v1/messages", nil)
		next.Set("api_key", &APIKey{ID: 1})
		switch change {
		case "key":
			next.Set("api_key", &APIKey{ID: 2})
		case "session":
			next.Request.Header.Set("X-Session-Id", "other")
		case "history":
			copyReq.Messages = append(append([]antigravity.ClaudeMessage(nil), req.Messages...), req.Messages[0])
		case "model":
			copyReq.Model = "other-model"
		}
		_, got, err := svc.prepareMalformedFunctionRecovery(next, &copyReq, wire)
		require.NoError(t, err)
		require.Empty(t, got.autoReason, change)
	}
	now = now.Add(antigravityMalformedRetryTTL)
	got, state, err := svc.prepareMalformedFunctionRecovery(c, &req, wire)
	require.NoError(t, err)
	require.Empty(t, state.autoReason)
	require.Equal(t, wire, got)
}

func TestAntigravityMalformedRetryCacheBoundedAndConcurrent(t *testing.T) {
	cache := newAntigravityMalformedRetryCache()
	var wg sync.WaitGroup
	for i := 0; i < antigravityMalformedRetryLimit+20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var key [32]byte
			key[0], key[1] = byte(i), byte(i>>8)
			cache.mark(key)
			cache.contains(key)
		}(i)
	}
	wg.Wait()
	require.Len(t, cache.entries, antigravityMalformedRetryLimit)
}

func TestAntigravityMalformedRetryDoesNotDependOnReplayPolicy(t *testing.T) {
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "MALFORMED_FUNCTION_CALL")),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{}, upstream)
	body := []byte(malformedRecoveryRequest)
	first, _ := newAntigravityCompatContext("POST", "/v1/messages", body)
	_, err := svc.Forward(context.Background(), first, antigravityThoughtAccount(nil), body, false)
	require.True(t, IsAntigravityNoUsableAnswerError(err))
	require.Equal(t, 1, upstream.callCount)
	next, _ := newAntigravityCompatContext("POST", "/v1/messages", body)
	_, err = svc.Forward(context.Background(), next, antigravityThoughtAccount(nil), body, false)
	require.NoError(t, err)
	require.Equal(t, "AUTO", gjson.GetBytes(upstream.requestBodies[1], antigravityFunctionModePath).String())
	require.Empty(t, svc.malformedFunctionRetries.entries)
}
