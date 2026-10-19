package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCatalogProjectionNamingEvidence(t *testing.T) {
	effortTestSettings(t, "extra-low")
	account := effortTestAccount(nil)
	p := currentAntigravityModelEffortSettings()
	for _, tc := range []struct {
		name string
		ids  []string
		want map[string]string
	}{
		{"generic longest suffix", []string{"custom-extra-low", "custom-low", "custom-high", "custom-low"}, map[string]string{"custom": "custom-{effort:extra-low}"}},
		{"global default present", []string{"foo-medium", "foo-high"}, map[string]string{"foo": "foo-{effort}"}},
		{"base and one variant", []string{"foo", "foo-high"}, map[string]string{"foo": "foo-{effort:high}"}},
		{"single literal budget", []string{"claude-opus-5-5-high"}, map[string]string{"claude-opus-5-5-high": "claude-opus-5-5-high"}},
		{"native only and independent IDs", []string{"gemini-pro-agent", "gemini-lite", "image-high", "tiered", "foo-future"}, map[string]string{"gemini-pro-agent": "gemini-pro-agent", "gemini-lite": "gemini-lite", "image-high": "image-high", "tiered": "tiered", "foo-future": "foo-future"}},
		{"unknown future alongside family", []string{"foo-low", "foo-high", "foo-future"}, map[string]string{"foo": "foo-{effort:high}", "foo-future": "foo-future"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yes := true
			metadata := map[string]UpstreamModelMetadata{}
			for _, id := range tc.ids {
				metadata[id] = UpstreamModelMetadata{ID: id, Reasoning: &yes, SupportedReasoningLevels: []string{"low", "medium", "high", "max"}}
			}
			mapping, _ := projectUpstreamModelMapping(account, tc.ids, metadata, p)
			require.Equal(t, tc.want, mapping)
		})
	}
	metadata := map[string]UpstreamModelMetadata{"foo": {ID: "foo", DefaultReasoningLevel: "low"}}
	mapping, families := projectUpstreamModelMapping(account, []string{"foo", "foo-low", "foo-high"}, metadata, p)
	require.Equal(t, "foo-{effort:low}", mapping["foo"])
	require.Equal(t, []string{"high", "low"}, families["foo"])
	require.Equal(t, "catalog_suffix", metadata["foo"].ReasoningLevelsSource)
	mapping, _ = projectUpstreamModelMapping(&Account{Platform: PlatformGemini}, []string{"foo-low", "foo-high"}, map[string]UpstreamModelMetadata{}, p)
	require.Equal(t, map[string]string{"foo-low": "foo-low", "foo-high": "foo-high"}, mapping)
}

func TestCatalogSnapshotDoesNotVetoConfiguredRoutingAndRealForward(t *testing.T) {
	effortTestSettings(t)
	metadata := map[string]UpstreamModelMetadata{}
	ids := []string{"bespoke-low", "bespoke-high"}
	mapping, families := projectUpstreamModelMapping(effortTestAccount(nil), ids, metadata, currentAntigravityModelEffortSettings())
	account := effortTestAccount(mapping)
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, ModelIDs: ids, Models: metadata, EffortFamilies: families, Identity: upstreamCatalogIdentity(account)})
	for _, requested := range []string{"bespoke-max", "bespoke-medium"} {
		require.True(t, account.IsModelSupported(requested))
		require.Equal(t, requested, mapAntigravityModel(account, requested))
		require.True(t, explicitModelMappingClaims(*account, requested))
	}
	for _, effort := range []string{"low", "high", "", "max"} {
		body := []byte(`{"model":"bespoke","reasoning_effort":"` + effort + `","messages":[{"role":"user","content":"hello"}]}`)
		ctx := WithAntigravityRequestEffort(context.Background(), body)
		supported := (&GatewayService{}).isModelSupportedByAccountWithContext(ctx, account, "bespoke")
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
		svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)
		_, err := svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
		require.True(t, supported)
		require.NoError(t, err)
		require.Len(t, upstream.requestBodies, 1)
		want := effort
		if want == "" {
			want = "high"
		}
		require.Equal(t, "bespoke-"+want, gjson.GetBytes(upstream.requestBodies[0], "model").String())
	}
	// A later, smaller discovery snapshot cannot revoke the saved template.
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, ModelIDs: []string{"bespoke-low"}, Models: map[string]UpstreamModelMetadata{}})
	require.True(t, account.IsModelSupported("bespoke-high"))
	require.True(t, account.IsModelSupported("bespoke-low"))
	now := time.Now()
	setAccountModelRateLimitSnapshot(account, "bespoke", now.Add(time.Minute), "fixture", now)
	ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"low"}`))
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "bespoke"))
	require.False(t, account.IsSchedulableForModelWithContext(ctx, "bespoke-low"))
}

func TestCatalogOAuthFetchUsesSelectedCredentialsAndDirectMetadata(t *testing.T) {
	effortTestSettings(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)
		require.Equal(t, "Bearer fresh-oauth-token", r.Header.Get("Authorization"))
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "project-3757", request["project"])
		_, _ = io.WriteString(w, `{"models":{"custom-low":{"supportsThinking":true,"maxTokens":1000,"maxOutputTokens":200},"custom-high":{"supportsThinking":true},"gemini-pro-agent":{"supportsThinking":true,"supportedReasoningLevels":["low","high"]},"claude-opus-5-5-high":{"supportsThinking":true,"thinkingBudget":2000}}}`)
	}))
	defer server.Close()
	oldURL, oldURLs := antigravity.BaseURL, antigravity.BaseURLs
	antigravity.BaseURL, antigravity.BaseURLs = server.URL, []string{server.URL}
	t.Cleanup(func() { antigravity.BaseURL, antigravity.BaseURLs = oldURL, oldURLs })
	repo := &upstreamModelMetadataRepoStub{}
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	account.Extra = map[string]any{"model_rate_limits": map[string]any{"custom": map[string]any{"reset_time": time.Now().Add(time.Hour).Format(time.RFC3339)}}}
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo, antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{}, nil)}
	before := account.GetModelMapping()
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.True(t, catalog.Live)
	require.Equal(t, "upstream", catalog.Source)
	require.Equal(t, "custom-{effort:high}", catalog.ModelMapping["custom"])
	require.Equal(t, "claude-opus-5-5-high", catalog.ModelMapping["claude-opus-5-5-high"])
	require.Equal(t, "gemini-pro-agent", catalog.ModelMapping["gemini-pro-agent"])
	require.Equal(t, []string{"high", "low"}, catalog.Metadata["custom"].SupportedReasoningLevels)
	require.Equal(t, "catalog_suffix", catalog.Metadata["custom"].ReasoningLevelsSource)
	require.Equal(t, int64(1000), catalog.Metadata["custom-low"].ContextWindow)
	require.Equal(t, "upstream", catalog.Metadata["gemini-pro-agent"].ReasoningLevelsSource)
	require.Equal(t, before, account.GetModelMapping()) // Save has not occurred.
	require.NotNil(t, account.Extra["model_rate_limits"])
	require.NotNil(t, repo.updates[UpstreamModelMetadataExtraKey])
}

func TestCatalogFallbackHasNoReplacementAndKeepsLiveSnapshot(t *testing.T) {
	repo := &upstreamModelMetadataRepoStub{}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`not found`))}, {StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}}}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "base_url": "https://example.test/v1", "model_mapping": map[string]any{"foo": "foo"}}}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, Source: "upstream", ModelIDs: []string{"previous"}, Models: map[string]UpstreamModelMetadata{"previous": {ID: "previous"}}})
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), httpUpstream: upstream, accountRepo: repo}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.False(t, catalog.Live)
	require.Equal(t, "configured", catalog.Source)
	require.Empty(t, catalog.ModelMapping)
	require.Nil(t, repo.updates)
	require.Equal(t, []string{"previous"}, account.GetUpstreamModelMetadataSnapshot().ModelIDs)
}

func TestCatalogAuthoritativeEmptyAndFailureKeepLastGood(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"authoritative empty", `{"models":{}}`, 200, false},
		{"missing catalog", `{}`, 200, true},
		{"wrong shape", `{"models":[]}`, 200, true},
		{"HTTP error", `{"models":{"foo":{}}}`, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			oldURL, oldURLs := antigravity.BaseURL, antigravity.BaseURLs
			antigravity.BaseURL, antigravity.BaseURLs = server.URL, []string{server.URL}
			t.Cleanup(func() { antigravity.BaseURL, antigravity.BaseURLs = oldURL, oldURLs })
			repo := &upstreamModelMetadataRepoStub{}
			account := newAntigravityCompatAccount(AccountTypeOAuth)
			account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, ModelIDs: []string{"old-high"}, Models: map[string]UpstreamModelMetadata{}})
			svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo, antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{}, nil)}
			catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, catalog)
			} else {
				require.NoError(t, err)
				require.True(t, catalog.Live)
				require.Equal(t, "upstream", catalog.Source)
				require.Empty(t, catalog.Models)
				require.Empty(t, catalog.ModelMapping)
			}
			require.Nil(t, repo.updates)
			require.Equal(t, []string{"old-high"}, account.GetUpstreamModelMetadataSnapshot().ModelIDs)
		})
	}
}

func TestCatalogLiteralOverridesAndScopeRemainExplicit(t *testing.T) {
	effortTestSettings(t)
	account := effortTestAccount(map[string]string{"foo": "foo-{effort:high}", "foo-max": "foo-custom", "alias": "foo-custom", "other": "other"})
	snapshot := UpstreamModelMetadataSnapshot{Live: true, ModelIDs: []string{"foo-low", "foo-high"}, Models: map[string]UpstreamModelMetadata{}, Identity: upstreamCatalogIdentity(account)}
	account.SetUpstreamModelMetadataSnapshot(snapshot)
	require.Equal(t, "foo-custom", mapAntigravityModel(account, "alias"))
	require.Equal(t, "foo-custom", mapAntigravityModelWithContext(WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"max"}`)), account, "foo"))
	require.Equal(t, "other", mapAntigravityModel(account, "other"))
	account.Credentials["access_token"] = "normal rotation"
	require.Equal(t, snapshot.Identity, upstreamCatalogIdentity(account))
	account.Credentials["project_id"] = "different project"
	require.NotEqual(t, snapshot.Identity, upstreamCatalogIdentity(account))
	// Stale evidence belongs to the previous network/project identity only.
	require.Equal(t, "foo-medium", mapAntigravityModel(account, "foo-medium"))
}

// Reduced allowlist fixture from the parent-owned live fetch (no requests here).
// It preserves all IDs and only the representative capability fields relevant
// to suffix naming; display labels and budgets are not naming evidence.
func TestCatalogProjectionObserved20261011(t *testing.T) {
	effortTestSettings(t, "extra-low")
	body, err := os.ReadFile("testdata/antigravity_catalog_20261011.json")
	require.NoError(t, err)
	var fixture struct {
		AccountIDs        []int64                    `json:"account_ids"`
		ModelIDs          []string                   `json:"model_ids"`
		MetadataAccountID int64                      `json:"representative_metadata_account_id"`
		Metadata          map[string]json.RawMessage `json:"representative_metadata"`
	}
	require.NoError(t, json.Unmarshal(body, &fixture))
	raw, _ := json.Marshal(map[string]any{"models": fixture.Metadata})
	for _, accountID := range fixture.AccountIDs {
		t.Run(fmt.Sprint(accountID), func(t *testing.T) {
			account := effortTestAccount(nil)
			account.ID = accountID
			metadata := map[string]UpstreamModelMetadata{}
			if accountID == fixture.MetadataAccountID {
				metadata = extractAntigravityCatalogMetadata(raw)
			}
			mapping, families := projectUpstreamModelMapping(account, fixture.ModelIDs, metadata, currentAntigravityModelEffortSettings())
			plan, err := json.Marshal(map[string]any{"account_id": accountID, "raw_count": len(fixture.ModelIDs), "rule_count": len(mapping), "model_mapping": mapping, "effort_families": families})
			require.NoError(t, err)
			t.Logf("catalog_projection %s", plan)
			require.Len(t, fixture.ModelIDs, 33)
			require.Len(t, mapping, 25)
			require.Len(t, families, 5)
			require.Equal(t, "gemini-3.1-pro-{effort:high}", mapping["gemini-3.1-pro"])
			require.Equal(t, "gemini-3.5-flash-{effort:extra-low}", mapping["gemini-3.5-flash"])
			for _, base := range []string{"gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash"} {
				require.Equal(t, base+"-{effort}", mapping[base])
				require.Equal(t, []string{"high", "low", "medium"}, metadata[base].SupportedReasoningLevels)
				require.Equal(t, "medium", metadata[base].DefaultReasoningLevel)
			}
			require.Equal(t, []string{"extra-low", "low"}, metadata["gemini-3.5-flash"].SupportedReasoningLevels)
			if accountID == fixture.MetadataAccountID {
				require.Equal(t, "Gemini 3.5 Flash (Low)", metadata["gemini-3.5-flash-extra-low"].DisplayName)
				require.Equal(t, "Gemini 3.5 Flash (Medium)", metadata["gemini-3.5-flash-low"].DisplayName)
			}
			require.Equal(t, "gpt-oss-120b-medium", mapping["gpt-oss-120b-medium"])
			require.NotContains(t, mapping, "gpt-oss-120b")
			require.Empty(t, metadata["gpt-oss-120b-medium"].SupportedReasoningLevels)
			require.Equal(t, "gemini-pro-agent", mapping["gemini-pro-agent"])
			require.Empty(t, metadata["gemini-pro-agent"].SupportedReasoningLevels)
			for _, id := range []string{"gemini-3.1-flash-image", "gemini-3.5-flash-lite", "gemini-3.8-flash-tiered", "chat_20706", "tab_flash_lite_preview"} {
				require.Equal(t, id, mapping[id])
			}
			account = effortTestAccount(mapping)
			account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, ModelIDs: fixture.ModelIDs, Models: metadata, EffortFamilies: families})
			require.Equal(t, "gemini-3.8-flash-max", mapAntigravityModel(account, "gemini-3.8-flash-max"))
			require.Equal(t, "gemini-3.1-pro-medium", mapAntigravityModel(account, "gemini-3.1-pro-medium"))
			require.Equal(t, "gemini-3.5-flash-medium", mapAntigravityModel(account, "gemini-3.5-flash-medium"))
			require.Equal(t, "gemini-3.5-flash-low", mapAntigravityModel(account, "gemini-3.5-flash-low"))
		})
	}
}

func TestConfiguredAntigravityRoutesIgnoreMissingCatalogIDs(t *testing.T) {
	effortTestSettings(t, "ultra")
	for _, live := range []bool{false, true} {
		for _, tc := range []struct {
			name, requested, effort, want string
			mapping                       map[string]string
		}{
			{"literal", "gemini-4-argon", "high", "gemini-4-argon", map[string]string{"gemini-4-argon": "gemini-4-argon"}},
			{"alias", "friendly", "high", "gemini-4-argon-ultra", map[string]string{"friendly": "gemini-4-argon-ultra"}},
			{"wildcard", "lab-new", "high", "gemini-4-argon-ultra", map[string]string{"lab-*": "gemini-4-argon-ultra"}},
			{"template", "gemini-4-argon", "ultra", "gemini-4-argon-ultra", map[string]string{"gemini-4-argon": "gemini-4-argon-{effort}"}},
			{"template default", "friendly", "", "gemini-4-argon-high", map[string]string{"friendly": "gemini-4-argon-{effort:high}"}},
			{"template suffix", "friendly-ultra", "high", "gemini-4-argon-ultra", map[string]string{"friendly": "gemini-4-argon-{effort}"}},
			{"not configured", "gemini-4-argon", "high", "", map[string]string{"known": "known"}},
			{"disabled effort", "gemini-4-argon", "disabled", "", map[string]string{"gemini-4-argon": "gemini-4-argon-{effort}"}},
			{"invalid template", "gemini-4-argon", "high", "", map[string]string{"gemini-4-argon": "gemini-4-argon-{effort:bad name}"}},
			{"empty target", "blocked", "", "", map[string]string{"blocked": ""}},
		} {
			t.Run(fmt.Sprintf("live_%t/%s", live, tc.name), func(t *testing.T) {
				account := effortTestAccount(tc.mapping)
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
					Live: live, Identity: upstreamCatalogIdentity(account), ModelIDs: []string{"old-low", "old-high"},
				})
				ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"`+tc.effort+`"}`))
				require.Equal(t, tc.want, resolveFinalAntigravityModelKey(ctx, account, tc.requested))
				require.Equal(t, tc.want != "", (&GatewayService{}).isModelSupportedByAccountWithContext(ctx, account, tc.requested))
				if tc.want != "" {
					require.Contains(t, account.modelRateLimitKeysForRequest(ctx, tc.requested), tc.want)
					now := time.Now()
					setAccountModelRateLimitSnapshot(account, tc.want, now.Add(time.Minute), "fixture", now)
					require.True(t, account.isModelRateLimitedWithContext(ctx, tc.requested), "configured models must retain exact-target rate limits")
				} else {
					body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":200,"output_config":{"effort":%q},"messages":[{"role":"user","content":"hello"}]}`, tc.requested, tc.effort))
					upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
					svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
					c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
					_, err := svc.Forward(ctx, c, account, body, false)
					require.Error(t, err)
					wantStatus := http.StatusForbidden
					if tc.name == "disabled effort" {
						wantStatus = http.StatusBadRequest
					}
					require.Equal(t, wantStatus, recorder.Code)
					require.Zero(t, upstream.callCount)
				}
			})
		}
	}
}

// Replays the account73 Hub-profile IDs observed at 2026-10-19T12:10:50Z;
// only the ID list is retained, not credentials or capability metadata.
func TestCatalogHubProfileProjectsObservedArgon(t *testing.T) {
	effortTestSettings(t)
	body, err := os.ReadFile("testdata/antigravity_catalog_20261019_hub.json")
	require.NoError(t, err)
	var fixture struct {
		ModelIDs []string `json:"model_ids"`
	}
	require.NoError(t, json.Unmarshal(body, &fixture))
	models := make(map[string]any, len(fixture.ModelIDs))
	for _, id := range fixture.ModelIDs {
		models[id] = map[string]any{}
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)
		require.Equal(t, "antigravity/hub/"+antigravity.GetUserAgentVersionForContext(r.Context())+" darwin/arm64", r.Header.Get("User-Agent"))
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"models": models}))
	}))
	defer server.Close()
	oldURL, oldURLs := antigravity.BaseURL, antigravity.BaseURLs
	antigravity.BaseURL, antigravity.BaseURLs = server.URL, []string{server.URL}
	t.Cleanup(func() { antigravity.BaseURL, antigravity.BaseURLs = oldURL, oldURLs })
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	repo := &upstreamModelMetadataRepoStub{}
	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo, antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{}, nil)}
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.True(t, catalog.Live)
	require.Len(t, catalog.Models, 34)
	require.ElementsMatch(t, fixture.ModelIDs, catalog.Models)
	require.Equal(t, "gemini-4-argon-{effort}", catalog.ModelMapping["gemini-4-argon"])
	for _, effort := range []string{"low", "medium", "high"} {
		require.NotContains(t, catalog.ModelMapping, "gemini-4-argon-"+effort)
	}
	require.Equal(t, []string{"high", "low", "medium"}, catalog.Metadata["gemini-4-argon"].SupportedReasoningLevels)
	require.Equal(t, "medium", catalog.Metadata["gemini-4-argon"].DefaultReasoningLevel)
}
