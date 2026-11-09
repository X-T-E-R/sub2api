package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func transformGeminiMessagesForTest(t *testing.T, input, model string, policy GeminiMessagesCompatibilityOptions) (V1InternalRequest, error) {
	t.Helper()
	var request ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(input), &request))
	originalMessages, err := json.Marshal(request.Messages)
	require.NoError(t, err)
	options := DefaultTransformOptions()
	options.GeminiMessages = policy
	body, err := TransformClaudeToGeminiWithOptions(&request, "mock-project", model, options)
	afterMessages, marshalErr := json.Marshal(request.Messages)
	require.NoError(t, marshalErr)
	require.JSONEq(t, string(originalMessages), string(afterMessages), "the adapter must not mutate caller history")
	if err != nil {
		require.Nil(t, body)
		return V1InternalRequest{}, err
	}
	var result V1InternalRequest
	require.NoError(t, json.Unmarshal(body, &result))
	return result, nil
}

func geminiRolesForTest(contents []GeminiContent) []string {
	roles := make([]string, len(contents))
	for i, content := range contents {
		roles[i] = content.Role
	}
	return roles
}

func TestGeminiMessagesCompatibilityTextAndEmptyUser(t *testing.T) {
	tests := []struct {
		name        string
		tail        string
		wantText    string
		oldTailRole string
		empty       bool
	}{
		{name: "ordinary user", tail: `"follow up"`, wantText: "follow up", oldTailRole: "user"},
		{name: "string whitespace retained around text", tail: `"  keep this\n "`, wantText: "  keep this\n ", oldTailRole: "user"},
		{name: "literal sentinel string", tail: `"(no content)"`, wantText: "(no content)", oldTailRole: "model"},
		{name: "literal sentinel block", tail: `[{"type":"text","text":"(no content)"}]`, wantText: "(no content)", oldTailRole: "model"},
		{name: "empty string", tail: `""`, oldTailRole: "model", empty: true},
		{name: "empty array", tail: `[]`, oldTailRole: "model", empty: true},
		{name: "empty text block", tail: `[{"type":"text","text":""}]`, oldTailRole: "model", empty: true},
		{name: "blank string", tail: `" \n\t"`, oldTailRole: "model", empty: true},
		{name: "blank blocks", tail: `[{"type":"text","text":"  "},{"type":"text","text":"\t"}]`, oldTailRole: "model", empty: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":` + tt.tail + `}]}`
			legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{})
			require.NoError(t, err)
			require.Equal(t, tt.oldTailRole, legacy.Request.Contents[len(legacy.Request.Contents)-1].Role)
			fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(fixed.Request.Contents))
			require.Equal(t, "question", fixed.Request.Contents[0].Parts[0].Text)
			require.Equal(t, "answer", fixed.Request.Contents[1].Parts[0].Text)
			last := fixed.Request.Contents[2]
			require.Len(t, last.Parts, 1)
			if tt.empty {
				require.Contains(t, last.Parts[0].Text, "[sub2api:empty-user]")
				require.NotContains(t, last.Parts[0].Text, "Continue")
			} else {
				require.Equal(t, tt.wantText, last.Parts[0].Text)
			}
			require.NotContains(t, last.Parts[0].Text, "assistant-prefill")
		})
	}
}

func TestGeminiMessagesCompatibilityAssistantPrefill(t *testing.T) {
	tests := []struct {
		name     string
		tail     string
		thinking string
		text     string
		thought  bool
	}{
		{name: "exact string prefix", tail: `"  {\"answer\": "`, text: "  {\"answer\": "},
		{name: "text block prefix", tail: `[{"type":"text","text":"prefix:"}]`, text: "prefix:"},
		{name: "thinking enabled text prefix", tail: `"prefix:"`, thinking: `,"thinking":{"type":"enabled","budget_tokens":1024}`, text: "prefix:"},
		{name: "existing thought retained", tail: `[{"type":"thinking","thinking":"plan","signature":"real-signature"},{"type":"text","text":"prefix:"}]`, text: "prefix:", thought: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"model":"client-alias"` + tt.thinking + `,"messages":[{"role":"user","content":"question"},{"role":"assistant","content":` + tt.tail + `}]}`
			fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(fixed.Request.Contents))
			modelParts := fixed.Request.Contents[1].Parts
			require.Equal(t, tt.text, modelParts[len(modelParts)-1].Text)
			if tt.thought {
				require.Len(t, modelParts, 2)
				require.True(t, modelParts[0].Thought)
				require.Equal(t, "plan", modelParts[0].Text)
				require.Equal(t, "real-signature", modelParts[0].ThoughtSignature)
			} else {
				require.Len(t, modelParts, 1, "no invented dummy prefill thought")
			}
			require.Len(t, fixed.Request.Contents[2].Parts, 1)
			require.Contains(t, fixed.Request.Contents[2].Parts[0].Text, "[sub2api:assistant-prefill]")
		})
	}
}

func TestGeminiMessagesCompatibilityPreservesImagesAndTools(t *testing.T) {
	input := `{"model":"client-alias","messages":[
		{"role":"user","content":[{"type":"text","text":"question"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2UtYnl0ZXM="}}]},
		{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call-a","name":"echo","input":{"value":"one"},"signature":"real-call-signature"},{"type":"tool_use","id":"call-b","name":"echo","input":{"value":"two"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-b","content":"two"},{"type":"tool_result","tool_use_id":"call-a","content":"one"},{"type":"text","text":"use both results"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"bGFzdC1pbWFnZQ=="}}]}
	]}`
	fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
	require.NoError(t, err)
	require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(fixed.Request.Contents))
	require.Equal(t, &GeminiInlineData{MimeType: "image/png", Data: "aW1hZ2UtYnl0ZXM="}, fixed.Request.Contents[0].Parts[1].InlineData)
	calls := fixed.Request.Contents[1].Parts
	require.Equal(t, "checking", calls[0].Text)
	require.Equal(t, &GeminiFunctionCall{ID: "call-a", Name: "echo", Args: map[string]any{"value": "one"}}, calls[1].FunctionCall)
	require.Equal(t, "real-call-signature", calls[1].ThoughtSignature)
	require.Equal(t, &GeminiFunctionCall{ID: "call-b", Name: "echo", Args: map[string]any{"value": "two"}}, calls[2].FunctionCall)
	results := fixed.Request.Contents[2].Parts
	require.Len(t, results, 4)
	require.Equal(t, &GeminiFunctionResponse{ID: "call-b", Name: "echo", Response: map[string]any{"result": "two"}}, results[0].FunctionResponse)
	require.Equal(t, &GeminiFunctionResponse{ID: "call-a", Name: "echo", Response: map[string]any{"result": "one"}}, results[1].FunctionResponse)
	require.Equal(t, "use both results", results[2].Text)
	require.Equal(t, &GeminiInlineData{MimeType: "image/jpeg", Data: "bGFzdC1pbWFnZQ=="}, results[3].InlineData)
}

func TestGeminiMessagesCompatibilityRealToolOutput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		isError bool
		want    string
	}{
		{name: "absent"},
		{name: "empty string", content: `,"content":""`},
		{name: "null", content: `,"content":null`},
		{name: "empty array", content: `,"content":[]`},
		{name: "empty text block", content: `,"content":[{"type":"text","text":""}]`},
		{name: "whitespace is output", content: `,"content":" \n"`, want: " \n"},
		{name: "text array", content: `,"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]`, want: "one\ntwo"},
		{name: "explicit failure", content: `,"content":"operation failed"`, isError: true, want: "operation failed"},
		{name: "failure without output", isError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorField := ""
			key := "result"
			if tt.isError {
				errorField = `,"is_error":true`
				key = "error"
			}
			input := `{"model":"client-alias","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-a","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a"` + tt.content + errorField + `}]}]}`
			fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			require.Equal(t, []string{"model", "user"}, geminiRolesForTest(fixed.Request.Contents))
			result := fixed.Request.Contents[1].Parts[0].FunctionResponse
			require.Equal(t, "call-a", result.ID)
			require.Equal(t, "echo", result.Name)
			require.Equal(t, map[string]any{key: tt.want}, result.Response)
			require.NotContains(t, result.Response[key], "Command executed successfully")
		})
	}
}

func TestGeminiMessagesCompatibilityRejectsUnsafeTailCompensation(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{name: "pending call", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]}`, want: "real tool results"},
		{name: "pending call with text", messages: `{"role":"assistant","content":[{"type":"text","text":"prefix"},{"type":"tool_use","id":"a","name":"echo","input":{}}]}`, want: "real tool results"},
		{name: "pending call then empty user", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"user","content":[]}`, want: "pending tool_use"},
		{name: "pending history then text prefill", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"user","content":"skip it"},{"role":"assistant","content":"prefix"}`, want: "real tool_result"},
		{name: "orphan result then prefill", messages: `{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"value"}]},{"role":"assistant","content":"prefix"}`, want: "preceding tool_use"},
		{name: "duplicate result then prefill", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"},{"type":"tool_result","tool_use_id":"a","content":"two"}]},{"role":"assistant","content":"prefix"}`, want: "preceding tool_use"},
		{name: "mismatched name then prefill", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","name":"different","content":"one"}]},{"role":"assistant","content":"prefix"}`, want: "id and name"},
		{name: "duplicate call id then prefill", messages: `{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{}},{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"}]},{"role":"assistant","content":"prefix"}`, want: "unique id"},
		{name: "missing call identity then prefill", messages: `{"role":"assistant","content":[{"type":"tool_use","name":"echo","input":{}}]},{"role":"assistant","content":"prefix"}`, want: "unique id"},
		{name: "wrong tool role then prefill", messages: `{"role":"user","content":[{"type":"tool_use","id":"a","name":"echo","input":{}}]},{"role":"assistant","content":"prefix"}`, want: "assistant tool_use"},
		{name: "unsupported history then prefill", messages: `{"role":"user","content":[{"type":"document","source":{"type":"text","data":"old document"}}]},{"role":"assistant","content":"prefix"}`, want: "supported by Gemini Messages"},
		{name: "thinking after model text", messages: `{"role":"assistant","content":"earlier answer"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"sig"}]}`, want: "nonempty assistant text"},
		{name: "image tail", messages: `{"role":"assistant","content":[{"type":"text","text":"image:"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}`, want: "assistant text prefills"},
		{name: "empty assistant after model", messages: `{"role":"assistant","content":"earlier answer"},{"role":"assistant","content":[]}`, want: "nonempty assistant text"},
		{name: "terminal system is not prefill", messages: `{"role":"assistant","content":"answer"},{"role":"system","content":"new instructions"}`, want: "explicit assistant text prefill"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},` + tt.messages + `]}`
			_, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.ErrorContains(t, err, tt.want)
			var compatibilityErr *GeminiMessagesCompatibilityError
			require.ErrorAs(t, err, &compatibilityErr)
			_, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{})
			require.NoError(t, legacyErr, "new local rejection is deliberate and scoped to compensation")
		})
	}
}

func TestGeminiMessagesCompatibilityDoesNotLoseUnsupportedFinalInput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "URL image", content: `[{"type":"image","source":{"type":"url","url":"https://private.example/secret"}}]`, want: "base64 source"},
		{name: "document", content: `[{"type":"document","source":{"type":"text","data":"secret-document"}}]`, want: "supported by Gemini Messages"},
		{name: "unsupported beside text", content: `[{"type":"text","text":"keep me"},{"type":"document","source":{"type":"text","data":"secret-document"}}]`, want: "content[1]"},
		{name: "unknown block", content: `[{"type":"unknown","data":"secret-value"}]`, want: "content[0]"},
		{name: "null content", content: `null`, want: "string or content block array"},
		{name: "tool result document", content: `[{"type":"tool_result","tool_use_id":"a","content":[{"type":"document","source":{"type":"text","data":"secret-document"}}]}]`, want: "tool-result content"},
		{name: "tool result URL image", content: `[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"https://private.example/secret"}}]}]`, want: "base64 source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":` + tt.content + `}]}`
			_, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, "messages[2].content")
			require.NotContains(t, err.Error(), "secret")
			require.NotContains(t, err.Error(), "private.example")
			_, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{})
			require.NoError(t, legacyErr)
		})
	}
}

func TestGeminiMessagesCompatibilityKeepsOrdinaryUserEndedHistoryPolicy(t *testing.T) {
	tests := []string{
		`{"role":"assistant","content":[{"type":"tool_use","id":"pending","name":"echo","input":{}}]},{"role":"user","content":"new user input"}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"actual output"}]}`,
		`{"role":"user","content":[{"type":"document","source":{"type":"text","data":"old unsupported document"}}]},{"role":"assistant","content":"old answer"},{"role":"user","content":"new user input"}`,
		`{"role":"assistant","content":"answer"},{"role":"user","content":"new user input"},{"role":"assistant","content":[]}`,
	}
	for _, messages := range tests {
		input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},` + messages + `]}`
		legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{})
		require.NoError(t, err)
		fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		require.Equal(t, legacy.Request.Contents, fixed.Request.Contents)
		require.Equal(t, "user", fixed.Request.Contents[len(fixed.Request.Contents)-1].Role)
	}
}

func TestGeminiMessagesCompatibilityClosedMultiTurnToolsThenPrefill(t *testing.T) {
	input := `{"model":"client-alias","messages":[
		{"role":"user","content":"question"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"echo","input":{"n":1}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"b","name":"echo","input":{"n":2}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"two"}]},
		{"role":"assistant","content":"summary:"}
	]}`
	legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{})
	require.NoError(t, err)
	fixed, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
	require.NoError(t, err)
	require.Len(t, fixed.Request.Contents, 7)
	require.Equal(t, legacy.Request.Contents, fixed.Request.Contents[:6])
	require.Equal(t, "user", fixed.Request.Contents[6].Role)
}

func TestGeminiMessagesCompatibilityFinalModelScope(t *testing.T) {
	input := `{"model":"gemini-client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"prefix"}]}`
	tests := []struct {
		name       string
		model      string
		policy     GeminiMessagesCompatibilityOptions
		webSearch  bool
		wantAdapt  bool
		wantTarget string
	}{
		{name: "enabled Gemini", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantAdapt: true},
		{name: "explicit off", model: "gemini-3.8-flash"},
		{name: "allowlist match", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-3.8-flash"}}, wantAdapt: true},
		{name: "allowlist opt out", model: "gemini-2.5-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-3.8-flash"}}},
		{name: "Claude target", model: "claude-sonnet-4-5", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
		{name: "Grok target", model: "grok-4", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
		{name: "OpenAI target", model: "gpt-5", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
		{name: "non Gemini cannot be allowlisted", model: "claude-sonnet-4-5", policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"claude-sonnet-4-5"}}},
		{name: "web search actual fallback selected", model: "claude-sonnet-4-5", webSearch: true, policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-2.5-flash"}}, wantAdapt: true, wantTarget: "gemini-2.5-flash"},
		{name: "web search excludes original model", model: "gemini-3.8-flash", webSearch: true, policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-3.8-flash"}}, wantTarget: "gemini-2.5-flash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := input
			if tt.webSearch {
				body = strings.TrimSuffix(input, "}") + `,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
			}
			result, err := transformGeminiMessagesForTest(t, body, tt.model, tt.policy)
			require.NoError(t, err)
			wantRoles := []string{"user", "model"}
			if tt.wantAdapt {
				wantRoles = append(wantRoles, "user")
			}
			require.Equal(t, wantRoles, geminiRolesForTest(result.Request.Contents))
			require.Equal(t, "prefix", result.Request.Contents[1].Parts[0].Text)
			if tt.wantTarget != "" {
				require.Equal(t, tt.wantTarget, result.Model)
			} else {
				require.Equal(t, tt.model, result.Model)
			}
		})
	}
}

func TestGeminiMessagesCompatibilityEffectiveUserTail(t *testing.T) {
	users := []struct {
		name        string
		content     string
		unsupported bool
	}{
		{name: "text and image", content: `[{"type":"text","text":"keep terminal text"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]`},
		{name: "document beside text", content: `[{"type":"text","text":"keep terminal text"},{"type":"document","source":{"type":"text","data":"document payload"}}]`, unsupported: true},
		{name: "URL beside text", content: `[{"type":"text","text":"keep terminal text"},{"type":"image","source":{"type":"url","url":"https://example.test/image"}}]`, unsupported: true},
	}
	tails := []struct{ name, messages string }{
		{name: "direct"},
		{name: "empty array", messages: `,{"role":"assistant","content":[]}`},
		{name: "empty string", messages: `,{"role":"assistant","content":""}`},
		{name: "blank text", messages: `,{"role":"assistant","content":[{"type":"text","text":" \t\n"}]}`},
		{name: "multiple empty assistants", messages: `,{"role":"assistant","content":[]},{"role":"assistant","content":""}`},
		{name: "hoisted system", messages: `,{"role":"system","content":"system instruction"}`},
	}
	for _, user := range users {
		for _, tail := range tails {
			t.Run(user.name+"/"+tail.name, func(t *testing.T) {
				input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":` + user.content + `}` + tail.messages + `]}`
				result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
				if user.unsupported {
					require.ErrorContains(t, err, "messages[2].content[1]")
					return
				}
				require.NoError(t, err)
				require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(result.Request.Contents))
				require.Equal(t, "keep terminal text", result.Request.Contents[2].Parts[0].Text)
				require.Equal(t, &GeminiInlineData{MimeType: "image/png", Data: "aW1hZ2U="}, result.Request.Contents[2].Parts[1].InlineData)
			})
		}
	}
	t.Run("empty user before empty assistant", func(t *testing.T) {
		input := `{"model":"client-alias","messages":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":[]},{"role":"assistant","content":[]}]}`
		result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(result.Request.Contents))
		require.Contains(t, result.Request.Contents[2].Parts[0].Text, "[sub2api:empty-user]")
	})
	t.Run("older unsupported history remains outside terminal check", func(t *testing.T) {
		input := `{"model":"client-alias","messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"old document"}}]},{"role":"assistant","content":"answer"},{"role":"user","content":"current input"},{"role":"assistant","content":[]}]}`
		result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		require.Equal(t, []string{"model", "user"}, geminiRolesForTest(result.Request.Contents))
		require.Equal(t, "current input", result.Request.Contents[1].Parts[0].Text)
	})
}

func TestGeminiMessagesCompatibilityImageToolResults(t *testing.T) {
	for _, isError := range []bool{false, true} {
		for _, prefill := range []bool{false, true} {
			errorField, key := "", "result"
			if isError {
				errorField, key = `,"is_error":true`, "error"
			}
			tail := `,{"role":"assistant","content":[]}`
			if prefill {
				tail = `,{"role":"assistant","content":"summary:"}`
			}
			input := `{"model":"client-alias","messages":[{"role":"user","content":"capture"},{"role":"assistant","content":[{"type":"tool_use","id":"capture-1","name":"capture","input":{"screen":1},"signature":"real-call-signature"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"capture-1"` + errorField + `,"content":[{"type":"text","text":"first note"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"Zmlyc3Q="}},{"type":"text","text":"second note"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"c2Vjb25k"}}]}]}` + tail + `]}`
			result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			wantRoles := []string{"user", "model", "user"}
			if prefill {
				wantRoles = append(wantRoles, "model", "user")
			}
			require.Equal(t, wantRoles, geminiRolesForTest(result.Request.Contents))
			require.Equal(t, "real-call-signature", result.Request.Contents[1].Parts[0].ThoughtSignature)
			require.Len(t, result.Request.Contents[2].Parts, 1, "images belong to their function response, not a separate user turn")
			response := result.Request.Contents[2].Parts[0].FunctionResponse
			require.Equal(t, "capture-1", response.ID)
			require.Equal(t, "capture", response.Name)
			require.Equal(t, map[string]any{key: "first note\nsecond note"}, response.Response)
			require.Equal(t, []GeminiFunctionResponsePart{
				{InlineData: &GeminiInlineData{MimeType: "image/png", Data: "Zmlyc3Q="}},
				{InlineData: &GeminiInlineData{MimeType: "image/jpeg", Data: "c2Vjb25k"}},
			}, response.Parts)
		}
	}
	t.Run("image only has no invented success text", func(t *testing.T) {
		input := `{"model":"client-alias","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}]}`
		result, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		response := result.Request.Contents[1].Parts[0].FunctionResponse
		require.Equal(t, map[string]any{"result": ""}, response.Response)
		require.Equal(t, "aW1hZ2U=", response.Parts[0].InlineData.Data)
	})
}

func TestGeminiMessagesCompatibilityImageToolModelScope(t *testing.T) {
	input := `{"model":"client-alias","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"capture","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"image output"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}]}`
	tests := []struct {
		name, model string
		policy      GeminiMessagesCompatibilityOptions
		wantError   bool
		wantImage   bool
		webSearch   bool
	}{
		{name: "Gemini 3", model: "gemini-3-pro", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantImage: true},
		{name: "Gemini 3.8", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantImage: true},
		{name: "older Gemini", model: "gemini-2.5-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantError: true},
		{name: "web search fallback is older Gemini", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantError: true, webSearch: true},
		{name: "unknown Gemini generation", model: "gemini-custom", policy: GeminiMessagesCompatibilityOptions{Enabled: true}, wantError: true},
		{name: "disabled", model: "gemini-3.8-flash"},
		{name: "model opted out", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{Enabled: true, Models: []string{"gemini-2.5-flash"}}},
		{name: "Claude unchanged", model: "claude-sonnet-4-5", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
		{name: "OpenAI unchanged", model: "gpt-5", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
		{name: "Grok unchanged", model: "grok-4", policy: GeminiMessagesCompatibilityOptions{Enabled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := input
			if tt.webSearch {
				body = strings.TrimSuffix(input, "}") + `,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
			}
			result, err := transformGeminiMessagesForTest(t, body, tt.model, tt.policy)
			if tt.wantError {
				require.ErrorContains(t, err, "Gemini 3+ final model")
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"model", "user"}, geminiRolesForTest(result.Request.Contents))
			response := result.Request.Contents[1].Parts[0].FunctionResponse
			require.Equal(t, map[string]any{"result": "image output"}, response.Response)
			if tt.wantImage {
				require.Len(t, response.Parts, 1)
				require.Equal(t, "aW1hZ2U=", response.Parts[0].InlineData.Data)
			} else {
				require.Empty(t, response.Parts)
			}
		})
	}
}

func TestGeminiMessagesCompatibilityCompactionHeadAndUserAnchoredToolBatches(t *testing.T) {
	mediaResultBlock := func(id, textPrefix string) string {
		return `{"type":"tool_result","tool_use_id":"` + id + `","content":[` +
			`{"type":"text","text":"` + textPrefix + `-header"},` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},` +
			`{"type":"text","text":"` + textPrefix + `-meta"},` +
			`{"type":"text","text":"` + textPrefix + `-tail"}` +
			`]}`
	}

	assertGoogleToolTurnInvariants := func(t *testing.T, contents []GeminiContent) {
		t.Helper()
		require.NotEmpty(t, contents)
		require.Equal(t, "user", contents[0].Role, "first Gemini turn must be user")
		require.Equal(t, "user", contents[len(contents)-1].Role, "last Gemini turn must be user")

		pendingCallIDs := make(map[string]string)
		for i, turn := range contents {
			if i > 0 {
				require.NotEqualf(t, contents[i-1].Role, turn.Role, "no adjacent same-role turns at contents[%d]", i)
			}
			hasFunctionCall := false
			hasFunctionResponse := false
			for _, part := range turn.Parts {
				if part.FunctionCall != nil {
					hasFunctionCall = true
					pendingCallIDs[part.FunctionCall.ID] = part.FunctionCall.Name
				}
				if part.FunctionResponse != nil {
					hasFunctionResponse = true
					expectedName, ok := pendingCallIDs[part.FunctionResponse.ID]
					require.Truef(t, ok, "functionResponse %q at contents[%d] must match an active preceding functionCall", part.FunctionResponse.ID, i)
					require.Equalf(t, expectedName, part.FunctionResponse.Name, "functionResponse %q name mismatch at contents[%d]", part.FunctionResponse.ID, i)
					delete(pendingCallIDs, part.FunctionResponse.ID)
				}
			}
			if hasFunctionCall {
				require.Equalf(t, "model", turn.Role, "functionCall turn at contents[%d] must have role=model", i)
				require.Positivef(t, i, "functionCall turn at contents[0] has no preceding user or functionResponse turn")
				require.Equalf(t, "user", contents[i-1].Role, "functionCall turn at contents[%d] must immediately follow a user/functionResponse turn", i)
			}
			if hasFunctionResponse {
				require.Equalf(t, "user", turn.Role, "functionResponse turn at contents[%d] must have role=user", i)
				require.Positivef(t, i, "functionResponse turn at contents[0] has no preceding model functionCall turn")
				require.Equalf(t, "model", contents[i-1].Role, "functionResponse turn at contents[%d] must immediately follow a model functionCall turn", i)
			}
			if turn.Role == "user" {
				require.Emptyf(t, pendingCallIDs, "all preceding functionCalls must be answered in contents[%d]", i)
			}
		}
	}

	// Pre-fix 0.29 / 1.1 shape: compaction_summary was placed after call68's tool_result,
	// leaving messages[0] as assistant(ReadMediaFile x2) with no leading user turn.
	unanchoredCompacted := func(trailingTexts []string) string {
		tailBlocks := []string{
			`{"type":"tool_result","tool_use_id":"call71","content":[{"type":"text","text":"out"},{"type":"text","text":"appendix"}]}`,
		}
		for _, txt := range trailingTexts {
			tailBlocks = append(tailBlocks, `{"type":"text","text":"`+txt+`"}`)
		}
		return strings.Join([]string{
			`{"role":"assistant","content":[{"type":"tool_use","id":"call12","name":"ReadMediaFile","input":{"path":"a.png"},"signature":"sig-12"},{"type":"tool_use","id":"call13","name":"ReadMediaFile","input":{"path":"b.png"},"signature":"sig-13"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call12", "r12") + `,` + mediaResultBlock("call13", "r13") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call14","name":"ReadMediaFile","input":{"path":"c.png"},"signature":"sig-14"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call14", "r14") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call63","name":"ReadMediaFile","input":{"path":"d.png"},"signature":"sig-63"},{"type":"tool_use","id":"call64","name":"ReadMediaFile","input":{"path":"e.png"},"signature":"sig-64"},{"type":"tool_use","id":"call65","name":"ReadMediaFile","input":{"path":"f.png"},"signature":"sig-65"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call63", "r63") + `,` + mediaResultBlock("call64", "r64") + `,` + mediaResultBlock("call65", "r65") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call68","name":"ReadMediaFile","input":{"path":"g.png"},"signature":"sig-68"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call68", "r68") + `,{"type":"text","text":"[compaction_summary] summarized earlier context"}]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call71","name":"Bash","input":{"command":"ls"},"signature":"sig-71"}]}`,
			`{"role":"user","content":[` + strings.Join(tailBlocks, ",") + `]}`,
		}, ",")
	}

	// Post-fix 0.29 / 1.1 shape (b3): compaction_summary is hoisted to messages[0] as the
	// leading user turn, and call68's user turn holds only its tool_result.
	fixedCompacted := func(trailingTexts []string) string {
		tailBlocks := []string{
			`{"type":"tool_result","tool_use_id":"call71","content":[{"type":"text","text":"out"},{"type":"text","text":"appendix"}]}`,
		}
		for _, txt := range trailingTexts {
			tailBlocks = append(tailBlocks, `{"type":"text","text":"`+txt+`"}`)
		}
		return strings.Join([]string{
			`{"role":"user","content":[{"type":"text","text":"[compaction_summary] summarized earlier context"}]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call12","name":"ReadMediaFile","input":{"path":"a.png"},"signature":"sig-12"},{"type":"tool_use","id":"call13","name":"ReadMediaFile","input":{"path":"b.png"},"signature":"sig-13"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call12", "r12") + `,` + mediaResultBlock("call13", "r13") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call14","name":"ReadMediaFile","input":{"path":"c.png"},"signature":"sig-14"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call14", "r14") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call63","name":"ReadMediaFile","input":{"path":"d.png"},"signature":"sig-63"},{"type":"tool_use","id":"call64","name":"ReadMediaFile","input":{"path":"e.png"},"signature":"sig-64"},{"type":"tool_use","id":"call65","name":"ReadMediaFile","input":{"path":"f.png"},"signature":"sig-65"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call63", "r63") + `,` + mediaResultBlock("call64", "r64") + `,` + mediaResultBlock("call65", "r65") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call68","name":"ReadMediaFile","input":{"path":"g.png"},"signature":"sig-68"}]}`,
			`{"role":"user","content":[` + mediaResultBlock("call68", "r68") + `]}`,
			`{"role":"assistant","content":[{"type":"tool_use","id":"call71","name":"Bash","input":{"command":"ls"},"signature":"sig-71"}]}`,
			`{"role":"user","content":[` + strings.Join(tailBlocks, ",") + `]}`,
		}, ",")
	}

	t.Run("pre-fix 0.29 and 1.1 unanchored assistant head produces leading model functionCall at contents[0]", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			trailingTexts []string
		}{
			{name: "step-0.29", trailingTexts: []string{"ssh_hosts", "model_steering"}},
			{name: "step-1.1", trailingTexts: []string{"ssh_hosts", "model_steering", "runtime_snapshot", "system_trigger", "runtime_snapshot", "model_steering"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				input := `{"model":"gemini-4-argon","messages":[` + unanchoredCompacted(tc.trailingTexts) + `]}`
				result, err := transformGeminiMessagesForTest(t, input, "gemini-4-argon", GeminiMessagesCompatibilityOptions{Enabled: true})
				require.NoError(t, err)
				require.Equal(t, []string{
					"model", "user",
					"model", "user",
					"model", "user",
					"model", "user",
					"model", "user",
				}, geminiRolesForTest(result.Request.Contents))
				// First two Gemini turns in pre-fix shape:
				// contents[0] is model(functionCall x2) with no preceding user turn -> triggers Google 400 INVALID_ARGUMENT.
				require.Equal(t, "model", result.Request.Contents[0].Role)
				require.Len(t, result.Request.Contents[0].Parts, 2)
				require.Equal(t, "call12", result.Request.Contents[0].Parts[0].FunctionCall.ID)
				require.Equal(t, "ReadMediaFile", result.Request.Contents[0].Parts[0].FunctionCall.Name)
				require.Equal(t, "sig-12", result.Request.Contents[0].Parts[0].ThoughtSignature)
				require.Equal(t, "call13", result.Request.Contents[0].Parts[1].FunctionCall.ID)
				require.Equal(t, "ReadMediaFile", result.Request.Contents[0].Parts[1].FunctionCall.Name)
				require.Equal(t, "sig-13", result.Request.Contents[0].Parts[1].ThoughtSignature)

				require.Equal(t, "user", result.Request.Contents[1].Role)
				require.Len(t, result.Request.Contents[1].Parts, 2)
				require.Equal(t, "call12", result.Request.Contents[1].Parts[0].FunctionResponse.ID)
				require.Equal(t, "call13", result.Request.Contents[1].Parts[1].FunctionResponse.ID)
			})
		}
	})

	t.Run("post-fix 0.29 and 1.1 hoisted compaction_summary starts with user and satisfies all Gemini turn adjacency invariants", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			trailingTexts []string
		}{
			{name: "step-0.29", trailingTexts: []string{"ssh_hosts", "model_steering"}},
			{name: "step-1.1", trailingTexts: []string{"ssh_hosts", "model_steering", "runtime_snapshot", "system_trigger", "runtime_snapshot", "model_steering"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				input := `{"model":"gemini-4-argon","messages":[` + fixedCompacted(tc.trailingTexts) + `]}`
				result, err := transformGeminiMessagesForTest(t, input, "gemini-4-argon", GeminiMessagesCompatibilityOptions{Enabled: true})
				require.NoError(t, err)
				require.Equal(t, []string{
					"user",
					"model", "user",
					"model", "user",
					"model", "user",
					"model", "user",
					"model", "user",
				}, geminiRolesForTest(result.Request.Contents))

				// Verify full sequence satisfies strict Gemini role alternation and functionCall/functionResponse adjacency.
				assertGoogleToolTurnInvariants(t, result.Request.Contents)

				// First two Gemini turns in post-fix shape:
				// contents[0] is user(text: compaction_summary), contents[1] is model(functionCall: call12, call13).
				require.Equal(t, []GeminiPart{
					{Text: "[compaction_summary] summarized earlier context"},
				}, result.Request.Contents[0].Parts)
				require.Len(t, result.Request.Contents[1].Parts, 2)
				require.Equal(t, "call12", result.Request.Contents[1].Parts[0].FunctionCall.ID)
				require.Equal(t, "ReadMediaFile", result.Request.Contents[1].Parts[0].FunctionCall.Name)
				require.Equal(t, "sig-12", result.Request.Contents[1].Parts[0].ThoughtSignature)
				require.Equal(t, "call13", result.Request.Contents[1].Parts[1].FunctionCall.ID)
				require.Equal(t, "ReadMediaFile", result.Request.Contents[1].Parts[1].FunctionCall.Name)
				require.Equal(t, "sig-13", result.Request.Contents[1].Parts[1].ThoughtSignature)

				// Batch 1 responses (contents[2])
				require.Len(t, result.Request.Contents[2].Parts, 2)
				require.Equal(t, "call12", result.Request.Contents[2].Parts[0].FunctionResponse.ID)
				require.Equal(t, "ReadMediaFile", result.Request.Contents[2].Parts[0].FunctionResponse.Name)
				require.Equal(t, map[string]any{"result": "r12-header\nr12-meta\nr12-tail"}, result.Request.Contents[2].Parts[0].FunctionResponse.Response)
				require.Len(t, result.Request.Contents[2].Parts[0].FunctionResponse.Parts, 1)
				require.Equal(t, "aW1hZ2U=", result.Request.Contents[2].Parts[0].FunctionResponse.Parts[0].InlineData.Data)
				require.Equal(t, "call13", result.Request.Contents[2].Parts[1].FunctionResponse.ID)

				// Batch 2 (contents[3..4]): call14
				require.Len(t, result.Request.Contents[3].Parts, 1)
				require.Equal(t, "call14", result.Request.Contents[3].Parts[0].FunctionCall.ID)
				require.Len(t, result.Request.Contents[4].Parts, 1)
				require.Equal(t, "call14", result.Request.Contents[4].Parts[0].FunctionResponse.ID)

				// Batch 3 (contents[5..6]): call63, call64, call65
				require.Len(t, result.Request.Contents[5].Parts, 3)
				require.Equal(t, []string{"call63", "call64", "call65"}, []string{
					result.Request.Contents[5].Parts[0].FunctionCall.ID,
					result.Request.Contents[5].Parts[1].FunctionCall.ID,
					result.Request.Contents[5].Parts[2].FunctionCall.ID,
				})
				require.Len(t, result.Request.Contents[6].Parts, 3)
				require.Equal(t, []string{"call63", "call64", "call65"}, []string{
					result.Request.Contents[6].Parts[0].FunctionResponse.ID,
					result.Request.Contents[6].Parts[1].FunctionResponse.ID,
					result.Request.Contents[6].Parts[2].FunctionResponse.ID,
				})

				// Batch 4 (contents[7..8]): call68 only (compaction_summary was moved to contents[0])
				require.Len(t, result.Request.Contents[7].Parts, 1)
				require.Equal(t, "call68", result.Request.Contents[7].Parts[0].FunctionCall.ID)
				require.Len(t, result.Request.Contents[8].Parts, 1)
				require.Equal(t, "call68", result.Request.Contents[8].Parts[0].FunctionResponse.ID)

				// Batch 5 (contents[9..10]): call71 + trailing injected user text blocks in same final user turn
				require.Len(t, result.Request.Contents[9].Parts, 1)
				require.Equal(t, "call71", result.Request.Contents[9].Parts[0].FunctionCall.ID)
				require.Equal(t, "Bash", result.Request.Contents[9].Parts[0].FunctionCall.Name)
				require.Len(t, result.Request.Contents[10].Parts, 1+len(tc.trailingTexts))
				require.Equal(t, "call71", result.Request.Contents[10].Parts[0].FunctionResponse.ID)
				require.Equal(t, "Bash", result.Request.Contents[10].Parts[0].FunctionResponse.Name)
				require.Equal(t, map[string]any{"result": "out\nappendix"}, result.Request.Contents[10].Parts[0].FunctionResponse.Response)
				for idx, txt := range tc.trailingTexts {
					require.Equal(t, txt, result.Request.Contents[10].Parts[1+idx].Text)
				}
			})
		}
	})
}
