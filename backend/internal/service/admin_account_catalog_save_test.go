package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAdminCatalogSyncSaveRetainsLatestAvailability(t *testing.T) {
	effortTestSettings(t, "extra-low")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)
		_, _ = io.WriteString(w, `{"models":{"bespoke-low":{"supportsThinking":false,"maxTokens":1000},"bespoke-high":{"supportsThinking":false,"maxTokens":1000}}}`)
	}))
	defer server.Close()
	oldURL, oldURLs := antigravity.BaseURL, antigravity.BaseURLs
	antigravity.BaseURL, antigravity.BaseURLs = server.URL, []string{server.URL}
	t.Cleanup(func() { antigravity.BaseURL, antigravity.BaseURLs = oldURL, oldURLs })
	for _, name := range []string{"first_sync", "resync_stale_catalog", "explicit_null_catalog"} {
		t.Run(name, func(t *testing.T) {
			account := effortTestAccount(map[string]string{"bespoke": "bespoke-{effort:high}"})
			account.Extra = map[string]any{"quota_daily_limit": 100}
			if name == "resync_stale_catalog" {
				account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Live: true, Identity: upstreamCatalogIdentity(account), ModelIDs: []string{"bespoke-low", "bespoke-high", "bespoke-max"}, Models: map[string]UpstreamModelMetadata{}})
			}
			repo := &accountBillingSettingsAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}}
			opened, err := repo.GetByID(context.Background(), account.ID)
			require.NoError(t, err)
			// Mirrors the editor's pre-sync Extra; removing ordinary quota is intentional.
			staleExtra := mergeMap(nil, opened.Extra)
			delete(staleExtra, "quota_daily_limit")
			if name == "explicit_null_catalog" {
				staleExtra[UpstreamModelMetadataExtraKey] = nil
			}
			syncService := &AccountTestService{cfg: upstreamModelSyncTestConfig(), accountRepo: repo, antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{}, nil)}
			catalog, err := syncService.SyncUpstreamModelCatalog(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, "bespoke-{effort:high}", catalog.ModelMapping["bespoke"])
			latest := account.GetUpstreamModelMetadataSnapshot()
			require.NotNil(t, latest)
			assertCatalogMissingMaxNotForwarded(t, account)
			credentials := mergeMap(nil, opened.Credentials)
			credentials["model_mapping"] = map[string]any{"bespoke": catalog.ModelMapping["bespoke"]}
			saved, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Credentials: credentials, Extra: staleExtra})
			require.NoError(t, err)
			require.Equal(t, latest, saved.GetUpstreamModelMetadataSnapshot())
			require.NotContains(t, saved.Extra, "quota_daily_limit", "ordinary explicit clears must still work")
			assertCatalogMissingMaxNotForwarded(t, saved)
		})
	}
	require.Equal(t, 3, calls)
}

func assertCatalogMissingMaxNotForwarded(t *testing.T, account *Account) {
	t.Helper()
	body := []byte(`{"model":"bespoke","reasoning_effort":"max","messages":[{"role":"user","content":"hello"}]}`)
	ctx := WithAntigravityRequestEffort(context.Background(), body)
	require.False(t, (&GatewayService{}).isModelSupportedByAccountWithContext(ctx, account, "bespoke"))
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)
	_, err := svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
	require.Error(t, err)
	require.Zero(t, upstream.callCount)
	require.Empty(t, upstream.requestBodies)
}

func TestAdminCatalogClientCannotCreateManagedSnapshot(t *testing.T) {
	account := effortTestAccount(nil)
	repo := &accountBillingSettingsAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}}
	forged := UpstreamModelMetadataSnapshot{Live: true, ModelIDs: []string{"forged"}, Models: map[string]UpstreamModelMetadata{}}
	saved, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Extra: map[string]any{UpstreamModelMetadataExtraKey: forged}})
	require.NoError(t, err)
	require.NotContains(t, saved.Extra, UpstreamModelMetadataExtraKey)
	created, err := buildAccountForCreate(&CreateAccountInput{Platform: PlatformAntigravity, Type: AccountTypeOAuth}, map[string]any{UpstreamModelMetadataExtraKey: forged})
	require.NoError(t, err)
	require.NotContains(t, created.Extra, UpstreamModelMetadataExtraKey)
}

func TestAdminCatalogSavePreservesEvidenceAndIdentityContract(t *testing.T) {
	effortTestSettings(t)
	for _, name := range []string{"token_rotation", "project_change", "omitted_extra"} {
		t.Run(name, func(t *testing.T) {
			account := effortTestAccount(map[string]string{"bespoke": "bespoke-{effort:high}"})
			snapshot := UpstreamModelMetadataSnapshot{Live: true, Identity: upstreamCatalogIdentity(account), ModelIDs: []string{"bespoke-low", "bespoke-high"}, Models: map[string]UpstreamModelMetadata{}}
			account.SetUpstreamModelMetadataSnapshot(snapshot)
			repo := &accountBillingSettingsAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}}
			input := &UpdateAccountInput{Credentials: mergeMap(nil, account.Credentials), Extra: map[string]any{}}
			switch name {
			case "token_rotation":
				input.Credentials["access_token"] = "rotated-token"
			case "project_change":
				input.Credentials["project_id"] = "different-project"
			case "omitted_extra":
				input.Extra = nil
			}
			saved, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, input)
			require.NoError(t, err)
			require.Equal(t, &snapshot, saved.GetUpstreamModelMetadataSnapshot())
			if name == "project_change" {
				require.NotEqual(t, snapshot.Identity, upstreamCatalogIdentity(saved))
				require.Equal(t, "bespoke-max", mapAntigravityModel(saved, "bespoke-max"), "old identity evidence must not constrain a different project")
			} else {
				require.Equal(t, snapshot.Identity, upstreamCatalogIdentity(saved))
				assertCatalogMissingMaxNotForwarded(t, saved)
			}
		})
	}
}

func TestAdminCatalogExtraPatchAndBulkCannotInjectEvidence(t *testing.T) {
	account := effortTestAccount(nil)
	snapshot := UpstreamModelMetadataSnapshot{Live: true, ModelIDs: []string{"current"}, Models: map[string]UpstreamModelMetadata{}}
	account.SetUpstreamModelMetadataSnapshot(snapshot)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	svc := &adminServiceImpl{accountRepo: repo}
	require.NoError(t, svc.UpdateAccountExtra(context.Background(), account.ID, map[string]any{UpstreamModelMetadataExtraKey: nil, "custom": "value"}))
	require.Equal(t, &snapshot, account.GetUpstreamModelMetadataSnapshot())
	require.Equal(t, "value", account.Extra["custom"])
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{account.ID}, Extra: map[string]any{UpstreamModelMetadataExtraKey: map[string]any{"live": true}, "custom": "bulk"}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.NotContains(t, repo.bulkUpdates[0].Extra, UpstreamModelMetadataExtraKey)
	require.Equal(t, "bulk", repo.bulkUpdates[0].Extra["custom"])
}
