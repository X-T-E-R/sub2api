package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityCollectedTerminalPreservesUsageDetails(t *testing.T) {
	for _, policyOn := range []bool{false, true} {
		for _, protocol := range []string{"messages", "chat", "responses", "gemini"} {
			t.Run(fmt.Sprintf("%s/A=%t", protocol, policyOn), func(t *testing.T) {
				// The final usage frame updates counters but omits modality details.
				line := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"promptTokensDetails":[{"modality":"TEXT","tokenCount":8}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":3}]}}}` + "\n\n" +
					`data: {"response":{"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4,"totalTokenCount":13}}}`
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
				svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: policyOn}, upstream)
				body := []byte(`{"model":"client-alias","stream":false,"messages":[{"role":"user","content":"question"}],"input":"question","contents":[{"role":"user","parts":[{"text":"question"}]}]}`)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/fixture", body)
				account := antigravityThoughtAccount(nil)
				var result *ForwardResult
				var err error
				switch protocol {
				case "messages":
					result, err = svc.Forward(context.Background(), c, account, body, false)
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				case "responses":
					result, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				case "gemini":
					result, err = svc.ForwardGemini(context.Background(), c, account, "gemini-3.1-pro-high", "generateContent", false, body, false)
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 9, result.Usage.InputTokens)
				require.Equal(t, 3, result.Usage.ImageOutputTokens)
				require.Equal(t, 1, upstream.callCount)
				if protocol == "gemini" {
					wire := recorder.Body.Bytes()
					require.Equal(t, int64(3), gjson.GetBytes(wire, "usageMetadata.candidatesTokensDetails.0.tokenCount").Int())
					require.Equal(t, "TEXT", gjson.GetBytes(wire, "usageMetadata.promptTokensDetails.0.modality").String())
				}
			})
		}
	}
}
