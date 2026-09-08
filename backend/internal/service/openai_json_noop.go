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
		end, escaped := openAIJSONStringEnd(body, start)
		if openAIJSONStringMatches(body[start:end+1], escaped, candidates) {
			return false
		}
		pos = end + 1
	}
	return true
}

// openAIJSONObjectExcludesTopLevelKeys ignores nested keys and string values,
// such as tool parameter names. Every top-level occurrence is checked (including
// escaped and duplicate keys); a match leaves decoding and conversion unchanged.
func openAIJSONObjectExcludesTopLevelKeys(body []byte, candidates ...string) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(body) {
		return false
	}
	depth := 0
	key := false
	for pos := 0; pos < len(body); pos++ {
		switch body[pos] {
		case '{', '[':
			depth++
			if depth == 1 {
				key = true
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				key = true
			}
		case '"':
			end, escaped := openAIJSONStringEnd(body, pos)
			if depth == 1 && key {
				if openAIJSONStringMatches(body[pos:end+1], escaped, candidates) {
					return false
				}
				key = false
			}
			pos = end
		}
	}
	return true
}

// openAIJSONStringEnd requires validated JSON and the opening quote position.
func openAIJSONStringEnd(body []byte, start int) (end int, escaped bool) {
	end = start + 1
	for {
		end += bytes.IndexAny(body[end:], `"\`)
		if body[end] == '"' {
			return end, escaped
		}
		escaped = true
		end += 2 // Skip the escaped byte, including an escaped quote.
	}
}

func openAIJSONStringMatches(token []byte, escaped bool, candidates []string) bool {
	raw := token[1 : len(token)-1]
	for _, candidate := range candidates {
		if !escaped {
			if string(raw) == candidate {
				return true
			}
		} else if len(raw) <= 6*len(candidate) {
			// These candidates are ASCII: each byte can occupy at most one
			// six-byte Unicode escape. Never unmarshal a large text token.
			var decoded string
			if json.Unmarshal(token, &decoded) != nil || decoded == candidate {
				return true
			}
		}
	}
	return false
}
