package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// allowThinkingTail is the opt-in policy that permits a received thinking tail to
// stay as the final model turn of a Gemini Messages request.
func allowThinkingTail(models ...string) GeminiMessagesCompatibilityOptions {
	return GeminiMessagesCompatibilityOptions{Enabled: true, Models: models, AllowThinkingTailContinuation: true}
}

// requireNoSyntheticTurn asserts that the adapter added no compensation text: the
// continuation carries the received thinking and signature only.
func requireNoSyntheticTurn(t *testing.T, request V1InternalRequest) {
	t.Helper()
	body, err := json.Marshal(request.Request.Contents)
	require.NoError(t, err)
	require.NotContains(t, string(body), "sub2api:")
	require.NotContains(t, string(body), "assistant-prefill")
	require.NotContains(t, string(body), "empty-user")
}

// requireModelTail asserts the request ends with one model turn that holds exactly
// the received carriers, in the received order.
func requireModelTail(t *testing.T, request V1InternalRequest, want []GeminiPart) {
	t.Helper()
	require.Equal(t, []string{"user", "model"}, geminiRolesForTest(request.Request.Contents))
	require.Equal(t, want, request.Request.Contents[1].Parts)
	requireNoSyntheticTurn(t, request)
}

func TestGeminiMessagesThinkingTailContinuationKeepsModelTail(t *testing.T) {
	t.Run("terminal thinking tail follows the same user turn", func(t *testing.T) {
		input := `{"system":"keep instructions","messages":[{"role":"user","content":"inspect"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]}]}`
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
			{Role: "model", Parts: []GeminiPart{{Text: "plan", Thought: true, ThoughtSignature: "real-signature"}}},
		}, request.Request.Contents, "the received thinking tail is the final model turn with no added user turn")
		requireNoSyntheticTurn(t, request)
		require.Contains(t, request.Request.SystemInstruction.Parts, GeminiPart{Text: "keep instructions"})

		// Without the opt-in the same input keeps the strict behavior: the partial
		// tail is dropped at the user boundary instead of being sent back.
		legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		require.Equal(t, []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
		}, legacy.Request.Contents)
		require.Equal(t, legacy.Request.SessionID, request.Request.SessionID, "the continuation stays in the same derived session")
	})

	t.Run("tool result boundary keeps call, result and tail", func(t *testing.T) {
		input := `{"system":"keep instructions","messages":[` +
			`{"role":"system","content":"hoisted instruction"},` +
			`{"role":"user","content":"inspect"},` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"previous plan","signature":"previous-signature"},{"type":"tool_use","id":"call-a","name":"echo","input":{"n":1}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":"actual output"}]},` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"partial plan","signature":"real-signature"}]}]}`
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
			{Role: "model", Parts: []GeminiPart{
				{Text: "previous plan", Thought: true, ThoughtSignature: "previous-signature"},
				{FunctionCall: &GeminiFunctionCall{ID: "call-a", Name: "echo", Args: map[string]any{"n": float64(1)}}, ThoughtSignature: DummyThoughtSignature},
			}},
			{Role: "user", Parts: []GeminiPart{{FunctionResponse: &GeminiFunctionResponse{ID: "call-a", Name: "echo", Response: map[string]any{"result": "actual output"}}}}},
			{Role: "model", Parts: []GeminiPart{{Text: "partial plan", Thought: true, ThoughtSignature: "real-signature"}}},
		}, request.Request.Contents)
		requireNoSyntheticTurn(t, request)
		require.Contains(t, request.Request.SystemInstruction.Parts, GeminiPart{Text: "keep instructions"})
		require.Contains(t, request.Request.SystemInstruction.Parts, GeminiPart{Text: "hoisted instruction"})
	})

	t.Run("blank envelopes and multiple carriers keep order and text", func(t *testing.T) {
		input := `{"messages":[` +
			`{"role":"user","content":"inspect"},` +
			`{"role":"assistant","content":[{"type":"thinking","thinking":"  first thought\n","signature":"signature-one"},{"type":"thinking","thinking":"second thought","signature":"signature-two"}]},` +
			`{"role":"assistant","content":[]},` +
			`{"role":"assistant","content":""}]}`
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
			{Role: "model", Parts: []GeminiPart{
				{Text: "  first thought\n", Thought: true, ThoughtSignature: "signature-one"},
				{Text: "second thought", Thought: true, ThoughtSignature: "signature-two"},
			}},
		}, request.Request.Contents, "each received thought keeps its own signature carrier and position")
		requireNoSyntheticTurn(t, request)
	})

	t.Run("dropped envelopes around the tail keep one model turn", func(t *testing.T) {
		expected := []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "inspect"}}},
			{Role: "model", Parts: []GeminiPart{{Text: "plan", Thought: true, ThoughtSignature: "real-signature"}}},
		}
		for _, tt := range []struct{ name, envelopes string }{
			{name: "empty array before", envelopes: `{"role":"assistant","content":[]},`},
			{name: "blank text before", envelopes: `{"role":"assistant","content":[{"type":"text","text":" \t\n"}]},`},
			{name: "empty string before", envelopes: `{"role":"assistant","content":""},`},
			{name: "trailing empty envelopes", envelopes: `{"role":"assistant","content":[]},{"role":"assistant","content":""},`},
		} {
			t.Run(tt.name, func(t *testing.T) {
				input := `{"messages":[{"role":"user","content":"inspect"},` + tt.envelopes + `{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]}]}`
				request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
				require.NoError(t, err)
				require.Equal(t, expected, request.Request.Contents, "an empty envelope adds no part and no turn")
				requireNoSyntheticTurn(t, request)
			})
		}
	})

	t.Run("ordinary user ended history keeps its policy", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]},{"role":"user","content":"follow up"}]}`
		legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, legacy.Request.Contents, request.Request.Contents)
		require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(request.Request.Contents))
	})

	t.Run("model scope keeps other targets unchanged", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"inspect"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]}]}`
		for _, tt := range []struct {
			name   string
			model  string
			policy GeminiMessagesCompatibilityOptions
		}{
			{name: "allowlist opt out", model: "gemini-3.8-flash", policy: allowThinkingTail("gemini-2.5-flash")},
			{name: "disabled", model: "gemini-3.8-flash", policy: GeminiMessagesCompatibilityOptions{AllowThinkingTailContinuation: true}},
			{name: "Claude target", model: "claude-sonnet-4-5", policy: allowThinkingTail()},
			{name: "Grok target", model: "grok-4", policy: allowThinkingTail()},
			{name: "OpenAI target", model: "gpt-5", policy: allowThinkingTail()},
		} {
			t.Run(tt.name, func(t *testing.T) {
				legacyPolicy := tt.policy
				legacyPolicy.AllowThinkingTailContinuation = false
				legacy, err := transformGeminiMessagesForTest(t, input, tt.model, legacyPolicy)
				require.NoError(t, err)
				request, err := transformGeminiMessagesForTest(t, input, tt.model, tt.policy)
				require.NoError(t, err)
				require.Equal(t, legacy.Request.Contents, request.Request.Contents)
			})
		}
	})
}

// TestGeminiMessagesThinkingTailContinuationCarriers pins what the opt-in forwards:
// the received thinking chunks and their signature carriers, in the received order,
// with no invented thought or signature.
func TestGeminiMessagesThinkingTailContinuationCarriers(t *testing.T) {
	for _, tt := range []struct {
		name     string
		messages string
		want     []GeminiPart
	}{
		{
			name:     "unsigned thought text",
			messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"plan"}]}`,
			want:     []GeminiPart{{Text: "plan", Thought: true}},
		},
		{
			name:     "thought text then separate signature carrier",
			messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"thinking","thinking":"","signature":"real-signature"}]}`,
			want:     []GeminiPart{{Text: "plan", Thought: true}, {Thought: true, ThoughtSignature: "real-signature"}},
		},
		{
			name:     "signature carrier before thought text keeps order",
			messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"carrier-signature"},{"type":"thinking","thinking":"plan"}]}`,
			want:     []GeminiPart{{Thought: true, ThoughtSignature: "carrier-signature"}, {Text: "plan", Thought: true}},
		},
		{
			name:     "dummy carrier is forwarded as received",
			messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"thinking","thinking":"","signature":"` + DummyThoughtSignature + `"}]}`,
			want:     []GeminiPart{{Text: "plan", Thought: true}, {Thought: true, ThoughtSignature: DummyThoughtSignature}},
		},
		{
			name:     "two thinking envelopes aggregate into one model turn",
			messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"first","signature":"signature-one"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"second","signature":"signature-two"}]}`,
			want:     []GeminiPart{{Text: "first", Thought: true, ThoughtSignature: "signature-one"}, {Text: "second", Thought: true, ThoughtSignature: "signature-two"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"messages":[{"role":"user","content":"question"},` + tt.messages + `]}`
			request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
			require.NoError(t, err)
			requireModelTail(t, request, tt.want)
			legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			require.Equal(t, []GeminiContent{{Role: "user", Parts: []GeminiPart{{Text: "question"}}}}, legacy.Request.Contents, "without the opt-in the tail is still dropped")
		})
	}

	t.Run("unsigned thought does not gain a signature", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan"}]}]}`
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		requireModelTail(t, request, []GeminiPart{{Text: "plan", Thought: true}})
		body, err := json.Marshal(request.Request.Contents)
		require.NoError(t, err)
		require.NotContains(t, string(body), "thoughtSignature", "no dummy signature may be invented for a received thought")
		require.NotContains(t, string(body), DummyThoughtSignature)
	})
}

// TestGeminiMessagesThinkingTailContinuationRejectsEmptyCarrier pins the tails the
// opt-in must not continue: a signature without received thinking holds nothing to
// continue from. The opt-in reports that instead of trimming it away and silently
// repeating the original request.
func TestGeminiMessagesThinkingTailContinuationRejectsEmptyCarrier(t *testing.T) {
	for _, tt := range []struct{ name, messages string }{
		{name: "signature only carrier", messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"real-signature"}]}`},
		{name: "blank thinking text with signature", messages: `{"role":"assistant","content":[{"type":"thinking","thinking":" \n\t","signature":"real-signature"}]}`},
		{name: "dummy signature only", messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"` + DummyThoughtSignature + `"}]}`},
		{name: "empty thinking envelope", messages: `{"role":"assistant","content":[{"type":"thinking","thinking":""}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"messages":[{"role":"user","content":"question"},` + tt.messages + `]}`
			body, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
			require.ErrorContains(t, err, "received assistant thinking")
			require.ErrorContains(t, err, "messages[1]")
			var compatibilityErr *GeminiMessagesCompatibilityError
			require.ErrorAs(t, err, &compatibilityErr)
			require.Empty(t, body.Request.Contents, "an ineligible tail is reported instead of silently trimmed")
			require.NotContains(t, err.Error(), "plan")
			require.NotContains(t, err.Error(), "real-signature")
			require.NotContains(t, err.Error(), DummyThoughtSignature)

			legacy, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, legacyErr)
			require.Equal(t, []GeminiContent{{Role: "user", Parts: []GeminiPart{{Text: "question"}}}}, legacy.Request.Contents, "the off path keeps its trim behavior")
		})
	}
}

// TestGeminiMessagesThinkingTailContinuationBoundaries pins the inputs that keep
// their pre-existing behavior with the opt-in on.
func TestGeminiMessagesThinkingTailContinuationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name     string
		messages string
		want     string
	}{
		{name: "mixed tool tail", messages: `{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"},{"type":"tool_use","id":"a","name":"echo","input":{}}]}`, want: "assistant text prefills"},
		{name: "redacted thinking", messages: `{"role":"assistant","content":[{"type":"redacted_thinking","data":"private","signature":"real-signature"}]}`, want: "supported by Gemini Messages"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"messages":[{"role":"user","content":"question"},` + tt.messages + `]}`
			_, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
			require.ErrorContains(t, legacyErr, tt.want)
			body, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
			require.ErrorContains(t, err, tt.want, "the opt-in does not widen these shapes")
			require.Empty(t, body.Request.Contents)
		})
	}

	t.Run("no user turn before the model tail", func(t *testing.T) {
		input := `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]}]}`
		_, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.ErrorContains(t, legacyErr, "nonempty assistant text")
		body, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.ErrorContains(t, err, "nonempty assistant text", "a model tail still needs the preceding user turn")
		require.Empty(t, body.Request.Contents)
	})

	t.Run("model turn between user and thinking tail", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":"earlier answer"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]}]}`
		_, legacyErr := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.ErrorContains(t, legacyErr, "nonempty assistant text")
		body, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.ErrorContains(t, err, "nonempty assistant text", "adjacent model turns are not created")
		require.Empty(t, body.Request.Contents)
	})

	// A mixed tail that carries visible text keeps the documented text-prefill
	// continuation; only a thinking-only tail skips the synthetic user turn.
	t.Run("text prefill keeps its continuation instruction", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"},{"type":"text","text":"prefix:"}]}]}`
		legacy, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", GeminiMessagesCompatibilityOptions{Enabled: true})
		require.NoError(t, err)
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, legacy.Request.Contents, request.Request.Contents)
		require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(request.Request.Contents))
		require.Contains(t, request.Request.Contents[2].Parts[0].Text, "[sub2api:assistant-prefill]")
		require.Equal(t, "prefix:", request.Request.Contents[1].Parts[len(request.Request.Contents[1].Parts)-1].Text)
	})

	t.Run("terminal user input is untouched", func(t *testing.T) {
		input := `{"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"real-signature"}]},{"role":"user","content":[]}]}`
		request, err := transformGeminiMessagesForTest(t, input, "gemini-3.8-flash", allowThinkingTail())
		require.NoError(t, err)
		require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(request.Request.Contents))
		require.Equal(t, geminiEmptyUserMessage, request.Request.Contents[2].Parts[0].Text)
	})
}
