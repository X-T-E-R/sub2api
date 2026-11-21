package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// Parsed scalar fields are stored beyond the request (usage tasks, usage-log batch
// queues), so they must own their bytes instead of aliasing the request body.
func TestParseGatewayRequestScalarsDoNotAliasBody(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-6","stream":true,"speed":"fast","metadata":{"user_id":"user_abc_session_1"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	require.NoError(t, err)
	for i := range body {
		if body[i] >= 'a' && body[i] <= 'z' {
			body[i] = 'X'
		}
	}
	require.Equal(t, "claude-opus-4-6", parsed.Model)
	require.Equal(t, "user_abc_session_1", parsed.MetadataUserID)
	require.Equal(t, "high", parsed.OutputEffort)
	require.Equal(t, "fast", parsed.Speed)
}

func TestAntigravityMalformedRetryKeyIgnoresWhitespaceAndTracksHistory(t *testing.T) {
	mk := func(system, content string) *antigravity.ClaudeRequest {
		return &antigravity.ClaudeRequest{
			Model:    "gemini-3-pro-high",
			System:   json.RawMessage(system),
			Tools:    []antigravity.ClaudeTool{{Name: "Read", InputSchema: map[string]any{"type": "object"}}},
			Messages: []antigravity.ClaudeMessage{{Role: "user", Content: json.RawMessage(content)}},
		}
	}
	base, err := antigravityMalformedRetryKey(7, "s1", "gemini-3-pro-high", mk(`"sys"`, `[{"type":"text","text":"a"}]`))
	require.NoError(t, err)
	spaced, err := antigravityMalformedRetryKey(7, "s1", "gemini-3-pro-high", mk(` "sys" `, "[ {\"type\": \"text\",\n \"text\": \"a\"} ]"))
	require.NoError(t, err)
	require.Equal(t, base, spaced, "formatting-only differences must reuse the failure entry")

	changed, err := antigravityMalformedRetryKey(7, "s1", "gemini-3-pro-high", mk(`"sys"`, `[{"type":"text","text":"b"}]`))
	require.NoError(t, err)
	require.NotEqual(t, base, changed)
	// Field boundaries are length-prefixed: moving bytes between fields changes the key.
	shifted, err := antigravityMalformedRetryKey(7, "s", "1gemini-3-pro-high", mk(`"sys"`, `[{"type":"text","text":"a"}]`))
	require.NoError(t, err)
	require.NotEqual(t, base, shifted)

	_, err = antigravityMalformedRetryKey(7, "s1", "gemini-3-pro-high", mk(`"sys"`, `[{"type":`))
	require.Error(t, err, "invalid history JSON must still fail like the previous json.Marshal path")
}
