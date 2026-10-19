package antigravity

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeUserAgentOverride(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		invalid           bool
	}{
		{"empty", "", "", false},
		{"trim", "  independent-client/9 (custom; arm64)  ", "independent-client/9 (custom; arm64)", false},
		{"clear", "   ", "", false},
		{"limit", strings.Repeat("a", 512), strings.Repeat("a", 512), false},
		{"too long", strings.Repeat("a", 513), "", true},
		{"crlf at edges", "\r\nclient\r\n", "", true},
		{"injected header", "client\r\nX-Other: value", "", true},
		{"tab", "client\tvalue", "", true},
		{"control", "client\x00", "", true},
		{"delete", "client\x7f", "", true},
		{"non ascii", "客户端/1", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeUserAgentOverride(tc.value)
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUserAgentOverridePrecedenceAndAPIRequest(t *testing.T) {
	userAgentVersionMu.RLock()
	oldUA, oldVersion := userAgentResolver, userAgentVersionResolver
	userAgentVersionMu.RUnlock()
	t.Cleanup(func() {
		SetUserAgentResolver(oldUA)
		SetUserAgentVersionResolver(oldVersion)
	})
	SetUserAgentVersionResolver(func(context.Context) string { return "1.23.2" })
	value := "  arbitrary-client/7 (custom profile)  "
	SetUserAgentResolver(func(ctx context.Context) string {
		require.NotNil(t, ctx)
		return value
	})
	for _, action := range []string{"streamGenerateContent", "fetchAvailableModels", "loadCodeAssist"} {
		req, err := NewAPIRequestWithURL(context.Background(), "https://example.invalid", action, "fixture-token", nil)
		require.NoError(t, err)
		require.Equal(t, "arbitrary-client/7 (custom profile)", req.Header.Get("User-Agent"))
	}
	require.Equal(t, "1.23.2", GetUserAgentVersionForContext(context.Background()), "full override does not change numeric metadata")
	value = ""
	require.Equal(t, "antigravity/hub/1.23.2 darwin/arm64", GetUserAgentForContext(context.Background()))
	value = "bad\r\nheader"
	require.Equal(t, "antigravity/hub/1.23.2 darwin/arm64", GetUserAgent())
}
