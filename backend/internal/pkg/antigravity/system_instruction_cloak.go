// Semantic rewordings and invisible-tolerant matching are adapted from Magpie
// (https://github.com/yetone/magpie, internal/gateway/codeassist.go):
// MIT License, Copyright (c) 2026 yetone.

package antigravity

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	rewriteInvisible = `[\x{00ad}\x{180e}\x{200b}-\x{200f}\x{2060}-\x{2064}\x{feff}\x{034f}]`
	rewriteSpace     = `[\s\v\p{Zs}\x{2028}\x{2029}\x{feff}]`
)

var defaultSystemInstructionRewritePhrases = []string{
	`x-anthropic-billing-header: => ""`,
	`re:(?im)^[ \t]*x-[a-z0-9_-]*billing[a-z0-9_-]*:[^\r\n]*(?:\r?\n)? => ""`,
	`built on Anthropic's Claude Agent SDK => built on the Claude Agent SDK by Anthropic`,
	`@user You are Codex, an agent based on GPT-5 => You are Codex, an agent built on GPT-5`,
	`@user You are Codex, a coding agent based on GPT-5 => You are Codex, a coding agent built on GPT-5`,
	`Hermes Agent, an intelligent AI assistant created by Nous Research => Hermes Agent, an intelligent AI assistant built by Nous Research`,
	`RFC 2119 keywords: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL. => RFC 2119 key words: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.`,
	`RFC 2119: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL. => RFC 2119 key words: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.`,
	`You are an AI agent running in OpenCode => You are an AI coding assistant`,
	`Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @"..." quotes a path containing spaces. => When the user writes @ followed by a path, they are pointing at that file or folder. Relative paths start at the workspace root; absolute paths are host paths. A path ending in a slash is a folder, so list it if its contents matter; otherwise it is a file, so open it with the read tool before relying on it. Paths with spaces appear as @"...".`,
}

// DefaultSystemInstructionRewritePhrases returns a defensive copy of the rules.
func DefaultSystemInstructionRewritePhrases() []string {
	return append([]string(nil), defaultSystemInstructionRewritePhrases...)
}

type instructionRewriteRule struct {
	pattern     *regexp.Regexp
	replacement string
	user        bool
}

// CompiledSystemInstructionRewriter is an immutable snapshot, safe to share
// across requests. Compile only when loading or changing settings.
type CompiledSystemInstructionRewriter struct {
	rules []instructionRewriteRule
}

func stripRewriteInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x00ad || r == 0x180e || r >= 0x200b && r <= 0x200f || r >= 0x2060 && r <= 0x2064 || r == 0xfeff || r == 0x034f {
			return -1
		}
		return r
	}, s)
}

func splitRewriteRule(line string) (target, replacement string, user bool) {
	target, replacement, _ = strings.Cut(line, "=>")
	target, replacement = strings.TrimSpace(target), strings.TrimSpace(replacement)
	if replacement == `""` {
		replacement = ""
	}
	for _, scope := range []string{"@user", "@system"} {
		if len(target) < len(scope) || !strings.EqualFold(target[:len(scope)], scope) {
			continue
		}
		rest := target[len(scope):]
		first, _ := utf8.DecodeRuneInString(rest)
		if rest == "" || unicode.IsSpace(first) {
			user = scope == "@user"
			target = strings.TrimSpace(target[len(scope):])
			break
		}
	}
	return
}

// NormalizeSystemInstructionRewritePhrases upgrades legacy phrases in place.
// Unknown bare phrases become deletion rules; an explicit [] stays empty.
// Regex targets and replacement text are preserved verbatim apart from trimming.
func NormalizeSystemInstructionRewritePhrases(phrases []string) []string {
	out := make([]string, 0, len(phrases))
	seen := make(map[string]bool)
	for _, raw := range phrases {
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, "=>") {
				line = upgradeLegacyRewritePhrase(stripRewriteInvisible(line))
			}
			target, replacement, user := splitRewriteRule(line)
			regex := strings.HasPrefix(target, "re:")
			if !regex {
				target = strings.Join(strings.Fields(stripRewriteInvisible(target)), " ")
			}
			scope := ""
			if user {
				scope = "@user "
			}
			line = scope + target + " => " + replacement
			if replacement == "" {
				line += `""`
			}
			key := line
			if !regex {
				key = scope + strings.ToLower(target) + " => " + replacement
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, line)
			}
		}
	}
	return out
}

func upgradeLegacyRewritePhrase(phrase string) string {
	key := strings.ToLower(strings.Join(strings.Fields(phrase), " "))
	if key == "rfc 2119 keywords" || key == "rfc 2119 keywords:" {
		return "RFC 2119 keywords: => RFC 2119 key words:"
	}
	for _, rule := range defaultSystemInstructionRewritePhrases {
		target, _, _ := splitRewriteRule(rule)
		if strings.EqualFold(key, target) {
			return rule
		}
	}
	return phrase + ` => ""`
}

func compileInstructionRewriteRule(line string) (instructionRewriteRule, error) {
	target, replacement, user := splitRewriteRule(line)
	if target == "" || target == "re:" {
		return instructionRewriteRule{}, fmt.Errorf("rewrite target must not be empty")
	}
	pattern := strings.TrimPrefix(target, "re:")
	if !strings.HasPrefix(target, "re:") {
		var b strings.Builder
		b.WriteString("(?i)")
		if replacement == "" && strings.HasSuffix(target, ":") {
			b.WriteString("(?m)(?:^[ \\t]*)?")
		}
		for i, word := range strings.Fields(target) {
			if i > 0 {
				b.WriteString("(?:" + rewriteSpace + rewriteInvisible + "*)+")
			}
			b.WriteString(rewriteInvisible + "*")
			for _, r := range word {
				b.WriteString(regexp.QuoteMeta(string(r)))
				b.WriteString(rewriteInvisible + "*")
			}
		}
		if replacement == "" && strings.HasSuffix(target, ":") {
			b.WriteString(`[^\r\n]*(?:\r?\n)?`)
		}
		pattern = b.String()
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return instructionRewriteRule{}, err
	}
	if re.MatchString("") {
		return instructionRewriteRule{}, fmt.Errorf("rewrite target must not match empty text")
	}
	return instructionRewriteRule{pattern: re, replacement: replacement, user: user}, nil
}

// ValidateSystemInstructionRewritePhrases validates administrator input before save.
func ValidateSystemInstructionRewritePhrases(phrases []string) error {
	for i, line := range NormalizeSystemInstructionRewritePhrases(phrases) {
		if _, err := compileInstructionRewriteRule(line); err != nil {
			return fmt.Errorf("antigravity_system_instruction_rewrite_phrases rule %d: %w", i+1, err)
		}
	}
	return nil
}

// NewCompiledSystemInstructionRewriter skips invalid stored rules. Admin saves
// use ValidateSystemInstructionRewritePhrases so invalid new rules are rejected.
func NewCompiledSystemInstructionRewriter(phrases []string) *CompiledSystemInstructionRewriter {
	r := &CompiledSystemInstructionRewriter{}
	for _, line := range NormalizeSystemInstructionRewritePhrases(phrases) {
		if rule, err := compileInstructionRewriteRule(line); err == nil {
			r.rules = append(r.rules, rule)
		}
	}
	return r
}

func ParseStoredSystemInstructionRewritePhrases(raw string) []string {
	var phrases []string
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &phrases) != nil {
		return DefaultSystemInstructionRewritePhrases()
	}
	out := []string{}
	for _, line := range NormalizeSystemInstructionRewritePhrases(phrases) {
		if _, err := compileInstructionRewriteRule(line); err == nil {
			out = append(out, line)
		}
	}
	return out
}

func FormatStoredSystemInstructionRewritePhrases(phrases []string) string {
	encoded, _ := json.Marshal(NormalizeSystemInstructionRewritePhrases(phrases))
	return string(encoded)
}

// Rewrite applies all system rules. Matches are selected from the original text
// only: leftmost first, longest at equal starts, declaration order for ties.
// Replacements are literal strings and never participate in this pass.
func (r *CompiledSystemInstructionRewriter) Rewrite(text string) string {
	return r.rewrite(text, false)
}

// RewriteUser applies only explicitly opted-in @user rules, including in code.
func (r *CompiledSystemInstructionRewriter) RewriteUser(text string) string {
	return r.rewrite(text, true)
}

func (r *CompiledSystemInstructionRewriter) rewrite(text string, user bool) string {
	if r == nil || len(r.rules) == 0 || text == "" {
		return text
	}
	type match struct{ start, end, rule int }
	var matches []match
	for i, rule := range r.rules {
		if user && !rule.user {
			continue
		}
		for _, loc := range rule.pattern.FindAllStringIndex(text, -1) {
			if loc[0] != loc[1] {
				matches = append(matches, match{loc[0], loc[1], i})
			}
		}
	}
	if len(matches) == 0 {
		return text
	}
	slices.SortFunc(matches, func(a, b match) int {
		if a.start != b.start {
			return a.start - b.start
		}
		if a.end != b.end {
			return b.end - a.end
		}
		return a.rule - b.rule
	})
	var out strings.Builder
	prev := 0
	for _, m := range matches {
		if m.start < prev {
			continue
		}
		out.WriteString(text[prev:m.start])
		out.WriteString(r.rules[m.rule].replacement)
		prev = m.end
	}
	out.WriteString(text[prev:])
	return out.String()
}

// RewriteSystemInstructionText is a convenience for non-request callers.
// Gateway request paths use the cached CompiledSystemInstructionRewriter.
func RewriteSystemInstructionText(text string, phrases []string) string {
	return NewCompiledSystemInstructionRewriter(phrases).Rewrite(text)
}
