package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeToolResultMediaPartition(t *testing.T) {
	const input = `{"model":"client-alias","messages":[
		{"role":"user","content":"inspect the tool outputs"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"a","name":"read_images","input":{},"signature":"real-signature"},
			{"type":"tool_use","id":"b","name":"read_image","input":{}},
			{"type":"tool_use","id":"c","name":"read_pdf","input":{}}
		]},
		{"role":"user","content":[
			{"type":"text","text":"first note"},
			{"type":"tool_result","tool_use_id":"a","content":[
				{"type":"text","text":"one"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"+/8="}},
				{"type":"text","text":"two"},
				{"type":"video","source":{"type":"base64","media_type":"video/mp4","data":"VklE"}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"UE5H"}},
				{"type":"audio","source":{"type":"base64","media_type":"audio/wav","data":"V0FW"}},
				{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"dGV4dA=="}},
				{"type":"image","source":{"type":"url","url":"https://example.test/image.png"}}
			]},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"VVNFUg=="}},
			{"type":"tool_result","tool_use_id":"b","content":{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"V0VCUA=="}}},
			{"type":"tool_result","tool_use_id":"c","is_error":true,"content":{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}},
			{"type":"text","text":"last note"}
		]}
	]}`
	const wantParts = `[
		{"functionResponse":{"id":"a","name":"read_images","response":{"result":"one\ntwo"}}},
		{"functionResponse":{"id":"b","name":"read_image","response":{"result":"Command executed successfully."}}},
		{"functionResponse":{"id":"c","name":"read_pdf","response":{"result":"Tool execution failed with no output."}}},
		{"text":"2 images returned by tool read_images (call a):"},
		{"inlineData":{"mimeType":"image/jpeg","data":"+/8="}},
		{"inlineData":{"mimeType":"image/png","data":"UE5H"}},
		{"text":"Image returned by tool read_image (call b):"},
		{"inlineData":{"mimeType":"image/webp","data":"V0VCUA=="}},
		{"text":"Image returned by tool read_pdf (call c):"},
		{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}},
		{"text":"first note"},
		{"inlineData":{"mimeType":"image/png","data":"VVNFUg=="}},
		{"text":"last note"}
	]`
	for _, model := range []string{"claude-opus-5-5", "claude-opus-5-5-high", "claude-sonnet-5-5", " CLAUDE-sonnet-5-5 "} {
		t.Run(model, func(t *testing.T) {
			out, err := transformGeminiMessagesForTest(t, input, model, GeminiMessagesCompatibilityOptions{Enabled: true})
			require.NoError(t, err)
			require.Equal(t, []string{"user", "model", "user"}, geminiRolesForTest(out.Request.Contents))
			calls := out.Request.Contents[1].Parts
			require.Len(t, calls, 3)
			require.Equal(t, "real-signature", calls[0].ThoughtSignature)
			require.Empty(t, calls[1].ThoughtSignature, "Claude never receives a synthetic Gemini signature")
			parts, err := json.Marshal(out.Request.Contents[2].Parts)
			require.NoError(t, err)
			require.JSONEq(t, wantParts, string(parts))
		})
	}
}
