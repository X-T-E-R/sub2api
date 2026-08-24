//go:build unit

package service

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	grokViewImageCompatPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP4z8DwHwAFAAH/VscvDQAAAABJRU5ErkJggg=="
	grokViewImageCompatGIFDataURL = "data:image/gif;base64,R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="
)

func TestGrokToolOutputImageVariantNormalization(t *testing.T) {
	pngPayload := strings.TrimPrefix(grokViewImageCompatPNGDataURL, "data:image/png;base64,")
	gifPayload := strings.TrimPrefix(grokViewImageCompatGIFDataURL, "data:image/gif;base64,")

	tests := []struct {
		name       string
		output     string
		compat     bool
		wantArray  bool
		wantURL    string
		wantDetail string
		wantString string
	}{
		{
			name:       "declared PNG and high remain unchanged",
			output:     `[{"type":"input_image","image_url":"` + grokViewImageCompatPNGDataURL + `","detail":"high"}]`,
			compat:     true,
			wantArray:  true,
			wantURL:    grokViewImageCompatPNGDataURL,
			wantDetail: "high",
		},
		{
			name:       "octet stream PNG is detected and rewritten",
			output:     `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngPayload + `","detail":"high"}]`,
			compat:     true,
			wantArray:  true,
			wantURL:    grokViewImageCompatPNGDataURL,
			wantDetail: "high",
		},
		{
			name:      "octet stream GIF uses its detected image type",
			output:    `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + gifPayload + `"}]`,
			compat:    true,
			wantArray: true,
			wantURL:   grokViewImageCompatGIFDataURL,
		},
		{
			name:       "original detail maps to high",
			output:     `[{"type":"input_image","image_url":"` + grokViewImageCompatPNGDataURL + `","detail":"original"}]`,
			compat:     true,
			wantArray:  true,
			wantURL:    grokViewImageCompatPNGDataURL,
			wantDetail: "high",
		},
		{
			name:       "non image octet stream stringifies",
			output:     `[{"type":"input_image","image_url":"data:application/octet-stream;base64,dGhpcyBpcyB0ZXh0"}]`,
			compat:     true,
			wantString: "application/octet-stream",
		},
		{
			name:       "malformed octet stream stringifies",
			output:     `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngPayload + `%%%"}]`,
			compat:     true,
			wantString: "%%%",
		},
		{
			name:       "multiple sources stringify",
			output:     `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngPayload + `","file_id":"file_ambiguous"}]`,
			compat:     true,
			wantString: "file_ambiguous",
		},
		{
			name:       "compat off keeps old string fallback",
			output:     `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngPayload + `","detail":"original"}]`,
			compat:     false,
			wantString: "application/octet-stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"function_call","name":"render_image","call_id":"variant-call","arguments":"{}"},{"type":"function_call_output","call_id":"variant-call","output":` + tt.output + `}]}`)
			patched, err := sanitizeGrokResponsesModelInputWithCompat(body, tt.compat)
			require.NoError(t, err)
			require.Equal(t, "variant-call", gjson.GetBytes(patched, "input.1.call_id").String())

			output := gjson.GetBytes(patched, "input.1.output")
			if !tt.wantArray {
				require.Equal(t, gjson.String, output.Type)
				require.Contains(t, output.String(), tt.wantString)
				return
			}
			require.True(t, output.IsArray())
			require.Equal(t, tt.wantURL, output.Get("0.image_url").String())
			if tt.wantDetail == "" {
				require.False(t, output.Get("0.detail").Exists())
			} else {
				require.Equal(t, tt.wantDetail, output.Get("0.detail").String())
			}
		})
	}
}

func TestGrokNativeViewImageVariantReachesProviderBody(t *testing.T) {
	pngPayload := strings.TrimPrefix(grokViewImageCompatPNGDataURL, "data:image/png;base64,")
	body := []byte(`{"model":"grok-4.6","input":[{"type":"message","role":"user","content":"inspect"},{"type":"function_call","name":"render_image","call_id":"native-image","arguments":"{}"},{"type":"function_call_output","call_id":"native-image","output":[{"type":"input_text","text":"frame"},{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngPayload + `","detail":"original"}]}]}`)

	prepared, _, err := patchGrokResponsesBodyWithClientToolsCompat(body, "grok-4.6", true)
	require.NoError(t, err)
	account := healthyGrokOAuthGatewayTestAccount(9911, "access-token")
	req, err := buildGrokResponsesRequest(context.Background(), nil, account, prepared, "access-token", "", nil)
	require.NoError(t, err)
	providerBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	require.Equal(t, "native-image", gjson.GetBytes(providerBody, "input.2.call_id").String())
	require.Equal(t, "input_text", gjson.GetBytes(providerBody, "input.2.output.0.type").String())
	require.Equal(t, "frame", gjson.GetBytes(providerBody, "input.2.output.0.text").String())
	require.Equal(t, grokViewImageCompatPNGDataURL, gjson.GetBytes(providerBody, "input.2.output.1.image_url").String())
	require.Equal(t, "high", gjson.GetBytes(providerBody, "input.2.output.1.detail").String())
}

func TestGrokWSInheritedCustomViewImageVariantReachesProviderBody(t *testing.T) {
	account := healthyGrokOAuthGatewayTestAccount(9912, "access-token")
	seed, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.6","input":"start","tools":[{"type":"custom","name":"render","format":{"type":"text"}}]}`),
		account, "grok-4.6", apicompat.ResponsesClientToolMapping{}, nil, true,
	)
	require.NoError(t, err)
	require.True(t, seed.Mapping.CustomTools["render"])

	gifPayload := strings.TrimPrefix(grokViewImageCompatGIFDataURL, "data:image/gif;base64,")
	inheritedTools := decodeOpenAIWSHTTPBridgeLoweredTools(seed.LoweredTools)
	prepared, err := prepareGrokWSResponsesBody(
		[]byte(`{"type":"response.create","model":"grok-4.6","input":[{"type":"custom_tool_call","name":"render","call_id":"inherited-image","input":"{}"},{"type":"custom_tool_call_output","call_id":"inherited-image","output":[{"type":"input_text","text":"frame"},{"type":"input_image","image_url":"data:application/octet-stream;base64,`+gifPayload+`","detail":"original"}]}]}`),
		account, "grok-4.6", seed.Mapping, inheritedTools, true,
	)
	require.NoError(t, err)
	req, err := buildGrokResponsesRequest(context.Background(), nil, account, prepared.Body, "access-token", "", nil)
	require.NoError(t, err)
	providerBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	require.Equal(t, "function_call_output", gjson.GetBytes(providerBody, "input.1.type").String())
	require.Equal(t, "inherited-image", gjson.GetBytes(providerBody, "input.1.call_id").String())
	require.Equal(t, "input_text", gjson.GetBytes(providerBody, "input.1.output.0.type").String())
	require.Equal(t, grokViewImageCompatGIFDataURL, gjson.GetBytes(providerBody, "input.1.output.1.image_url").String())
	require.Equal(t, "high", gjson.GetBytes(providerBody, "input.1.output.1.detail").String())
}
