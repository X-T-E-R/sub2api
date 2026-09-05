package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// GeminiMessagesCompatibilityOptions controls the Antigravity Messages adapter.
// The Messages gateway opts in; other callers keep the existing conversion.
type GeminiMessagesCompatibilityOptions struct {
	Enabled bool
	Models  []string
}

type geminiMessagesConversionOptions struct {
	enabled               bool
	toolResultImages      bool
	terminalMessage       int
	recoveredThinkingTail bool
}

func supportsGeminiToolResultImages(model string) bool {
	version, ok := strings.CutPrefix(model, "gemini-")
	if !ok {
		return false
	}
	if end := strings.IndexAny(version, ".-"); end >= 0 {
		version = version[:end]
	}
	major, err := strconv.Atoi(version)
	return err == nil && major >= 3
}

// Only explicitly disposable assistant blocks may be skipped. Unsupported
// blocks must not count as empty merely because the parts builder omits them.
func effectiveGeminiTerminalMessage(messages []ClaudeMessage, skipThinking bool) int {
	last := len(messages) - 1
	for last >= 0 && messages[last].Role == "assistant" {
		content := bytes.TrimSpace(messages[last].Content)
		if len(content) == 0 || bytes.Equal(content, []byte("null")) {
			break
		}
		var text string
		if json.Unmarshal(content, &text) == nil {
			if strings.TrimSpace(text) != "" {
				break
			}
			last--
			continue
		}
		var blocks []ContentBlock
		if json.Unmarshal(content, &blocks) != nil {
			break
		}
		empty := true
		for _, block := range blocks {
			if skipThinking && block.Type == "thinking" {
				continue
			}
			if block.Type != "text" || strings.TrimSpace(block.Text) != "" {
				empty = false
				break
			}
		}
		if !empty {
			break
		}
		last--
	}
	return last
}

func (o GeminiMessagesCompatibilityOptions) enabledForModel(model string) bool {
	if !o.Enabled || !strings.HasPrefix(model, "gemini-") {
		return false
	}
	if len(o.Models) == 0 {
		return true
	}
	for _, allowed := range o.Models {
		if model == strings.TrimSpace(allowed) {
			return true
		}
	}
	return false
}

const (
	geminiAssistantContinuation = "[sub2api:assistant-prefill]\nContinue the preceding assistant message from where it ended. Do not repeat its existing text."
	geminiEmptyUserMessage      = "[sub2api:empty-user]\nThe client sent an empty user message.\n"
)

// GeminiMessagesCompatibilityError identifies input that the turn adapter cannot
// preserve. Its message contains only an input location and a recovery instruction.
type GeminiMessagesCompatibilityError struct {
	path   string
	reason string
}

func (e *GeminiMessagesCompatibilityError) Error() string {
	return e.path + ": " + e.reason
}

func geminiMessagesError(path, reason string) error {
	return &GeminiMessagesCompatibilityError{path: path, reason: reason}
}

// Only the terminal input, or history used for a synthetic continuation, is
// checked here. Ordinary user-ended requests keep their existing history policy.
func validateGeminiMessageContent(content json.RawMessage, index int, toolResultImages bool) error {
	path := fmt.Sprintf("messages[%d].content", index)
	if len(bytes.TrimSpace(content)) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return geminiMessagesError(path, "send a string or content block array for Gemini Messages")
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return geminiMessagesError(path, "send a string or content block array for Gemini Messages")
	}
	for i, block := range blocks {
		blockPath := fmt.Sprintf("%s[%d]", path, i)
		switch block.Type {
		case "text", "thinking", "tool_use":
		case "image":
			if block.Source == nil || block.Source.Type != "base64" {
				return geminiMessagesError(blockPath, "send the image as a base64 source for Gemini Messages")
			}
		case "tool_result":
			var resultBlocks []ContentBlock
			if json.Unmarshal(block.Content, &resultBlocks) == nil {
				for j, resultBlock := range resultBlocks {
					resultPath := fmt.Sprintf("%s.content[%d]", blockPath, j)
					switch resultBlock.Type {
					case "text":
					case "image":
						if resultBlock.Source == nil || resultBlock.Source.Type != "base64" {
							return geminiMessagesError(resultPath, "send the tool-result image as a base64 source")
						}
						if !toolResultImages {
							return geminiMessagesError(resultPath, "image tool results require a Gemini 3+ final model")
						}
					default:
						return geminiMessagesError(resultPath, "send text or supported base64 images as tool-result content for Gemini Messages")
					}
				}
			}
		default:
			return geminiMessagesError(blockPath, "send text, base64 images, thinking, or tool blocks supported by Gemini Messages")
		}
	}
	return nil
}

func emptyGeminiTextParts(parts []GeminiPart) bool {
	for _, part := range parts {
		if part.Thought || part.InlineData != nil || part.FunctionCall != nil || part.FunctionResponse != nil || strings.TrimSpace(part.Text) != "" {
			return false
		}
	}
	return true
}

func finalizeGeminiMessages(messages []ClaudeMessage, contents []GeminiContent, toolResultImages bool) ([]GeminiContent, error) {
	if len(contents) == 0 || contents[len(contents)-1].Role != "model" {
		return contents, nil
	}
	last := len(messages) - 1
	path := fmt.Sprintf("messages[%d]", last)
	if last < 0 || messages[last].Role != "assistant" {
		return nil, geminiMessagesError(path, "send a final user message; Gemini continuation requires an explicit assistant text prefill")
	}
	// A dropped empty assistant must not turn an earlier model message into a prefill.
	var text string
	var blocks []ContentBlock
	inputHasText := false
	if json.Unmarshal(messages[last].Content, &text) == nil {
		inputHasText = strings.TrimSpace(text) != ""
	} else if json.Unmarshal(messages[last].Content, &blocks) == nil {
		for _, block := range blocks {
			inputHasText = inputHasText || (block.Type == "text" && strings.TrimSpace(block.Text) != "")
		}
	}
	hasText := false
	for _, part := range contents[len(contents)-1].Parts {
		if part.FunctionCall != nil || part.FunctionResponse != nil || part.InlineData != nil {
			return nil, geminiMessagesError(path, "send the real tool results or a user message; Gemini continuation supports assistant text prefills")
		}
		if !part.Thought && strings.TrimSpace(part.Text) != "" {
			hasText = true
		}
	}
	if !hasText || !inputHasText {
		return nil, geminiMessagesError(path, "send a user message or a nonempty assistant text prefill for Gemini continuation")
	}
	if err := validateGeminiContinuationHistory(messages, toolResultImages); err != nil {
		return nil, err
	}
	return append(contents, GeminiContent{Role: "user", Parts: []GeminiPart{{Text: geminiAssistantContinuation}}}), nil
}

// Validate pairing before synthesizing a user turn or recovering a partial
// assistant suffix. Neither operation may hide a missing tool result.
func validateGeminiContinuationHistory(messages []ClaudeMessage, toolResultImages bool) error {
	pending := make(map[string]string)
	seen := make(map[string]bool)
	previousRole := ""
	for i, message := range messages {
		if err := validateGeminiMessageContent(message.Content, i, toolResultImages); err != nil {
			return err
		}
		if message.Role == "system" {
			continue
		}
		path := fmt.Sprintf("messages[%d]", i)
		if message.Role != "user" && message.Role != "assistant" {
			return geminiMessagesError(path, "use user and assistant message roles for Gemini continuation")
		}
		if message.Role == "assistant" && previousRole == "user" && len(pending) > 0 {
			return geminiMessagesError(path, "include all real tool_result blocks before the next assistant turn")
		}
		var blocks []ContentBlock
		_ = json.Unmarshal(message.Content, &blocks)
		for j, block := range blocks {
			blockPath := fmt.Sprintf("%s.content[%d]", path, j)
			switch block.Type {
			case "tool_use":
				if message.Role != "assistant" || block.ID == "" || block.Name == "" || seen[block.ID] {
					return geminiMessagesError(blockPath, "send an assistant tool_use with a unique id and a name before continuing")
				}
				seen[block.ID] = true
				pending[block.ID] = block.Name
			case "tool_result":
				name, ok := pending[block.ToolUseID]
				if message.Role != "user" || !ok || (block.Name != "" && block.Name != name) {
					return geminiMessagesError(blockPath, "match each user tool_result to its preceding tool_use id and name before continuing")
				}
				delete(pending, block.ToolUseID)
			}
		}
		previousRole = message.Role
	}
	if len(pending) > 0 {
		return geminiMessagesError("messages", "include the real tool_result for every pending tool_use before continuing")
	}
	return nil
}
