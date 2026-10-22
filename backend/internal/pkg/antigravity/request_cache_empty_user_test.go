package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// An empty user message is a real compatibility turn, not a disposable tail.
// Once sent, its representation must survive becoming history on the next call.
func TestRequestCacheEmptyUserSurvivesHistoryGrowth(t *testing.T) {
	for _, empty := range []string{`""`, `"  "`, `[]`, `[{"type":"text","text":" "}]`} {
		t.Run(empty, func(t *testing.T) {
			var req ClaudeRequest
			require.NoError(t, json.Unmarshal([]byte(`{"model":"alias","system":"stable","messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"thinking","thinking":"received","signature":"real-signature"},{"type":"text","text":"answer"}]},{"role":"user","content":`+empty+`}]}`), &req))
			opts := DefaultTransformOptions()
			opts.GeminiMessages.Enabled = true
			transform := func() V1InternalRequest {
				t.Helper()
				body, err := TransformClaudeToGeminiWithOptions(&req, "project", "gemini-4-argon-high", opts)
				require.NoError(t, err)
				var wire V1InternalRequest
				require.NoError(t, json.Unmarshal(body, &wire))
				return wire
			}
			first := transform()
			require.Len(t, first.Request.Contents, 3)
			require.Equal(t, geminiEmptyUserMessage, first.Request.Contents[2].Parts[0].Text)
			req.Messages = append(req.Messages, ClaudeMessage{Role: "assistant", Content: json.RawMessage(`"next answer"`)}, ClaudeMessage{Role: "user", Content: json.RawMessage(`"next question"`)})
			next := transform()
			require.Equal(t, first.Request.SystemInstruction, next.Request.SystemInstruction)
			require.Equal(t, first.Request.Tools, next.Request.Tools)
			require.Equal(t, first.Request.Contents, next.Request.Contents[:len(first.Request.Contents)], "append-only caller history must not drop a previously sent empty-user turn")
			require.Equal(t, "real-signature", next.Request.Contents[1].Parts[0].ThoughtSignature)
		})
	}
}

func TestRequestCacheHistoryDoesNotSynthesizeUnsupportedEmptyParts(t *testing.T) {
	opts := DefaultTransformOptions()
	opts.GeminiMessages.Enabled = true
	var req ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"alias","messages":[{"role":"user","content":[{"type":"unsupported"}]},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`), &req))
	body, err := TransformClaudeToGeminiWithOptions(&req, "project", "gemini-4-argon-high", opts)
	require.NoError(t, err)
	var wire V1InternalRequest
	require.NoError(t, json.Unmarshal(body, &wire))
	require.Len(t, wire.Request.Contents, 2, "existing unsupported historical policy is not an empty-user turn")
}
