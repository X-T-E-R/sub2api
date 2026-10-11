package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestAntigravityOverlappingSuffixFollowsResolvedTarget(t *testing.T) {
	settings, _ := effortTestSettings(t)
	// Public input order is not a routing priority. The formal settings loader
	// canonicalizes it before the longest-suffix resolver handles requests.
	publicPolicy := AntigravityModelEffortSettings{Levels: []string{"low", "medium", "high", "extra-low"}, DefaultEffort: "medium"}
	require.NoError(t, settings.SetAntigravityModelEffortSettings(context.Background(), publicPolicy))
	mapping := map[string]string{"custom": "route-a-{effort}", "custom-extra": "route-b-{effort}"}
	account := effortTestAccount(mapping)
	sent, matched := resolveAntigravityEffortMappingWithPolicy(mapping, "custom-extra-low", "high", currentAntigravityModelEffortSettings())
	require.True(t, matched)
	require.Equal(t, "route-a-extra-low", sent)

	c, recorder := newAntigravityCompatContext(http.MethodPost, "/test", nil)
	c.Set(accountTestRequestedKey, "custom-extra-low")
	c.Set(accountTestPromptKey, "overlap fixture")
	c.Set(accountTestEffortKey, "high")
	payload := map[string]any{"model": "custom-extra-low", "output_config": map[string]any{"effort": "high"}}
	// Exercise the same payload constructor the caller uses, deliberately giving
	// metadata the original unsorted policy and the authoritative resolved target.
	effective, err := (&AccountTestService{}).applyAntigravityAccountTestEffort(c, account, payload, sent, &publicPolicy)
	require.NoError(t, err)
	require.Equal(t, "extra-low", effective)
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Equal(t, effective, gjson.GetBytes(body, "output_config.effort").String())

	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	svc := &AccountTestService{antigravityGatewayService: newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)}
	require.NoError(t, svc.testAntigravityAccountConnection(c, account, "custom-extra-low"))
	require.Len(t, upstream.requestBodies, 1)
	require.Equal(t, sent, gjson.GetBytes(upstream.requestBodies[0], "model").String())
	require.Contains(t, recorder.Body.String(), `"upstream_sent_model":"route-a-extra-low"`)
	require.Contains(t, recorder.Body.String(), `"effective_effort":"extra-low"`)
	require.NotContains(t, recorder.Body.String(), `"effective_effort":"low"`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}
