package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	require.Equal(t, "AUTO", marked.effectiveMode)
	require.Equal(t, "recent_malformed_function_call", marked.autoReason)

	for _, choice := range []string{`null`, `"auto"`, `{"type":"auto"}`} {
		copyReq := req
		copyReq.ToolChoice = json.RawMessage(choice)
		explicit, err := antigravity.TransformClaudeToGeminiWithOptions(&copyReq, "project", "gemini-4-hydrogen", antigravity.TransformOptions{})
		require.NoError(t, err)
		got, state, err := svc.prepareMalformedFunctionRecovery(c, &copyReq, explicit)
		require.NoError(t, err)
		require.NotNil(t, state)
		require.Equal(t, initial.key, state.key)
		require.Equal(t, "AUTO", state.effectiveMode)
		if choice != `null` {
			require.Empty(t, state.autoReason)
			require.Equal(t, explicit, got)
		}
	}

	for _, choice := range []string{`"none"`, `"any"`, `{"type":"none"}`, `{"type":"any"}`, `{"type":"tool","name":"lookup"}`, `{"type":"function","function":{"name":"lookup"}}`} {
		copyReq := req
		copyReq.ToolChoice = json.RawMessage(choice)
		explicit, err := antigravity.TransformClaudeToGeminiWithOptions(&copyReq, "project", "gemini-4-hydrogen", antigravity.TransformOptions{})
		require.NoError(t, err)
		got, state, err := svc.prepareMalformedFunctionRecovery(c, &copyReq, explicit)
		require.NoError(t, err)
		require.Nil(t, state)
		require.Equal(t, explicit, got)
	}
	for path, value := range map[string]any{
		"model": "claude-opus-4-8", antigravityFunctionModePath: "NONE",
		"request.toolConfig.functionCallingConfig.allowedFunctionNames": []string{"lookup"},
	} {
		excluded, err := sjson.SetBytes(wire, path, value)
		require.NoError(t, err)
		got, state, err := svc.prepareMalformedFunctionRecovery(c, &req, excluded)
		require.NoError(t, err)
		require.Nil(t, state, path)
		require.Equal(t, excluded, got)
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
			cache.mark(key, "VALIDATED")
			cache.count(key)
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

func TestAntigravityMalformedAcrossExplicitAutoRetries(t *testing.T) {
	const malformedParts = `[{"functionCall":{"name":"lookup","args":{"path":"large optional argument"}}}]`
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(missingTerminalFixture(malformedParts, "MALFORMED_FUNCTION_CALL")),
		antigravityThoughtStreamResponse(missingTerminalFixture(malformedParts, "MALFORMED_FUNCTION_CALL")),
		antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
	}}
	svc := newMissingTerminalReplayService(t, upstream)
	body := []byte(malformedRecoveryRequest)
	auto, err := sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"auto"}`))
	require.NoError(t, err)
	core, logs := observer.New(zapcore.InfoLevel)
	for i, request := range [][]byte{body, auto, auto} {
		c, _ := newAntigravityCompatContext("POST", "/v1/messages", request)
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		_, err = svc.Forward(c.Request.Context(), c, antigravityThoughtAccount(nil), request, false)
		if i < 2 {
			require.True(t, IsAntigravityNoUsableAnswerError(err), "attempt %d: %v", i+1, err)
			require.Len(t, svc.malformedFunctionRetries.entries, 1)
			for _, entry := range svc.malformedFunctionRetries.entries {
				require.Equal(t, i+1, entry.count)
			}
		} else {
			require.NoError(t, err)
			require.Empty(t, svc.malformedFunctionRetries.entries)
		}
		require.Equal(t, i+1, upstream.callCount, "no replay after a partial tool call")
	}
	first, second, third := upstream.requestBodies[0], upstream.requestBodies[1], upstream.requestBodies[2]
	require.Equal(t, "VALIDATED", gjson.GetBytes(first, antigravityFunctionModePath).String())
	require.Equal(t, "AUTO", gjson.GetBytes(second, antigravityFunctionModePath).String())
	require.Equal(t, "AUTO", gjson.GetBytes(third, antigravityFunctionModePath).String())
	require.Equal(t, cachePrefixOf(first), cachePrefixOf(second))
	require.NotContains(t, string(second), "[System recovery note:")
	require.Equal(t, antigravityMalformedSingleToolNote, gjson.GetBytes(third, "request.contents.0.parts.1.text").String())
	thirdWithoutNote, err := sjson.DeleteBytes(third, "request.contents.0.parts.1")
	require.NoError(t, err)
	require.Equal(t, cachePrefixOf(second), cachePrefixOf(thirdWithoutNote))
	entries := logs.FilterMessage("antigravity.response_summary").All()
	require.Len(t, entries, 3)
	for i, wantMode := range []string{"VALIDATED", "AUTO", "AUTO"} {
		require.Equal(t, wantMode, entries[i].ContextMap()["function_calling_mode"])
	}
	require.Equal(t, "repeated_malformed_function_call_single_tool", entries[2].ContextMap()["function_calling_auto_reason"])
}

func TestAntigravityMalformedRetryFirstAutoFailureEscalatesAndRefreshesTTL(t *testing.T) {
	cache := newAntigravityMalformedRetryCache()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	r := &antigravityMalformedRecovery{cache: cache, effectiveMode: "AUTO"}
	failed := &antigravityThoughtGeneration{finishReason: "MALFORMED_FUNCTION_CALL"}
	r.finish(failed, false)
	require.Equal(t, 2, cache.count(r.key))
	now = now.Add(antigravityMalformedRetryTTL - time.Second)
	r.finish(failed, false)
	require.Equal(t, 3, cache.count(r.key))
	now = now.Add(time.Second)
	require.Equal(t, 3, cache.count(r.key), "a new malformed attempt refreshes expiry")
	now = now.Add(antigravityMalformedRetryTTL)
	require.Zero(t, cache.count(r.key))
}

func TestAntigravityMalformedSingleToolNotePreservesOtherBytes(t *testing.T) {
	for _, tc := range []struct {
		name, contents, prefix, suffix string
	}{
		{"last_user", `[ {"role":"user","parts":[{"text":"early question"}]}, {"role":"model","parts":[{"text":"reply"}]}, {"role":"user","parts":[{"text":"late question"}]}, {"role":"model","parts":[{"text":"tail"}]} ]`, `{"text":"late question"}`, `]}, {"role":"model","parts":[{"text":"tail"}]} ]`},
		{"last_content", `[ {"role":"model","parts":[{"text":"early model"}]}, {"role":"model","parts":[{"text":"tail"}]} ]`, `{"text":"tail"}`, `]} ]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{ "request": {"systemInstruction": {"parts": [{"text":"system"}]}, "contents": ` + tc.contents + `, "tools": [ {"functionDeclarations": [{"name":"lookup"}]} ] } }`)
			note, err := json.Marshal(map[string]string{"text": antigravityMalformedSingleToolNote})
			require.NoError(t, err)
			start := strings.Index(string(body), tc.prefix+tc.suffix) + len(tc.prefix)
			require.GreaterOrEqual(t, start, len(tc.prefix))
			want := string(body[:start]) + "," + string(note) + string(body[start:])
			got, err := appendAntigravityMalformedSingleToolNote(body)
			require.NoError(t, err)
			require.Equal(t, want, string(got), "the only byte change is appending the note to the selected content")
		})
	}
}

func TestAntigravityMalformedZeroOutputAutoReplayUsesSingleToolNote(t *testing.T) {
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
		antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "MALFORMED_FUNCTION_CALL")),
		antigravityThoughtStreamResponse(missingTerminalFixture(`[]`, "MALFORMED_FUNCTION_CALL")),
	}}
	svc := newMissingTerminalReplayService(t, upstream)
	body, err := sjson.SetBytes([]byte(malformedRecoveryRequest), "tool_choice", "auto")
	require.NoError(t, err)
	c, _ := newAntigravityCompatContext("POST", "/v1/messages", body)
	_, err = svc.Forward(c.Request.Context(), c, antigravityThoughtAccount(nil), body, false)
	require.Error(t, err)
	require.Equal(t, 2, upstream.callCount)
	require.NotContains(t, string(upstream.requestBodies[0]), "[System recovery note:")
	require.Equal(t, antigravityMalformedSingleToolNote, gjson.GetBytes(upstream.requestBodies[1], "request.contents.0.parts.1.text").String())
	require.Len(t, svc.malformedFunctionRetries.entries, 1)
	for _, entry := range svc.malformedFunctionRetries.entries {
		require.Equal(t, 3, entry.count, "both actual AUTO failures must be counted")
	}
}

func TestAntigravityMalformedStage2ReapplyPreservesSingleToolNote(t *testing.T) {
	wire := []byte(`{"model":"gemini-4-hydrogen","request":{"contents":[{"role":"user","parts":[{"text":"question"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}}}`)

	stage1 := &antigravityMalformedRecovery{autoReason: "recent_malformed_function_call"}
	gotStage1, err := stage1.reapply(wire)
	require.NoError(t, err)
	require.Equal(t, "AUTO", gjson.GetBytes(gotStage1, antigravityFunctionModePath).String())
	require.Equal(t, 1, len(gjson.GetBytes(gotStage1, "request.contents.0.parts").Array()))

	stage2 := &antigravityMalformedRecovery{autoReason: antigravityMalformedSingleToolReason}
	gotStage2, err := stage2.reapply(wire)
	require.NoError(t, err)
	require.Equal(t, "AUTO", gjson.GetBytes(gotStage2, antigravityFunctionModePath).String())
	require.Equal(t, antigravityMalformedSingleToolNote, gjson.GetBytes(gotStage2, "request.contents.0.parts.1.text").String())
}
