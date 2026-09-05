package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Relations below come from native Codex responses_headers.rs (same turn/time
// across tool steps), prompt_cache_key.rs (shared tree session/cache, distinct
// child thread and request-id alias), and responses_metadata.rs (canonical body).
// Expected IDs are deliberately not calculated with the production mapper.
func identityFixture(t *testing.T, session, thread, turn string, window int, refs map[string]any) map[string]any {
	t.Helper()
	metadata := map[string]any{
		"installation_id": "installation-a", "session_id": session,
		"thread_id": thread, "turn_id": turn, "window_id": fmt.Sprintf("%s:%d", thread, window),
		"window_number": window, "context_window_id": fmt.Sprintf("context-%s-%d", thread, window),
		"turn_started_at_unix_ms": int64(1700000000000), "sandbox": "fixture",
	}
	for k, v := range refs {
		metadata[k] = v
	}
	raw, err := json.Marshal(metadata)
	require.NoError(t, err)
	return map[string]any{"prompt_cache_key": session, "client_metadata": map[string]any{
		"session_id": session, "thread_id": thread, "x-client-request-id": thread,
		openAIWSTurnMetadataHeader: string(raw),
	}}
}

func identityContext(t *testing.T, headers http.Header) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if headers != nil {
		c.Request.Header = headers.Clone()
	}
	c.Set("api_key", &APIKey{ID: 77})
	return c
}

func projectedMetadata(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	metadata := codexMetadataObject(codexIdentityMetadata(body)[openAIWSTurnMetadataHeader])
	require.NotNil(t, metadata)
	return metadata
}

func TestCodexIdentityProjectionGraphAndLifetimes(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: mode})
			account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
			project := func(body map[string]any) (map[string]any, http.Header) {
				c := identityContext(t, nil)
				projectCodexRequestBody(c, account, body)
				h := http.Header{}
				applyCodexRequestHeaders(c, account, h)
				m := projectedMetadata(t, body)
				require.Equal(t, m["thread_id"], h.Get("thread-id"))
				require.Equal(t, m["thread_id"], h.Get("x-client-request-id"))
				require.Equal(t, m["session_id"], h.Get("session_id"))
				return m, h
			}
			rootBody := identityFixture(t, "tree-a", "tree-a", "turn-root", 0, nil)
			root, _ := project(rootBody)
			for i := 0; i < 50; i++ {
				repeated, _ := project(identityFixture(t, "tree-a", "tree-a", "turn-root", 0, nil))
				require.Equal(t, root, repeated, "same logical turn across HTTP/tool retries")
			}
			childBody := identityFixture(t, "tree-a", "child-a", "turn-child", 0, map[string]any{
				"parent_thread_id": "tree-a", "root_thread_id": "tree-a", "forked_from_thread_id": "tree-a",
				"parent_turn_id": "turn-root", "root_turn_id": "turn-root", "forked_from_ordinal_exclusive": 7,
			})
			child, childHeaders := project(childBody)
			sibling, _ := project(identityFixture(t, "tree-a", "child-b", "turn-sibling", 0, nil))
			secondBody := identityFixture(t, "tree-b", "tree-b", "turn-second", 0, nil)
			second, _ := project(secondBody)
			require.Equal(t, root["session_id"], child["session_id"])
			require.Equal(t, rootBody["prompt_cache_key"], childBody["prompt_cache_key"])
			require.NotEqual(t, rootBody["prompt_cache_key"], secondBody["prompt_cache_key"], "convergence never merges cache trees")
			for _, key := range []string{"parent_thread_id", "root_thread_id", "forked_from_thread_id"} {
				require.Equal(t, root["thread_id"], child[key], key)
			}
			require.Equal(t, root["thread_id"], childHeaders.Get("x-codex-parent-thread-id"))
			require.Equal(t, root["turn_id"], child["parent_turn_id"])
			require.Equal(t, root["turn_id"], child["root_turn_id"])
			require.NotEqual(t, root["turn_id"], child["turn_id"])
			if mode == "full" {
				require.Equal(t, root["thread_id"], child["thread_id"])
				require.Equal(t, root["thread_id"], second["thread_id"])
			} else {
				require.NotEqual(t, root["thread_id"], child["thread_id"])
				require.NotEqual(t, child["thread_id"], sibling["thread_id"])
				require.NotEqual(t, root["thread_id"], second["thread_id"])
			}
			if mode == "off" || mode == "device" {
				require.NotEqual(t, root["session_id"], second["session_id"])
				require.Equal(t, root["session_id"], root["thread_id"])
			} else {
				require.Equal(t, root["session_id"], second["session_id"])
			}
			next, _ := project(identityFixture(t, "tree-a", "child-a", "turn-new", 1, nil))
			require.NotEqual(t, child["turn_id"], next["turn_id"])
			require.Equal(t, child["thread_id"], next["thread_id"])
			require.Equal(t, next["thread_id"].(string)+":1", next["window_id"])
			require.Equal(t, float64(1), next["window_number"])
			require.NotEqual(t, child["context_window_id"], next["context_window_id"])
			require.Equal(t, float64(1700000000000), next["turn_started_at_unix_ms"])
		})
	}
}

func TestCodexIdentityProjectionCanonicalCarriersAndBuilders(t *testing.T) {
	account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "session"})
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	body := identityFixture(t, "tree-a", "child-a", "turn-a", 2, nil)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	h := http.Header{}
	h.Set("thread-id", "stale-flat-thread")
	h.Set("session-id", "stale-flat-session")
	h.Set(openAIWSTurnMetadataHeader, `{"thread_id":"stale-embedded-thread","turn_id":"stale-turn","turn_started_at_unix_ms":1}`)
	h.Set(openAIWSTurnStateHeader, "opaque-state")
	c := identityContext(t, h)
	projectCodexRequestBody(c, account, body)
	want := projectedMetadata(t, body)
	rawBody, changed, err := projectCodexRequestBodyRaw(c, account, raw)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &decoded))
	require.Equal(t, want, projectedMetadata(t, decoded))
	svc := &OpenAIGatewayService{}
	normal, err := svc.buildUpstreamRequest(context.Background(), c, account, rawBody, "token", true, "tree-a", true)
	require.NoError(t, err)
	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, rawBody, "token")
	require.NoError(t, err)
	ws, _, err := svc.buildOpenAIWSHeaders(context.Background(), c, account, "token", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, true, "opaque-state", h.Get(openAIWSTurnMetadataHeader), "tree-a", "", "")
	require.NoError(t, err)
	for _, headers := range []http.Header{normal.Header, passthrough.Header, ws} {
		require.Equal(t, want["thread_id"], headers.Get("thread-id"))
		require.Equal(t, want["thread_id"], headers.Get("x-client-request-id"))
		require.Equal(t, want["turn_id"], codexMetadataObject(headers.Get(openAIWSTurnMetadataHeader))["turn_id"])
		require.Equal(t, want["turn_started_at_unix_ms"], codexMetadataObject(headers.Get(openAIWSTurnMetadataHeader))["turn_started_at_unix_ms"])
		require.Equal(t, "opaque-state", headers.Get(openAIWSTurnStateHeader))
	}
	require.Equal(t, normal.Header.Get("conversation_id"), passthrough.Header.Get("conversation_id"), "raw/decoded routing must not double-scope cache keys")
	require.Empty(t, ws.Get("conversation_id"), "WS does not invent an absent conversation header")
	require.Equal(t, h, c.Request.Header, "never mutate inbound headers")
	clean := identityContext(t, nil)
	cleanBody := identityFixture(t, "tree-a", "child-a", "turn-a", 2, nil)
	projectCodexRequestBody(clean, account, cleanBody)
	require.Equal(t, want, projectedMetadata(t, cleanBody), "stale compatibility projections cannot change canonical identity")
}

func TestCodexIdentityProjectionWSFramesAndOriginalRetries(t *testing.T) {
	account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "session"})
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	h := http.Header{}
	h.Set(openAIWSTurnMetadataHeader, `{"thread_id":"handshake-thread","turn_id":"handshake-turn","turn_started_at_unix_ms":1}`)
	c := identityContext(t, h)
	var previous map[string]any
	for _, turn := range []string{"turn-a", "turn-b"} {
		original := identityFixture(t, "tree-a", "child-a", turn, 3, nil)
		before, err := json.Marshal(original)
		require.NoError(t, err)
		setOpenAIWSTurnMetadata(original, h.Get(openAIWSTurnMetadataHeader))
		after, err := json.Marshal(original)
		require.NoError(t, err)
		require.Equal(t, before, after, "WS setup must retain body snapshot")
		var first []byte
		for attempt := 0; attempt < 3; attempt++ {
			projected, _, err := projectCodexWSRequestBodyRaw(c, account, before)
			require.NoError(t, err)
			if first == nil {
				first = projected
			} else {
				require.JSONEq(t, string(first), string(projected))
			}
		}
		var output map[string]any
		require.NoError(t, json.Unmarshal(first, &output))
		current := projectedMetadata(t, output)
		if previous != nil {
			require.NotEqual(t, previous["turn_id"], current["turn_id"])
			require.Equal(t, previous["thread_id"], current["thread_id"])
		}
		previous = current
	}
}

func TestCodexIdentityProjectionCredentialAliasesAndFailover(t *testing.T) {
	first := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "session"})
	first.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	alias := newTestOAuthAccount(11, map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222"})
	alias.Credentials = first.Credentials
	other := newTestOAuthAccount(12, map[string]any{codexFingerprintModeExtraKey: "off"})
	other.Credentials = map[string]any{"chatgpt_account_id": "credential-b"}
	parentID := first.ID
	shadow := newTestOAuthAccount(13, map[string]any{codexFingerprintModeExtraKey: "session"})
	shadow.ParentAccountID = &parentID
	svc := &OpenAIGatewayService{accountRepo: &codexAccountIdentityRepoStub{account: first}}
	c := identityContext(t, nil)
	var snapshots []map[string]any
	for _, account := range []*Account{first, alias, shadow, other, first} {
		_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, account)
		require.NoError(t, err)
		body := identityFixture(t, "tree-a", "child-a", "turn-a", 1, nil)
		projectCodexRequestBody(c, account, body)
		snapshots = append(snapshots, projectedMetadata(t, body))
	}
	require.Equal(t, snapshots[0], snapshots[1], "credential aliases ignore local seed differences")
	require.Equal(t, snapshots[0], snapshots[2], "shadow uses resolved credential identity")
	require.Equal(t, snapshots[0], snapshots[4], "A-B-A from originals has no double mapping")
	for _, key := range []string{"session_id", "thread_id", "turn_id", "window_id", "installation_id"} {
		require.NotEqual(t, snapshots[0][key], snapshots[3][key], key)
	}
	c.Set("api_key", &APIKey{ID: 78})
	body := identityFixture(t, "tree-a", "child-a", "turn-a", 1, nil)
	projectCodexRequestBody(c, first, body)
	require.NotEqual(t, snapshots[0]["thread_id"], projectedMetadata(t, body)["thread_id"])
	// Intentional session convergence is account-global, but cache and real
	// threads retain downstream API-key isolation.
	require.Equal(t, snapshots[0]["session_id"], projectedMetadata(t, body)["session_id"])
	shadow.Extra = nil
	_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, shadow)
	require.NoError(t, err)
	unconfigured := identityFixture(t, "tree-a", "tree-a", "turn-a", 0, nil)
	projectCodexRequestBody(c, shadow, unconfigured)
	unconfiguredMetadata := projectedMetadata(t, unconfigured)
	require.Equal(t, unconfiguredMetadata["session_id"], unconfiguredMetadata["thread_id"], "unconfigured shadow stays off even when parent opts into session")
	require.NotEqual(t, snapshots[0]["session_id"], unconfiguredMetadata["session_id"])
}

func TestCodexIdentityProjectionMissingDefaultsAndCompact(t *testing.T) {
	for _, mode := range []string{"", "invalid", "off", "device", "session", "full"} {
		account := &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexFingerprintModeExtraKey: mode}}
		c := identityContext(t, nil)
		body := map[string]any{"input": "fixture"}
		require.False(t, projectCodexRequestBody(c, account, body))
		h := http.Header{}
		applyCodexRequestHeaders(c, account, h)
		require.Empty(t, h, "missing logical IDs and missing seed cannot create identities")
		c.Request.URL.Path = "/v1/responses/compact"
		for i := 0; i < 3; i++ {
			require.Empty(t, resolveOpenAICompactSessionID(c))
		}
		c.Request.Header.Set("session-id", "tree-a")
		require.Equal(t, "tree-a", resolveOpenAICompactSessionID(c))
	}
	account := &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := identityFixture(t, "tree-a", "child-a", "turn-a", 0, nil)
	before, _ := json.Marshal(body)
	require.False(t, projectCodexRequestBody(identityContext(t, nil), account, body))
	after, _ := json.Marshal(body)
	require.Equal(t, before, after, "API-key accounts retain original metadata")
}

func TestCodexIdentityProjectionCanonicalAbsenceAndCacheGrouping(t *testing.T) {
	account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "session"})
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	h := http.Header{}
	h.Set("x-codex-parent-thread-id", "stale-parent")
	h.Set("parent-turn-id", "stale-parent-turn")
	h.Set(openAIWSTurnMetadataHeader, `{"parent_thread_id":"stale-parent","forked_from_thread_id":"stale-fork","root_turn_id":"stale-root","turn_id":"stale-turn","turn_started_at_unix_ms":1}`)
	c := identityContext(t, h)
	body := map[string]any{"client_metadata": map[string]any{openAIWSTurnMetadataHeader: `{"session_id":"root","thread_id":"root","request_kind":"compact"}`, "parent_turn_id": "stale-parent-turn"}}
	projectCodexRequestBody(c, account, body)
	applyCodexRequestHeaders(c, account, h)
	for _, m := range []map[string]any{projectedMetadata(t, body), codexMetadataObject(h.Get(openAIWSTurnMetadataHeader)), codexIdentityMetadata(body)} {
		for _, key := range []string{"parent_thread_id", "parent_turn_id", "forked_from_thread_id", "root_turn_id", "turn_id", "turn_started_at_unix_ms"} {
			require.NotContains(t, m, key, "canonical absence is not a request to inherit stale references")
		}
	}
	require.Empty(t, h.Get("x-codex-parent-thread-id"))
	require.Empty(t, h.Get("parent-turn-id"))
	defaultKey := identityFixture(t, "tree-a", "tree-a", "turn-a", 0, nil)
	explicitKey := identityFixture(t, "tree-b", "tree-b", "turn-b", 0, nil)
	explicitKey["prompt_cache_key"] = "tree-a"
	projectCodexRequestBody(c, account, defaultKey)
	projectCodexRequestBody(c, account, explicitKey)
	require.Equal(t, defaultKey["prompt_cache_key"], explicitKey["prompt_cache_key"], "equal original cache keys keep their grouping regardless of session default")
	// Native Memory omits canonical core identity while client_metadata()
	// still emits base flat compatibility fields (responses_metadata.rs).
	memory := map[string]any{"client_metadata": map[string]any{"session_id": "root", "thread_id": "root", "x-codex-window-id": "root:0", openAIWSTurnMetadataHeader: `{"request_kind":"memory","sandbox":"fixture"}`}}
	projectCodexRequestBody(identityContext(t, nil), account, memory)
	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
		require.NotContains(t, projectedMetadata(t, memory), key)
	}
	require.NotEqual(t, "root", codexIdentityMetadata(memory)["thread_id"], "flat compatibility identity remains scoped")
}

func TestCodexIdentityProjectionFallbackNoSeedAndCompactBuilders(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 10, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	c := identityContext(t, http.Header{"Session_id": []string{"tree-a"}})
	raw := []byte(`{"model":"gpt-5.1","prompt_cache_key":"tree-a"}`)
	projected, _, err := projectCodexRequestBodyRaw(c, account, raw)
	require.NoError(t, err)
	request, err := svc.buildUpstreamRequest(context.Background(), c, account, projected, "token", true, "tree-a", true)
	require.NoError(t, err)
	require.Equal(t, isolateOpenAISessionID(77, "tree-a"), request.Header.Get("session_id"), "missing credential identity must not undo existing downstream key isolation")
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	var off map[string]any
	for _, mode := range []string{"off", "", "invalid", "device", "session", "full"} {
		account.Extra = map[string]any{codexFingerprintModeExtraKey: mode}
		body := identityFixture(t, "tree-a", "child-a", "turn-a", 1, nil)
		projectCodexRequestBody(c, account, body)
		if off == nil {
			off = projectedMetadata(t, body)
		} else {
			require.Equal(t, off, projectedMetadata(t, body), "optional mode without a seed remains off")
		}
	}
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		c.Request.URL.Path = path
		c.Request.Header.Set("session-id", "tree-a")
		c.Request.Header.Set("thread-id", "child-a")
		c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"tree-a","thread_id":"child-a","turn_id":"turn-a","window_id":"child-a:3","turn_started_at_unix_ms":1700000000000}`)
		stageCodexRequestIdentity(c, account, nil)
		first, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(`{"model":"gpt-5.1"}`), "token", true, "", true)
		require.NoError(t, err)
		second, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, []byte(`{"model":"gpt-5.1"}`), "token")
		require.NoError(t, err)
		require.Equal(t, first.Header.Get("session_id"), second.Header.Get("session_id"))
		require.Equal(t, first.Header.Get("thread-id"), second.Header.Get("thread-id"))
		require.Equal(t, first.Header.Get("x-codex-turn-metadata"), second.Header.Get("x-codex-turn-metadata"))
		require.Equal(t, first.Header.Get("thread-id")+":3", first.Header.Get("x-codex-window-id"))
	}
	// The same setup credential remains one namespace even if two rows have
	// different optional fingerprint seeds.
	a := newTestOAuthAccount(20, map[string]any{codexFingerprintModeExtraKey: "device"})
	a.Type, a.Credentials = AccountTypeSetupToken, map[string]any{"access_token": "fixture-setup-token"}
	b := *a
	b.Extra = map[string]any{codexFingerprintModeExtraKey: "device", codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222"}
	require.Equal(t, codexAccountIdentityNamespace(a), codexAccountIdentityNamespace(&b))
}

func TestCodexIdentityProjectionCompatibilityEntrypoints(t *testing.T) {
	account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "session"})
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a", "access_token": "fixture-token"}
	account.Concurrency = 1
	var first http.Header
	for _, route := range []string{"messages", "chat"} {
		c := identityContext(t, nil)
		c.Request.Header.Set("session-id", "tree-a")
		c.Request.Header.Set("thread-id", "child-a")
		c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"session_id":"tree-a","thread_id":"child-a","turn_id":"turn-a","window_id":"child-a:2","turn_started_at_unix_ms":1700000000000}`)
		upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_fixture", "gpt-5.4")}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
		body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"fixture"}],"stream":false}`)
		var err error
		if route == "messages" {
			_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "tree-a", "gpt-5.4")
		} else {
			_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "tree-a", "gpt-5.4")
		}
		require.NoError(t, err)
		require.NotNil(t, upstream.lastReq)
		h := upstream.lastReq.Header
		m := codexMetadataObject(h.Get(openAIWSTurnMetadataHeader))
		require.NotNil(t, m)
		require.Equal(t, h.Get("session-id"), h.Get("session_id"), "Messages final UUID restoration must not split projected session aliases")
		require.Equal(t, h.Get("thread-id"), h.Get("x-client-request-id"))
		require.Equal(t, m["thread_id"], h.Get("thread-id"))
		if first == nil {
			first = h
		} else {
			require.Equal(t, first.Get("thread-id"), h.Get("thread-id"))
			require.Equal(t, first.Get("session-id"), h.Get("session-id"))
			require.Equal(t, codexMetadataObject(first.Get(openAIWSTurnMetadataHeader))["turn_id"], m["turn_id"])
		}
	}
}

func TestCodexIdentityProjectionSnapshotAndMalformedCarriers(t *testing.T) {
	account := newTestOAuthAccount(10, map[string]any{codexFingerprintModeExtraKey: "off"})
	account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
	original := identityFixture(t, "tree-a", "child-a", "turn-a", 1, nil)
	cm := codexIdentityMetadata(original)
	cm["turn_started_at_unix_ms"] = "stale-time"
	before := cm[openAIWSTurnMetadataHeader]
	p := newCodexRequestIdentity(account, account, 77, nil, original)
	out := map[string]any{"client_metadata": cm, "prompt_cache_key": "tree-a"}
	require.True(t, p.applyBody(out))
	require.Equal(t, "1700000000000", codexIdentityMetadata(out)["turn_started_at_unix_ms"])
	require.Equal(t, before, cm[openAIWSTurnMetadataHeader], "projection clones mutable carrier maps")
	require.False(t, p.applyBody(out), "reapplying one immutable projection is idempotent")
	for _, raw := range []string{`[]`, `null`, `"scalar"`, `{"input":[],"client_metadata":null}`, `{"input":[],"client_metadata":{"x-codex-turn-metadata":"{broken"}}`} {
		c := identityContext(t, nil)
		next, changed, err := projectCodexRequestBodyRaw(c, account, []byte(raw))
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, raw, string(next), "no identities can be inferred from malformed/missing carriers")
	}
	// A malformed full blob is not a partially decoded canonical snapshot.
	h := http.Header{}
	h.Set("thread-id", "fallback-thread")
	c := identityContext(t, h)
	body := map[string]any{"client_metadata": map[string]any{openAIWSTurnMetadataHeader: `{"thread_id":"partial",`}}
	projectCodexRequestBody(c, account, body)
	clean := map[string]any{"client_metadata": map[string]any{"thread_id": "fallback-thread"}}
	projectCodexRequestBody(identityContext(t, nil), account, clean)
	require.Equal(t, codexIdentityMetadata(clean)["thread_id"], codexIdentityMetadata(body)["thread_id"])
}

// The mock replies only after a real forwarded frame, so passthrough's two
// concurrent relay pumps cannot complete turns before the client sends them.
type codexIdentityFrameConn struct {
	openAIWSCaptureConn
	replies chan []byte
}

func (c *codexIdentityFrameConn) WriteJSON(ctx context.Context, payload any) error {
	if err := c.openAIWSCaptureConn.WriteJSON(ctx, payload); err != nil {
		return err
	}
	select {
	case c.replies <- []byte(`{"type":"response.completed","response":{"id":"resp_fixture","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *codexIdentityFrameConn) WriteFrame(ctx context.Context, _ coderws.MessageType, payload []byte) error {
	return c.WriteJSON(ctx, json.RawMessage(payload))
}

func (c *codexIdentityFrameConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case reply := <-c.replies:
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *codexIdentityFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	payload, err := c.ReadMessage(ctx)
	return coderws.MessageText, payload, err
}

func TestCodexIdentityProjectionRealWSIngressFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, ingress := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(string(ingress), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			capture := &codexIdentityFrameConn{replies: make(chan []byte, 1)}
			dialer := &openAIWSSingleConnDialer{conn: capture}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, toolCorrector: NewCodexToolCorrector(), openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, openaiWSPassthroughDialer: dialer}
			account := newTestOAuthAccount(30, map[string]any{codexFingerprintModeExtraKey: "session", "openai_oauth_responses_websockets_v2_mode": ingress})
			account.Credentials = map[string]any{"chatgpt_account_id": "credential-a"}
			account.Concurrency = 1
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer conn.CloseNow()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r.Clone(r.Context())
				c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"thread_id":"stale-thread","turn_id":"stale-turn","turn_started_at_unix_ms":1}`)
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				defer cancel()
				_, first, err := conn.Read(ctx)
				if err != nil {
					done <- err
					return
				}
				done <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "fixture-token", first, nil)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			for i, turn := range []string{"turn-a", "turn-a", "turn-b"} {
				body := identityFixture(t, "tree-a", "child-a", turn, 2, nil)
				body["type"], body["model"], body["input"] = "response.create", "gpt-5.1", []any{}
				if i == 2 {
					// Older clients send a flat per-frame identity, not a full blob.
					cm := codexIdentityMetadata(body)
					delete(cm, openAIWSTurnMetadataHeader)
					cm["turn_id"] = turn
					cm["turn_started_at_unix_ms"] = "1700000000001"
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				require.NoError(t, client.Write(ctx, coderws.MessageText, raw))
				_, _, err = client.Read(ctx)
				require.NoError(t, err)
			}
			_ = client.Close(coderws.StatusNormalClosure, "fixture complete")
			select {
			case err := <-done:
				if err != nil {
					require.Contains(t, err.Error(), "StatusNormalClosure")
				}
			case <-ctx.Done():
				t.Fatal("ingress did not finish")
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			require.Len(t, capture.writes, 3)
			first := projectedMetadata(t, capture.writes[0])
			second := projectedMetadata(t, capture.writes[1])
			third := projectedMetadata(t, capture.writes[2])
			require.Equal(t, first["turn_id"], second["turn_id"])
			require.Equal(t, first["turn_started_at_unix_ms"], second["turn_started_at_unix_ms"])
			require.NotEqual(t, first["turn_id"], third["turn_id"])
			require.Equal(t, first["thread_id"], third["thread_id"])
			require.Equal(t, "1700000000001", third["turn_started_at_unix_ms"])
		})
	}
}
