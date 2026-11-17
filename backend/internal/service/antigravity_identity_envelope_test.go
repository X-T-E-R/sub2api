package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestInjectIdentityPatchToGeminiRequest_IsolatesExistingParts(t *testing.T) {
	body := []byte(`{"systemInstruction":{"role":"user","parts":[{"text":"You are Nia. Reply in short chat bubbles."},{"inlineData":{"mimeType":"text/plain","data":"aGk="}},{"text":"Keep my voice."}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"temperature":0.7}}`)
	got, err := injectIdentityPatchToGeminiRequest(body)
	require.NoError(t, err)

	parts := gjson.GetBytes(got, "systemInstruction.parts").Array()
	require.Len(t, parts, 6)
	require.Equal(t, antigravity.GetDefaultIdentityPatch(), parts[0].Get("text").String())
	require.Equal(t, antigravity.IdentityTransportUserRules, parts[1].Get("text").String())
	for i, original := range gjson.GetBytes(body, "systemInstruction.parts").Array() {
		require.JSONEq(t, original.Raw, parts[i+2].Raw)
	}
	require.Equal(t, "</user_rules>", parts[5].Get("text").String())
	require.Equal(t, "user", gjson.GetBytes(got, "systemInstruction.role").String())
	for _, path := range []string{"contents", "generationConfig"} {
		require.JSONEq(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(got, path).Raw)
	}

	// A second pass must not duplicate either the identity or the boundary.
	again, err := injectIdentityPatchToGeminiRequest(got)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestInjectIdentityPatchToGeminiRequest_IdentityOnlyWithoutParts(t *testing.T) {
	for _, body := range []string{`{}`, `{"systemInstruction":{"role":"user"}}`} {
		t.Run(body, func(t *testing.T) {
			got, err := injectIdentityPatchToGeminiRequest([]byte(body))
			require.NoError(t, err)
			parts := gjson.GetBytes(got, "systemInstruction.parts").Array()
			require.Len(t, parts, 1)
			require.Equal(t, antigravity.GetDefaultIdentityPatch(), parts[0].Get("text").String())
		})
	}
}

func TestInjectIdentityPatchToGeminiRequest_PreservesCallerIdentity(t *testing.T) {
	body := []byte(`{ "systemInstruction": {"parts": [{"text":"Keep this prefix."}, {"text":"You are Antigravity. Caller identity."}]}, "contents": [] }`)
	got, err := injectIdentityPatchToGeminiRequest(body)
	require.NoError(t, err)
	require.Equal(t, body, got)

	_, err = injectIdentityPatchToGeminiRequest([]byte(`{"systemInstruction":`))
	require.Error(t, err)
}

func TestAntigravityIdentityEnvelope_MessagesGeminiAndCompatShareBoundary(t *testing.T) {
	ctx := context.Background()
	svc := &AntigravityGatewayService{}
	claudeBody := []byte(`{"model":"gemini-4-hydrogen","max_tokens":64,"system":[{"type":"text","text":"You are Nia. Reply in short chat bubbles."},{"type":"text","text":"Keep my voice."}],"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"mcp__phone__tap","input_schema":{"type":"object"}}]}`)
	var req antigravity.ClaudeRequest
	require.NoError(t, json.Unmarshal(claudeBody, &req))
	messages, err := antigravity.TransformClaudeToGeminiWithOptions(&req, "project-1", req.Model, antigravity.DefaultTransformOptions())
	require.NoError(t, err)

	compat, err := svc.buildAntigravityCompatGeminiBody(ctx, claudeBody, &req, "project-1", req.Model, false)
	require.NoError(t, err)
	native, err := injectIdentityPatchToGeminiRequest([]byte(`{"systemInstruction":{"parts":[{"text":"You are Nia. Reply in short chat bubbles."},{"text":"Keep my voice."}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	require.NoError(t, err)
	native, err = svc.wrapV1InternalRequest("project-1", req.Model, native)
	require.NoError(t, err)

	require.JSONEq(t, gjson.GetBytes(messages, "request.systemInstruction.parts").Raw, gjson.GetBytes(native, "request.systemInstruction.parts").Raw)
	for name, wire := range map[string][]byte{"Messages": messages, "native Gemini": native, "compat": compat} {
		t.Run(name, func(t *testing.T) {
			parts := gjson.GetBytes(wire, "request.systemInstruction.parts").Array()
			if name == "compat" {
				// The existing compat converter joins source system blocks into one text part.
				require.Len(t, parts, 4)
			} else {
				require.Len(t, parts, 5)
			}
			require.Equal(t, antigravity.GetDefaultIdentityPatch(), parts[0].Get("text").String())
			require.Equal(t, antigravity.IdentityTransportUserRules, parts[1].Get("text").String())
			require.Equal(t, "</user_rules>", parts[len(parts)-1].Get("text").String())
			var userTexts []string
			for _, part := range parts[2 : len(parts)-1] {
				userTexts = append(userTexts, part.Get("text").String())
			}
			require.Equal(t, "You are Nia. Reply in short chat bubbles.\nKeep my voice.", strings.Join(userTexts, "\n"))
			require.NotContains(t, string(wire), "internal initialization logs")
			require.NotContains(t, string(wire), "SYSTEM_PROMPT_END")
			require.NotContains(t, string(wire), "MCP XML")
		})
	}
}
