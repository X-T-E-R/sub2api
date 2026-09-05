package antigravity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeminiMessagesThinkingTailRecovery(t *testing.T) {
	history := `{"role":"system","content":"hoisted instruction"},{"role":"user","content":"inspect"},{"role":"assistant","content":[{"type":"thinking","thinking":"previous plan","signature":"previous-signature"},{"type":"tool_use","id":"call-a","name":"echo","input":{"n":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":"actual output"}]}`
	for _, tail := range []string{
		`{"role":"assistant","content":[{"type":"thinking","thinking":"partial plan"}]}`,
		`{"role":"assistant","content":[{"type":"thinking","thinking":"partial plan","signature":"real-signature"}]}`,
		`{"role":"assistant","content":[]},{"role":"assistant","content":[{"type":"thinking","thinking":""},{"type":"text","text":" \n"}]},{"role":"assistant","content":""},{"role":"assistant","content":[{"type":"thinking","thinking":"another partial"}]}`,
	} {
		result, err := transformGeminiMessagesForTest(t, `{"system":"keep instructions","messages":[`+history+`,`+tail+`]}`, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		require.Equal(t, []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
			{Role: "model", Parts: []GeminiPart{
				{Text: "previous plan", Thought: true, ThoughtSignature: "previous-signature"},
				{FunctionCall: &GeminiFunctionCall{ID: "call-a", Name: "echo", Args: map[string]any{"n": float64(1)}}, ThoughtSignature: DummyThoughtSignature},
			}},
			{Role: "user", Parts: []GeminiPart{{FunctionResponse: &GeminiFunctionResponse{ID: "call-a", Name: "echo", Response: map[string]any{"result": "actual output"}}}}},
		}, result.Request.Contents)
		require.Contains(t, result.Request.SystemInstruction.Parts, GeminiPart{Text: "keep instructions"})
		require.Contains(t, result.Request.SystemInstruction.Parts, GeminiPart{Text: "hoisted instruction"})
	}
}

func TestGeminiMessagesThinkingTailBoundaries(t *testing.T) {
	t.Run("no preceding user", func(t *testing.T) {
		_, err := transformGeminiMessagesForTest(t, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"partial"}]}]}`, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.ErrorContains(t, err, "nonempty assistant text")
	})
	partial := `,{"role":"assistant","content":[{"type":"thinking","thinking":"private partial"}]},{"role":"assistant","content":[]}`
	tests := []struct{ name, history, want string }{
		{"model text", `{"role":"assistant","content":"keep answer"}`, "nonempty assistant text"},
		{"model tool", `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]}`, "nonempty assistant text"},
		{"unclosed tools", `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}},{"type":"tool_use","id":"b","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"}]}`, "every pending tool_use"},
		{"orphan result", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"output"}]}`, "preceding tool_use"},
		{"empty user", `{"role":"user","content":[]}`, "nonempty user message"},
		{"unknown block", `{"role":"assistant","content":[{"type":"unknown","data":"private"}]}`, "nonempty assistant text"},
		{"redacted thinking", `{"role":"assistant","content":[{"type":"redacted_thinking","data":"private"}]}`, "nonempty assistant text"},
		{"image", `{"role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}`, "nonempty assistant text"},
		{"unsupported user", `{"role":"user","content":[{"type":"text","text":"keep"},{"type":"document","source":{"type":"text","data":"private"}}]}`, "supported by Gemini Messages"},
		{"null assistant", `{"role":"assistant","content":null}`, "nonempty assistant text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := transformGeminiMessagesForTest(t, `{"messages":[{"role":"user","content":"question"},`+tt.history+partial+`]}`, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.ErrorContains(t, err, tt.want)
			require.NotContains(t, err.Error(), "private")
		})
	}
	for _, block := range []string{
		`{"type":"unknown","data":"private"}`,
		`{"type":"redacted_thinking","data":"private"}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}`,
		`{"type":"tool_use","id":"a","name":"echo","input":{}}`,
	} {
		_, err := transformGeminiMessagesForTest(t, `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"partial"},`+block+`]}]}`, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.Error(t, err, "mixed blocks must not be dropped as a thinking-only suffix")
	}
}

func TestGeminiMessagesThinkingTailPreservesMediaResult(t *testing.T) {
	input := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","is_error":true,"content":[{"type":"text","text":"actual error"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]},{"role":"assistant","content":[{"type":"thinking","thinking":"partial"}]}]}`
	result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
	require.NoError(t, err)
	require.Equal(t, []string{"model", "user"}, geminiRolesForTest(result.Request.Contents))
	require.Equal(t, GeminiContent{Role: "user", Parts: []GeminiPart{{FunctionResponse: &GeminiFunctionResponse{
		ID: "a", Name: "capture", Response: map[string]any{"error": "actual error"},
		Parts: []GeminiFunctionResponsePart{{InlineData: &GeminiInlineData{MimeType: "image/png", Data: "aW1hZ2U="}}},
	}}}}, result.Request.Contents[1])
}

func TestGeminiMessagesThinkingTailLongHistoryAndScope(t *testing.T) {
	history := strings.Repeat(`{"role":"user","content":"old question"},{"role":"assistant","content":"old answer"},`, 118)
	history += `{"role":"user","content":"  current input\n"}`
	tail := `,{"role":"assistant","content":[{"type":"thinking","thinking":"partial","signature":"signed-thought"}]}`
	for _, tt := range []struct {
		model   string
		policy  GeminiMessagesCompatibilityOptions
		recover bool
	}{
		{"gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true}, true},
		{"gemini-2.5-flash", GeminiMessagesCompatibilityOptions{Enabled: true}, true},
		{"gemini-3.8-flash", GeminiMessagesCompatibilityOptions{}, false},
		{"gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-2.5-flash"}}, false},
		{"claude-sonnet-4-5", GeminiMessagesCompatibilityOptions{Enabled: true}, false},
		{"gpt-5", GeminiMessagesCompatibilityOptions{Enabled: true}, false},
		{"grok-4", GeminiMessagesCompatibilityOptions{Enabled: true}, false},
	} {
		result, err := transformGeminiMessagesForTest(t, `{"messages":[`+history+tail+`]}`, tt.model, tt.policy)
		require.NoError(t, err)
		if tt.recover {
			require.Len(t, result.Request.Contents, 237)
			require.Equal(t, GeminiContent{Role: "user", Parts: []GeminiPart{{Text: "  current input\n"}}}, result.Request.Contents[236])
			for i := 0; i < 236; i += 2 {
				require.Equal(t, GeminiContent{Role: "user", Parts: []GeminiPart{{Text: "old question"}}}, result.Request.Contents[i])
				require.Equal(t, GeminiContent{Role: "model", Parts: []GeminiPart{{Text: "old answer"}}}, result.Request.Contents[i+1])
			}
		} else {
			require.Len(t, result.Request.Contents, 238)
			require.Equal(t, GeminiContent{Role: "model", Parts: []GeminiPart{{Text: "partial", Thought: true, ThoughtSignature: "signed-thought"}}}, result.Request.Contents[237])
		}
	}
}
