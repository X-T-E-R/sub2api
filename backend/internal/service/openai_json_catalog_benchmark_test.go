package service

import (
	"bytes"
	"fmt"
	"testing"
)

func openAINativeCatalogBody(size int) []byte {
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"`)
	body = append(body, bytes.Repeat([]byte("x"), size<<20)...)
	return append(body, []byte(`"}]}],"tools":[{"type":"function","name":"imagegen","parameters":{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}},{"type":"function","name":"create_thread","parameters":{"type":"object","properties":{"prompt":{"type":"string"},"messages":{"type":"array","items":{"type":"string"}}},"required":["prompt"]}},{"type":"function","name":"automation_update","parameters":{"type":"object","properties":{"prompt":{"type":"string"},"commands":{"type":"array","items":{"type":"string"}}},"required":["prompt"]}}]}`)...)
}

func BenchmarkOpenAINativeCatalogNoOp(b *testing.B) {
	for _, size := range []int{10, 30} {
		body := openAINativeCatalogBody(size)
		b.Run(fmt.Sprintf("%dMiB/legacy", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for range b.N {
				out, changed, err := normalizeOpenAIResponsesLegacyIngress(body)
				if err != nil || changed || len(out) != len(body) || &out[0] != &body[0] {
					b.Fatalf("expected unchanged native body: changed=%v err=%v", changed, err)
				}
			}
		})
	}
}
