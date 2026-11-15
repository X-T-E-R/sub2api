package antigravity

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetadataSessionIDNeverExposesExternalIdentity(t *testing.T) {
	for _, id := range []string{"user_fixture_account_fixture_session_fixture", `{"device_id":"fixture","session_id":"fixture"}`, "legacy-user-id", "-0", "-9223372036854775808", "-+12", "-12x"} {
		t.Run(id, func(t *testing.T) {
			got := metadataSessionID(id)
			require.NotEqual(t, id, got)
			require.Equal(t, got, metadataSessionID(id))
			n, err := strconv.ParseInt(got, 10, 64)
			require.NoError(t, err)
			require.Negative(t, n)
		})
	}
	for _, id := range []string{"-1", "-123456789", "-9223372036854775807"} {
		require.Equal(t, id, metadataSessionID(id))
	}
}

func TestTransformSessionIdentityPrecedence(t *testing.T) {
	for _, target := range []string{"gemini-3-flash", "claude-sonnet-4-5"} {
		t.Run(target, func(t *testing.T) {
			req := &ClaudeRequest{Model: target, MaxTokens: 64,
				Messages: []ClaudeMessage{{Role: "user", Content: json.RawMessage(`"fixture"`)}},
				Metadata: &ClaudeMetadata{UserID: "user_fixture_account_fixture_session_fixture"},
			}
			for _, explicit := range []string{"", "-314159"} {
				body, err := TransformClaudeToGeminiWithOptions(req, "project", target, TransformOptions{SessionID: explicit})
				require.NoError(t, err)
				var wire V1InternalRequest
				require.NoError(t, json.Unmarshal(body, &wire))
				want := metadataSessionID(req.Metadata.UserID)
				if explicit != "" {
					want = explicit
				}
				require.Equal(t, want, wire.Request.SessionID)
				require.NotContains(t, string(body), req.Metadata.UserID)
			}
		})
	}
}
