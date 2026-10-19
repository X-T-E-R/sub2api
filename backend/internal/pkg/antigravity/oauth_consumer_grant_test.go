//go:build unit

package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildAuthorizationURL_ConsumerGrantMatchesNativeCLI(t *testing.T) {
	parsed, err := url.Parse(BuildAuthorizationURL("consumer-state", "consumer-challenge"))
	require.NoError(t, err)
	q := parsed.Query()
	// Native consumer auth requests both aicode and OpenID in addition to the
	// existing desktop scopes. Compare the actual URL, not the Scopes constant.
	expected := []string{
		"https://www.googleapis.com/auth/cloud-platform",
		"https://www.googleapis.com/auth/userinfo.email",
		"https://www.googleapis.com/auth/userinfo.profile",
		"https://www.googleapis.com/auth/cclog",
		"https://www.googleapis.com/auth/experimentsandconfigs",
		"https://www.googleapis.com/auth/aicode",
		"openid",
	}
	actual := strings.Fields(q.Get("scope"))
	require.ElementsMatch(t, expected, actual)
	require.Len(t, actual, len(expected))
	require.Equal(t, "https", parsed.Scheme)
	require.Equal(t, "accounts.google.com", parsed.Host)
	require.Equal(t, "/o/oauth2/v2/auth", parsed.Path)
	require.Equal(t, ClientID, q.Get("client_id"))
	require.Equal(t, RedirectURI, q.Get("redirect_uri"))
	require.Equal(t, "consumer-state", q.Get("state"))
	require.Equal(t, "consumer-challenge", q.Get("code_challenge"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, "offline", q.Get("access_type"))
	require.Equal(t, "consent", q.Get("prompt"))
	require.Equal(t, "true", q.Get("include_granted_scopes"))
}

func TestRefreshToken_ConsumerScopeUpdateDoesNotRequestNewGrant(t *testing.T) {
	old := defaultClientSecret
	defaultClientSecret = "synthetic-secret"
	t.Cleanup(func() { defaultClientSecret = old })
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.NoError(t, r.ParseForm())
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, url.Values{
			"client_id":     {ClientID},
			"client_secret": {"synthetic-secret"},
			"refresh_token": {"existing-synthetic-refresh"},
			"grant_type":    {"refresh_token"},
		}, r.PostForm)
		require.NotContains(t, r.PostForm, "scope")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "synthetic-access", ExpiresIn: 3600, TokenType: "Bearer"})
	}))
	defer server.Close()
	client := newTestClientWithRedirect(map[string]string{TokenURL: server.URL})
	got, err := client.RefreshToken(context.Background(), "existing-synthetic-refresh")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, "synthetic-access", got.AccessToken)
}
