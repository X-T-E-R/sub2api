package service

import (
	"bytes"
	"fmt"
	"testing"
)

// A diagnostic-only fixture: unchanged native Responses, with no legacy aliases
// or compaction trigger. It never constructs a service or sends an HTTP request.
func BenchmarkPostReleaseNativeNoOp(b *testing.B) {
	for _, size := range []int{10, 30} {
		body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"`)
		body = append(body, bytes.Repeat([]byte("x"), size<<20)...)
		body = append(body, []byte(`"}]}],"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}`)...)
		checks := []struct {
			name string
			run func([]byte) ([]byte, bool, error)
		}{
			{"legacy", normalizeOpenAIResponsesLegacyIngress},
			{"compaction", NormalizeCompactionTriggerInputOrder},
			{"tool_schema", func(v []byte) ([]byte, bool, error) { return sanitizeOpenAIResponsesToolSchemasForPlatform(v, "openai") }},
		}
		for _, check := range checks {
			b.Run(fmt.Sprintf("%dMiB/%s", size, check.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for range b.N {
					out, changed, err := check.run(body)
					if err != nil || changed || len(out) != len(body) || &out[0] != &body[0] {
						b.Fatalf("expected unchanged native body: changed=%v err=%v", changed, err)
					}
				}
			})
		}
	}
}
