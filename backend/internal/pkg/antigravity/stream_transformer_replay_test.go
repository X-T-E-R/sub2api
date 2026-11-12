package antigravity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamingProcessorReplayDiscardsEmptyGenerationSignatures(t *testing.T) {
	for _, carrier := range []string{`{"thought":true,"thoughtSignature":"sig-dead"}`, `{"text":"","thoughtSignature":"sig-dead"}`} {
		for _, next := range []string{
			`{"thought":true,"text":"Real thinking"}`, // No new signature: the old one must not survive.
			`{"thought":true,"text":"Real thinking","thoughtSignature":"sig-live"}`,
			`{"text":"answer"}`,
			`{"functionCall":{"name":"lookup","args":{"key":"value"}}}`,
		} {
			t.Run(carrier+"/"+next, func(t *testing.T) {
				p := NewStreamingProcessor("fixture")
				first := p.ProcessLine(`data: {"response":{"candidates":[{"content":{"parts":[` + carrier + `]}}],"usageMetadata":{"promptTokenCount":8,"thoughtsTokenCount":2}}}`)
				require.False(t, p.HasContent())
				p.AddUsageBaseline(p.CurrentUsage())
				p.DiscardEmptyGenerationCarriers()
				p.ResetGenerationUsage()
				second := p.ProcessLine(`data: {"response":{"candidates":[{"content":{"parts":[` + next + `]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":4}}}`)
				last, usage := p.Finish()
				wire := string(first) + string(second) + string(last)
				require.NotContains(t, wire, "sig-dead")
				require.Equal(t, 1, strings.Count(wire, "event: message_start\n"))
				require.Equal(t, 1, strings.Count(wire, "event: message_stop\n"))
				require.Equal(t, 28, usage.InputTokens)
				require.Equal(t, 6, usage.OutputTokens)
			})
		}
	}
}

func TestStreamingProcessorUnspecifiedDoesNotCloseThinking(t *testing.T) {
	p := NewStreamingProcessor("fixture")
	wire := p.ProcessLine(`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"thoughtSignature":"sig-dead"}]},"finishReason":"FINISH_REASON_UNSPECIFIED"}]}}`)
	require.NotContains(t, string(wire), "signature_delta")
	require.NotContains(t, string(wire), "content_block_stop")
	p.DiscardEmptyGenerationCarriers()
	p.ResetGenerationUsage()
	wire = p.ProcessLine(`data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Real thinking"},{"text":"answer"}]},"finishReason":"STOP"}]}}`)
	require.NotContains(t, string(wire), "sig-dead")
	require.Contains(t, string(wire), "Real thinking")
	require.Contains(t, string(wire), "answer")
}
