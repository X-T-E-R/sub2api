package service

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAntigravityZeroOutputContinueMatchesClientRequest(t *testing.T) {
	// Synthetic requests captured from Kiki's actual AnthropicChatProvider.generate
	// at the SDK create seam, with a stub client and no network. Empty assistant
	// history is omitted and adjacent user blocks (including tool results) coalesce.
	fixture, err := os.ReadFile("testdata/antigravity_client_continue_messages.json")
	require.NoError(t, err)
	var cases []struct {
		Name      string          `json:"name"`
		Initial   json.RawMessage `json:"initial"`
		Continued json.RawMessage `json:"continued"`
	}
	require.NoError(t, json.Unmarshal(fixture, &cases))
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{
				antigravityThoughtStreamResponse(antigravityEmptyStopLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
				antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine),
			}}
			var headers []http.Header
			var urls []string
			upstream.onCall = func(req *http.Request, _ *queuedHTTPUpstreamStub) {
				headers = append(headers, req.Header.Clone())
				urls = append(urls, req.URL.String())
			}
			svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueZeroOutput: true}, upstream)
			account := antigravityThoughtAccount(nil)
			for _, body := range [][]byte{tc.Initial, tc.Continued} {
				c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
				_, forwardErr := svc.Forward(context.Background(), c, account, body, false)
				require.NoError(t, forwardErr)
			}
			require.Equal(t, 3, upstream.callCount, "initial + one gateway extra + independent client follow-up")
			initial, gateway, client := upstream.requestBodies[0], upstream.requestBodies[1], upstream.requestBodies[2]
			require.Equal(t, headers[0], headers[1])
			require.Equal(t, headers[1], headers[2])
			require.Equal(t, urls[0], urls[1])
			require.Equal(t, urls[1], urls[2])
			require.Equal(t, gjson.GetBytes(initial, "requestId").String(), gjson.GetBytes(gateway, "requestId").String(), "gateway preserves the request envelope identity")
			require.NotEqual(t, gjson.GetBytes(gateway, "requestId").String(), gjson.GetBytes(client, "requestId").String(), "an independent inbound request gets its own request ID")
			gateway, err = sjson.DeleteBytes(gateway, "requestId")
			require.NoError(t, err)
			client, err = sjson.DeleteBytes(client, "requestId")
			require.NoError(t, err)
			require.JSONEq(t, string(client), string(gateway), "all semantic envelope fields and tool history match the independently transformed client follow-up")
		})
	}
}

func TestAppendAntigravityUserContinuePreservesPreparedEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, contents, want string
	}{
		{"user", `[{"role":"user","parts":[{"text":"question"}]}]`, `[{"role":"user","parts":[{"text":"question"},{"text":"Continue."}]}]`},
		{"model", `[{"role":"model","parts":[{"text":"prefill"}]}]`, `[{"role":"model","parts":[{"text":"prefill"}]},{"role":"user","parts":[{"text":"Continue."}]}]`},
		{"empty", `[]`, `[{"role":"user","parts":[{"text":"Continue."}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"fixture-model","requestId":"fixture-id","request":{"contents":` + tc.contents + `,"sessionId":"fixture-session","generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}},"unknown":{"preserve":true}}}`)
			updated, err := appendAntigravityUserContinue(body)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, gjson.GetBytes(updated, "request.contents").Raw)
			withoutContents, err := sjson.DeleteBytes(updated, "request.contents")
			require.NoError(t, err)
			originalWithoutContents, err := sjson.DeleteBytes(body, "request.contents")
			require.NoError(t, err)
			require.Equal(t, string(originalWithoutContents), string(withoutContents))
		})
	}
}
