package service

import (
	"bytes"
	"encoding/json"
)

// openAIJSONObjectExcludesStrings is a conservative no-op guard, not a parser
// or field lookup. Any matching string (even a nested key or value) falls back
// to the normal decoder, which owns duplicate-key and transformation semantics.
// Validation stays with encoding/json; malformed and non-object inputs also
// fall back so callers retain their original errors.
func openAIJSONObjectExcludesStrings(body []byte, candidates ...string) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(body) {
		return false
	}
	for pos := 0; pos < len(body); {
		start := bytes.IndexByte(body[pos:], '"')
		if start < 0 {
			break
		}
		start += pos
		end := start + 1
		escaped := false
		for end < len(body) {
			n := bytes.IndexAny(body[end:], `"\`)
			end += n
			if body[end] == '"' {
				break
			}
			escaped = true
			end += 2 // Skip the escaped byte, including an escaped quote.
		}
		for _, candidate := range candidates {
			raw := body[start+1 : end]
			if !escaped {
				if string(raw) == candidate {
					return false
				}
			} else if len(raw) <= 6*len(candidate) {
				// These candidates are ASCII: each byte can occupy at most one
				// six-byte Unicode escape. Never unmarshal a large text token.
				var decoded string
				if json.Unmarshal(body[start:end+1], &decoded) != nil || decoded == candidate {
					return false
				}
			}
		}
		pos = end + 1
	}
	return true
}
