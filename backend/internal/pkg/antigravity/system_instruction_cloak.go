// Portions of the Antigravity system-instruction phrase cloaking logic and
// default phrase list are adapted from Magpie commit 627fd1a
// (https://github.com/yetone/magpie, internal/gateway/codeassist.go):
// MIT License, Copyright (c) 2026 yetone.

package antigravity

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const zeroWidthSpace = "\u200b"

// defaultSystemInstructionRewritePhrases holds the exact five lowercase phrases
// from Magpie 627fd1a (github.com/yetone/magpie, MIT License, Copyright (c) 2026 yetone).
var defaultSystemInstructionRewritePhrases = []string{
	"x-anthropic-billing-header:",
	"built on anthropic's claude agent sdk",
	"you are codex, an agent based on gpt-5",
	"you are codex, a coding agent based on gpt-5",
	"hermes agent, an intelligent ai assistant created by nous research",
}

// DefaultSystemInstructionRewritePhrases returns a defensive copy of the
// default five Antigravity system-instruction rewrite phrases.
func DefaultSystemInstructionRewritePhrases() []string {
	return append([]string(nil), defaultSystemInstructionRewritePhrases...)
}

// NormalizeSystemInstructionRewritePhrases trims each entry, strips any
// embedded U+200B characters, drops empty lines, and deduplicates entries
// case-insensitively (ASCII) while preserving first-seen order.
// Always returns a non-nil slice so JSON marshaling produces [] rather than null.
func NormalizeSystemInstructionRewritePhrases(phrases []string) []string {
	if len(phrases) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(phrases))
	seen := make(map[string]struct{}, len(phrases))
	for _, raw := range phrases {
		for _, line := range strings.Split(raw, "\n") {
			cleaned := strings.TrimSpace(strings.ReplaceAll(line, zeroWidthSpace, ""))
			if cleaned == "" {
				continue
			}
			key := asciiLower(cleaned)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, cleaned)
		}
	}
	return out
}

// ParseStoredSystemInstructionRewritePhrases parses the persisted JSON array
// value from the settings store.
//   - An empty raw string ("") means the setting has not been configured yet
//     and falls back to DefaultSystemInstructionRewritePhrases().
//   - An explicit JSON array such as "[]" returns that normalized list (which
//     may be empty when the administrator intentionally cleared all phrases).
func ParseStoredSystemInstructionRewritePhrases(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultSystemInstructionRewritePhrases()
	}
	var parsed []string
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return DefaultSystemInstructionRewritePhrases()
	}
	return NormalizeSystemInstructionRewritePhrases(parsed)
}

// FormatStoredSystemInstructionRewritePhrases serializes the normalized phrase
// list as a JSON array string (e.g. "[]" when empty).
func FormatStoredSystemInstructionRewritePhrases(phrases []string) string {
	normalized := NormalizeSystemInstructionRewritePhrases(phrases)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// RewriteSystemInstructionText inserts a U+200B zero-width space right after
// the first UTF-8 rune of every ASCII case-insensitive occurrence of any
// configured phrase in system.
//
// Matching uses asciiLower so byte offsets in the lowered string remain 1:1
// with the original UTF-8 string even when system or phrases contain multi-byte
// Unicode runes whose Unicode lowercase mapping would change byte length
// (for example U+0130 'İ'). Each match advances by one UTF-8 rune from the
// match start so overlapping matches (for example phrase "aaa" in "aaaa") are
// collected in a single pass, and already-cloaked positions are skipped so
// repeated calls are strictly idempotent.
func RewriteSystemInstructionText(system string, phrases []string) string {
	if system == "" || len(phrases) == 0 {
		return system
	}
	low := asciiLower(system)
	var at []int
	for _, rawPhrase := range phrases {
		phrase := strings.TrimSpace(strings.ReplaceAll(rawPhrase, zeroWidthSpace, ""))
		if phrase == "" {
			continue
		}
		needle := asciiLower(phrase)
		for i := 0; i < len(low); {
			j := strings.Index(low[i:], needle)
			if j < 0 {
				break
			}
			matchStart := i + j
			_, firstRuneLen := utf8.DecodeRuneInString(system[matchStart:])
			if firstRuneLen <= 0 {
				firstRuneLen = 1
			}
			insertPos := matchStart + firstRuneLen
			if !strings.HasPrefix(system[insertPos:], zeroWidthSpace) {
				at = append(at, insertPos)
			}
			i = matchStart + firstRuneLen
		}
	}
	if len(at) == 0 {
		return system
	}
	slices.Sort(at)
	at = slices.Compact(at)

	var out strings.Builder
	out.Grow(len(system) + len(at)*len(zeroWidthSpace))
	prev := 0
	for _, pos := range at {
		out.WriteString(system[prev:pos])
		out.WriteString(zeroWidthSpace)
		prev = pos
	}
	out.WriteString(system[prev:])
	return out.String()
}

// RewriteGeminiSystemInstructionContent rewrites all non-empty text parts of a
// GeminiContent systemInstruction in place when enabled is true.
func RewriteGeminiSystemInstructionContent(content *GeminiContent, enabled bool, phrases []string) {
	if !enabled || content == nil || len(content.Parts) == 0 || len(phrases) == 0 {
		return
	}
	for i := range content.Parts {
		if content.Parts[i].Text == "" {
			continue
		}
		content.Parts[i].Text = RewriteSystemInstructionText(content.Parts[i].Text, phrases)
	}
}

// RewriteV1InternalSystemInstructionWire rewrites only the outbound
// systemInstruction / system_instruction text parts in a v1internal or Gemini
// JSON payload when enabled is true. When enabled is false or no phrase
// matches, wire is returned unchanged (exact same slice/bytes).
func RewriteV1InternalSystemInstructionWire(wire []byte, enabled bool, phrases []string) []byte {
	if !enabled || len(wire) == 0 || len(phrases) == 0 {
		return wire
	}
	out := wire
	for _, basePath := range []string{
		"request.systemInstruction.parts",
		"request.system_instruction.parts",
		"systemInstruction.parts",
		"system_instruction.parts",
	} {
		parts := gjson.GetBytes(out, basePath)
		if !parts.Exists() || !parts.IsArray() {
			continue
		}
		idx := 0
		parts.ForEach(func(_, part gjson.Result) bool {
			textRes := part.Get("text")
			if textRes.Exists() && textRes.Type == gjson.String && textRes.Str != "" {
				rewritten := RewriteSystemInstructionText(textRes.Str, phrases)
				if rewritten != textRes.Str {
					textPath := basePath + "." + itoaIndex(idx) + ".text"
					if updated, err := sjson.SetBytes(out, textPath, rewritten); err == nil {
						out = updated
					}
				}
			}
			idx++
			return true
		})
	}
	return out
}

func asciiLower(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func itoaIndex(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
