//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityConfiguredTemplateSelectedAndForwardedWithStaleCatalog(t *testing.T) {
	effortTestSettings(t)
	const model = "gemini-4-argon"
	body := []byte(`{"model":"gemini-4-argon","max_tokens":200,"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hello"}]}`)
	ctx := WithAntigravityRequestEffort(context.Background(), body)
	for _, configured := range []bool{true, false} {
		name := "configured"
		mapping := map[string]string{model: model + "-{effort}"}
		if !configured {
			name = "unconfigured"
			mapping = map[string]string{"old": "old-{effort}"}
		}
		t.Run(name, func(t *testing.T) {
			account := effortTestAccount(mapping)
			account.ID, account.Schedulable = 73, true
			account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
				Live: true, Identity: upstreamCatalogIdentity(account), ModelIDs: []string{"old-low", "old-high"},
			})
			repo := &mockAccountRepoForPlatform{accounts: []Account{*account}, accountsByID: map[int64]*Account{73: account}}
			gateway := &GatewayService{accountRepo: repo, cache: &mockGatewayCacheForPlatform{}, cfg: testConfig()}
			groupID := int64(8)
			selected, err := gateway.selectAccountForModelWithPlatform(ctx, &groupID, "", model, nil, PlatformAntigravity)
			if !configured {
				require.Error(t, err)
				require.Nil(t, selected)
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(73), selected.ID)
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			forwarder := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
			result, err := forwarder.Forward(ctx, c, selected, body, false)
			require.NoError(t, err)
			require.Len(t, upstream.requestBodies, 1)
			require.Equal(t, model+"-medium", gjson.GetBytes(upstream.requestBodies[0], "model").String())
			require.Equal(t, model+"-medium", result.UpstreamModel)
		})
	}
}
