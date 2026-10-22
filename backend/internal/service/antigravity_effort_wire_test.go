package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityEffortWireMatchesResolvedMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	effortTestSettings(t)
	const template = "gemini-explicit-custom-{effort}"
	cases := []struct {
		name      string
		model     string
		effort    string
		mapping   map[string]string
		wantModel string
		wantLevel string
	}{
		{
			name:      "suffix wins over body effort",
			model:     "family-low",
			effort:    "high",
			mapping:   map[string]string{"family": template},
			wantModel: "gemini-explicit-custom-low",
		},
		{
			name:   "effort override alias wins over template",
			model:  "family",
			effort: "high",
			mapping: map[string]string{
				"family":      template,
				"family-high": "gemini-alias-target",
			},
			wantModel: "gemini-alias-target",
		},
		{
			name:      "template stores the body effort in the model id",
			model:     "family",
			effort:    "high",
			mapping:   map[string]string{"family": template},
			wantModel: "gemini-explicit-custom-high",
		},
		{
			name:      "literal mapping leaves body effort for thinkingLevel",
			model:     "gemini-explicit-custom",
			effort:    "high",
			mapping:   map[string]string{"gemini-explicit-custom": "gemini-explicit-custom"},
			wantModel: "gemini-explicit-custom",
			wantLevel: "high",
		},
		{
			name:      "omitted effort does not invent a level",
			model:     "gemini-explicit-custom",
			mapping:   map[string]string{"gemini-explicit-custom": "gemini-explicit-custom"},
			wantModel: "gemini-explicit-custom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"` + tc.model + `","max_tokens":128,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`
			if tc.effort != "" {
				body = `{"model":"` + tc.model + `","max_tokens":128,"output_config":{"effort":"` + tc.effort + `"},"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`
			}
			account := effortTestAccount(tc.mapping)
			ctx := WithAntigravityRequestEffort(context.Background(), []byte(body))
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", []byte(body))
			result, err := svc.Forward(ctx, c, account, []byte(body), false)
			require.NoError(t, err)
			require.Len(t, upstream.requestBodies, 1)
			wire := upstream.requestBodies[0]
			require.Equal(t, tc.wantModel, gjson.GetBytes(wire, "model").String())
			require.Equal(t, tc.wantModel, result.UpstreamModel)
			require.Equal(t, tc.model, result.Model)
			require.False(t, result.UpstreamResponseModelConflict)
			level := gjson.GetBytes(wire, "request.generationConfig.thinkingConfig.thinkingLevel")
			if tc.wantLevel == "" {
				require.False(t, level.Exists(), "wire = %s", wire)
			} else {
				require.Equal(t, tc.wantLevel, level.String())
				require.False(t, gjson.GetBytes(wire, "request.generationConfig.thinkingConfig.thinkingBudget").Exists())
			}
		})
	}
}
