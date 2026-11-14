package antigravity

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func plainRewriteTextPart(part GeminiPart) bool {
	return !part.Thought && part.ThoughtSignature == "" && part.InlineData == nil && part.FunctionCall == nil && part.FunctionResponse == nil
}

// RewriteGeminiSystemInstructionContent rewrites plain text and removes empty
// parts. Removing the final part omits systemInstruction entirely on the wire.
func RewriteGeminiSystemInstructionContent(content **GeminiContent, enabled bool, rewriter *CompiledSystemInstructionRewriter) {
	if !enabled || rewriter == nil || len(rewriter.rules) == 0 || *content == nil {
		return
	}
	parts := (*content).Parts[:0]
	for _, part := range (*content).Parts {
		if plainRewriteTextPart(part) {
			part.Text = rewriter.Rewrite(part.Text)
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
		}
		parts = append(parts, part)
	}
	(*content).Parts = parts
	if len(parts) == 0 {
		*content = nil
	}
}

// RewriteGeminiUserContents never visits model turns or signed/tool/media parts.
func RewriteGeminiUserContents(contents []GeminiContent, enabled bool, rewriter *CompiledSystemInstructionRewriter) {
	if !enabled || rewriter == nil || len(rewriter.rules) == 0 {
		return
	}
	for i := range contents {
		if contents[i].Role != "user" && contents[i].Role != "" {
			continue
		}
		parts := contents[i].Parts[:0]
		removed := false
		for _, part := range contents[i].Parts {
			if plainRewriteTextPart(part) {
				rewritten := rewriter.RewriteUser(part.Text)
				if rewritten != part.Text && strings.TrimSpace(rewritten) == "" {
					removed = true
					continue
				}
				part.Text = rewritten
			}
			parts = append(parts, part)
		}
		if removed && len(parts) == 0 {
			parts = append(parts, GeminiPart{Text: geminiEmptyUserMessage})
		}
		contents[i].Parts = parts
	}
}

// RewriteV1InternalSystemInstructionWire handles native Gemini and v1internal
// envelopes without decoding/re-encoding signed parts or tool responses. Only
// plain text is changed; unchanged payloads retain their original bytes.
func RewriteV1InternalSystemInstructionWire(wire []byte, enabled bool, rewriter *CompiledSystemInstructionRewriter) []byte {
	if !enabled || rewriter == nil || len(rewriter.rules) == 0 {
		return wire
	}
	out := wire
	for _, root := range []string{"", "request."} {
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			path := root + key
			parts := gjson.GetBytes(out, path+".parts")
			if !parts.IsArray() {
				continue
			}
			out = rewriteWireTextParts(out, path+".parts", parts.Array(), rewriter, false)
			if len(gjson.GetBytes(out, path+".parts").Array()) == 0 {
				out, _ = sjson.DeleteBytes(out, path)
			}
		}
		contents := gjson.GetBytes(out, root+"contents").Array()
		for i, content := range contents {
			role := content.Get("role").String()
			if role != "user" && role != "" {
				continue
			}
			partsPath := root + "contents." + strconv.Itoa(i) + ".parts"
			parts := content.Get("parts").Array()
			out = rewriteWireTextParts(out, partsPath, parts, rewriter, true)
			if len(parts) > 0 && len(gjson.GetBytes(out, partsPath).Array()) == 0 {
				out, _ = sjson.SetBytes(out, partsPath, []GeminiPart{{Text: geminiEmptyUserMessage}})
			}
		}
	}
	return out
}

func plainWireRewriteTextPart(part gjson.Result) bool {
	for key, value := range part.Map() {
		if key == "text" || key == "thought" && value.Type == gjson.False {
			continue
		}
		return false
	}
	return true
}

func rewriteWireTextParts(wire []byte, path string, parts []gjson.Result, rewriter *CompiledSystemInstructionRewriter, user bool) []byte {
	// Work backwards so removing an empty part cannot move a later part's index.
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		text := part.Get("text")
		if text.Type != gjson.String || !plainWireRewriteTextPart(part) {
			continue
		}
		rewritten := ""
		if user {
			rewritten = rewriter.RewriteUser(text.Str)
		} else {
			rewritten = rewriter.Rewrite(text.Str)
		}
		partPath := path + "." + strconv.Itoa(i)
		if strings.TrimSpace(rewritten) == "" && (!user || rewritten != text.Str) {
			wire, _ = sjson.DeleteBytes(wire, partPath)
		} else if rewritten != text.Str {
			wire, _ = sjson.SetBytes(wire, partPath+".text", rewritten)
		}
	}
	return wire
}
