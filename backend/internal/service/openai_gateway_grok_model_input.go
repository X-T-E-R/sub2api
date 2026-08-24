package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sanitizeGrokResponsesModelInput converts replay items from OpenAI and Chat
// shapes into the subset accepted by xAI's ModelInput decoder. Call IDs are
// assigned in a separate pass so an output can safely appear before its call.
func sanitizeGrokResponsesModelInput(body []byte) ([]byte, error) {
	return sanitizeGrokResponsesModelInputWithCompat(body, true)
}

func sanitizeGrokResponsesModelInputWithCompat(body []byte, protocolCompat bool) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || input.Type == gjson.String {
		return body, nil
	}
	if input.IsObject() {
		var err error
		body, err = sjson.SetRawBytes(body, "input", []byte("["+input.Raw+"]"))
		if err != nil {
			return nil, fmt.Errorf("wrap Grok Responses model input: %w", err)
		}
		input = gjson.GetBytes(body, "input")
	}
	if !input.IsArray() {
		return body, nil
	}

	var items []any
	decoder := json.NewDecoder(bytes.NewReader([]byte(input.Raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&items); err != nil {
		return nil, fmt.Errorf("parse Grok Responses model input: %w", err)
	}
	callIDs, outputIDs := pairGrokReplayCallIDs(items)
	filtered := make([]any, 0, len(items))
	for index, rawItem := range items {
		agentProjected := false
		item, ok := rawItem.(map[string]any)
		if !ok {
			if text, ok := rawItem.(string); ok && strings.TrimSpace(text) != "" {
				filtered = append(filtered, map[string]any{"type": "message", "role": "user", "content": text})
			}
			continue
		}

		itemType := strings.ToLower(strings.TrimSpace(grokStringValue(item["type"])))
		role := strings.ToLower(strings.TrimSpace(grokStringValue(item["role"])))
		if role == "tool" || role == "function" || isGrokReplayOutputType(itemType) {
			output := firstNonNilGrokJSONValue(item["output"], item["content"], item["results"])
			normalizedOutput := any(grokModelInputString(output, "(empty)"))
			if protocolCompat {
				if content, ok := normalizeGrokToolOutputContentArray(output); ok {
					normalizedOutput = content
				}
			}
			filtered = append(filtered, map[string]any{
				"type":    "function_call_output",
				"call_id": outputIDs[index],
				"output":  normalizedOutput,
			})
			continue
		}

		switch itemType {
		case "agent_message":
			if protocolCompat {
				item = projectGrokAgentMessage(item)
				itemType = "message"
				role = "user"
				agentProjected = true
			}
		case "text", "input_text", "output_text":
			text := strings.TrimSpace(grokStringValue(item["text"]))
			if text == "" {
				continue
			}
			if role == "" {
				role = "user"
			}
			item = map[string]any{"type": "message", "role": role, "content": text}
			itemType = "message"
		case "custom_tool_call", "tool_search_call":
			originalType := itemType
			item["type"] = "function_call"
			itemType = "function_call"
			if strings.TrimSpace(grokStringValue(item["name"])) == "" {
				if originalType == "tool_search_call" {
					item["name"] = "tool_search"
				}
			}
		}

		if itemType == "" && role != "" {
			itemType = "message"
			item["type"] = itemType
		}
		if itemType == "message" {
			if role == "" {
				role = "user"
				item["role"] = role
			}
			if !agentProjected {
				content, keep := sanitizeGrokMessageContent(item["content"])
				if !keep {
					continue
				}
				item["content"] = content
			}
			if role == "assistant" && !grokIsCompleteOutputMessage(item) {
				if text, ok := collapseGrokAssistantOutputText(item["content"]); ok {
					item["content"] = text
				}
				delete(item, "id")
				delete(item, "status")
			}
		} else if itemType == "reasoning" {
			delete(item, "status")
			if content, exists := item["content"]; exists && content == nil {
				delete(item, "content")
			}
		} else if itemType == "function_call" {
			name := strings.TrimSpace(grokStringValue(item["name"]))
			arguments := item["arguments"]
			if function, ok := item["function"].(map[string]any); ok {
				if name == "" {
					name = strings.TrimSpace(grokStringValue(function["name"]))
				}
				if arguments == nil {
					arguments = function["arguments"]
				}
			}
			if arguments == nil {
				arguments = firstNonNilGrokJSONValue(item["input"], item["query"])
				if _, custom := item["input"]; custom {
					arguments = map[string]any{"input": arguments}
				}
			}
			if name == "" {
				name = "unknown_tool"
			}
			item["call_id"] = callIDs[index]
			item["name"] = name
			item["arguments"] = grokModelInputString(arguments, "{}")
			for _, field := range []string{"id", "status", "tool_call_id", "function", "input", "query", "execution"} {
				delete(item, field)
			}
		}
		if shouldStripOpenAIResponsesNonPairCallID(itemType) {
			delete(item, "call_id")
		}
		filtered = append(filtered, item)
	}

	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, fmt.Errorf("serialize Grok Responses model input: %w", err)
	}
	updated, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return nil, fmt.Errorf("set Grok Responses model input: %w", err)
	}
	return updated, nil
}

func isGrokToolOutputContentArray(output any) bool {
	_, ok := normalizeGrokToolOutputContentArray(output)
	return ok
}

func normalizeGrokToolOutputContentArray(output any) ([]any, bool) {
	parts, ok := output.([]any)
	if !ok || len(parts) == 0 {
		return nil, false
	}
	normalized := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, false
		}
		partType, ok := part["type"].(string)
		if !ok {
			return nil, false
		}
		switch partType {
		case "input_text":
			if _, ok := part["text"].(string); !ok {
				return nil, false
			}
			normalized = append(normalized, cloneGrokToolOutputContentPart(part))
		case "input_image":
			normalizedPart, ok := normalizeGrokToolOutputInputImagePart(part)
			if !ok {
				return nil, false
			}
			normalized = append(normalized, normalizedPart)
		default:
			return nil, false
		}
	}
	return normalized, true
}

func isGrokToolOutputInputImagePart(part map[string]any) bool {
	_, ok := normalizeGrokToolOutputInputImagePart(part)
	return ok
}

func normalizeGrokToolOutputInputImagePart(part map[string]any) (map[string]any, bool) {
	if _, exists := part["file_data"]; exists {
		return nil, false
	}
	if _, exists := part["file_url"]; exists {
		return nil, false
	}
	imageURL, hasImageURL := part["image_url"]
	fileID, hasFileID := part["file_id"]
	if hasImageURL == hasFileID {
		return nil, false
	}
	normalized := cloneGrokToolOutputContentPart(part)
	verifiedInlineImage := false
	if hasImageURL {
		value, ok := imageURL.(string)
		if !ok {
			return nil, false
		}
		image, ok := normalizeGrokToolOutputImageURL(value)
		if !ok {
			return nil, false
		}
		normalized["image_url"] = image.value
		verifiedInlineImage = !image.inlineData || image.verified
	}
	if hasFileID {
		value, ok := fileID.(string)
		if !ok || value == "" || value != strings.TrimSpace(value) {
			return nil, false
		}
	}
	if rawDetail, exists := part["detail"]; exists {
		detail, ok := rawDetail.(string)
		if !ok {
			return nil, false
		}
		switch detail {
		case "auto", "low", "high":
		case "original":
			// xAI accepts high in the same semantic slot. Inline data is
			// verified before changing the caller's declared detail so malformed
			// data URLs retain the existing string fallback.
			if hasImageURL && !verifiedInlineImage {
				return nil, false
			}
			normalized["detail"] = "high"
		default:
			return nil, false
		}
	}
	return normalized, true
}

func isGrokToolOutputImageURL(value string) bool {
	_, ok := normalizeGrokToolOutputImageURL(value)
	return ok
}

type grokToolOutputImageURLNormalization struct {
	value      string
	inlineData bool
	verified   bool
}

func normalizeGrokToolOutputImageURL(value string) (grokToolOutputImageURLNormalization, bool) {
	if value == "" || value != strings.TrimSpace(value) || isEmptyBase64DataURI(value) {
		return grokToolOutputImageURLNormalization{}, false
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return grokToolOutputImageURLNormalization{}, false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return grokToolOutputImageURLNormalization{value: value}, parsed.Host != ""
	case "data":
		return normalizeGrokToolOutputImageDataURL(value)
	default:
		return grokToolOutputImageURLNormalization{}, false
	}
}

const maxGrokToolOutputImageBytes = 20 << 20

func normalizeGrokToolOutputImageDataURL(value string) (grokToolOutputImageURLNormalization, bool) {
	comma := strings.IndexByte(value, ',')
	if comma < len("data:x;base64") || comma == len(value)-1 {
		return grokToolOutputImageURLNormalization{}, false
	}
	metadata := value[len("data:"):comma]
	separator := strings.LastIndexByte(metadata, ';')
	if separator <= 0 || !strings.EqualFold(metadata[separator+1:], "base64") {
		return grokToolOutputImageURLNormalization{}, false
	}
	declaredMIME := strings.ToLower(metadata[:separator])
	payload := value[comma+1:]
	detectedMIME, sniffed := sniffGrokToolOutputImageBase64(payload)
	if declaredMIME == "application/octet-stream" {
		if !sniffed {
			return grokToolOutputImageURLNormalization{}, false
		}
		return grokToolOutputImageURLNormalization{
			value:      "data:" + detectedMIME + ";base64," + payload,
			inlineData: true,
			verified:   true,
		}, true
	}
	if !strings.HasPrefix(declaredMIME, "image/") {
		return grokToolOutputImageURLNormalization{}, false
	}
	return grokToolOutputImageURLNormalization{
		value:      value,
		inlineData: true,
		verified:   sniffed && declaredMIME == detectedMIME,
	}, true
}

func sniffGrokToolOutputImageBase64(payload string) (string, bool) {
	const maxEncodedBytes = ((maxGrokToolOutputImageBytes + 2) / 3) * 4
	if payload == "" || len(payload) > maxEncodedBytes || !isStrictGrokToolOutputBase64(payload) {
		return "", false
	}

	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(payload))
	sniff := make([]byte, 512)
	n, err := io.ReadFull(decoder, sniff)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", false
	}
	if n == 0 {
		return "", false
	}
	if err == nil {
		if _, err := io.Copy(io.Discard, decoder); err != nil {
			return "", false
		}
	}

	detected := http.DetectContentType(sniff[:n])
	switch detected {
	case "image/gif", "image/jpeg", "image/png", "image/webp":
		return detected, true
	default:
		return "", false
	}
}

func isStrictGrokToolOutputBase64(payload string) bool {
	if len(payload)%4 != 0 {
		return false
	}
	padding := 0
	for index := 0; index < len(payload); index++ {
		char := payload[index]
		if char == '=' {
			padding++
			if padding > 2 || index < len(payload)-2 {
				return false
			}
			continue
		}
		if padding != 0 || !((char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '+' || char == '/') {
			return false
		}
	}
	return true
}

func cloneGrokToolOutputContentPart(part map[string]any) map[string]any {
	cloned := make(map[string]any, len(part))
	for key, value := range part {
		cloned[key] = value
	}
	return cloned
}

const grokAgentMessageMetadataLabel = "[sub2api client-declared inter-agent metadata]"

type grokAgentMessageMetadata struct {
	Author       string   `json:"author"`
	Recipient    string   `json:"recipient"`
	ContentTypes []string `json:"content_types"`
}

func projectGrokAgentMessage(item map[string]any) map[string]any {
	contentTypes, sourceTexts := grokAgentMessageSources(item["content"])
	metadata, _ := json.Marshal(grokAgentMessageMetadata{
		Author:       grokStringValue(item["author"]),
		Recipient:    grokStringValue(item["recipient"]),
		ContentTypes: contentTypes,
	})
	content := make([]any, 0, len(sourceTexts)+1)
	content = append(content, map[string]any{
		"type": "input_text",
		"text": grokAgentMessageMetadataLabel + "\n" + string(metadata),
	})
	for _, text := range sourceTexts {
		content = append(content, map[string]any{"type": "input_text", "text": text})
	}
	return map[string]any{"type": "message", "role": "user", "content": content}
}

func grokAgentMessageSources(content any) ([]string, []string) {
	if text, ok := content.(string); ok {
		return []string{"input_text"}, []string{text}
	}
	parts, ok := content.([]any)
	if !ok {
		return []string{"unknown"}, []string{grokCanonicalJSONText(content)}
	}
	types := make([]string, 0, len(parts))
	texts := make([]string, 0, len(parts))
	for _, rawPart := range parts {
		part, isObject := rawPart.(map[string]any)
		partType := "unknown"
		if isObject {
			if declared := grokStringValue(part["type"]); declared != "" {
				partType = declared
			}
			switch partType {
			case "input_text":
				if text, ok := part["text"].(string); ok {
					types = append(types, partType)
					texts = append(texts, text)
					continue
				}
			case "encrypted_content":
				if text, ok := part["encrypted_content"].(string); ok {
					types = append(types, partType)
					texts = append(texts, text)
					continue
				}
			}
		}
		types = append(types, partType)
		texts = append(texts, grokCanonicalJSONText(rawPart))
	}
	return types, texts
}

func grokCanonicalJSONText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

func pairGrokReplayCallIDs(items []any) (map[int]string, map[int]string) {
	callIDs := make(map[int]string)
	outputIDs := make(map[int]string)
	aliases := make(map[string]string)
	conflictingAliases := make(map[string]struct{})
	pendingCalls := make([]string, 0)
	nextID := 0
	synthetic := func() string {
		nextID++
		return fmt.Sprintf("grok_replay_call_%d", nextID)
	}
	registerAlias := func(alias, canonical string) {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return
		}
		if _, conflict := conflictingAliases[alias]; conflict {
			return
		}
		if existing, exists := aliases[alias]; exists && existing != canonical {
			delete(aliases, alias)
			conflictingAliases[alias] = struct{}{}
			return
		}
		aliases[alias] = canonical
	}

	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.ToLower(strings.TrimSpace(grokStringValue(item["type"])))
		if !isGrokReplayCallType(itemType) {
			continue
		}
		callID := firstNonEmptyGrokString(item["call_id"], item["tool_call_id"])
		if callID == "" {
			callID = synthetic()
		}
		callIDs[index] = callID
		pendingCalls = append(pendingCalls, callID)
		registerAlias(callID, callID)
		registerAlias(grokStringValue(item["call_id"]), callID)
		registerAlias(grokStringValue(item["tool_call_id"]), callID)
		registerAlias(grokStringValue(item["id"]), callID)
	}

	consumedCalls := make(map[string]struct{})
	consumeNextCall := func() string {
		for _, callID := range pendingCalls {
			if _, consumed := consumedCalls[callID]; consumed {
				continue
			}
			consumedCalls[callID] = struct{}{}
			return callID
		}
		return synthetic()
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.ToLower(strings.TrimSpace(grokStringValue(item["type"])))
		role := strings.ToLower(strings.TrimSpace(grokStringValue(item["role"])))
		if role != "tool" && role != "function" && !isGrokReplayOutputType(itemType) {
			continue
		}
		alias := firstNonEmptyGrokString(item["call_id"], item["tool_call_id"], item["id"])
		_, conflict := conflictingAliases[alias]
		if canonical := aliases[alias]; canonical != "" && !conflict {
			outputIDs[index] = canonical
			consumedCalls[canonical] = struct{}{}
		} else if firstNonEmptyGrokString(item["call_id"], item["tool_call_id"]) != "" {
			// Explicit call identifiers remain authoritative even when an invalid
			// transcript reuses the same identifier for more than one call.
			outputIDs[index] = alias
		} else if conflict {
			// An item ID shared by multiple calls cannot safely identify either
			// one. Keep it out of the call_id namespace and leave the output orphaned.
			outputIDs[index] = synthetic()
		} else {
			outputIDs[index] = consumeNextCall()
		}
	}
	return callIDs, outputIDs
}

func isGrokReplayCallType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func isGrokReplayOutputType(itemType string) bool {
	switch itemType {
	case "function_call_output", "custom_tool_call_output", "tool_search_output", "tool_search_call_output":
		return true
	default:
		return false
	}
}

func firstNonEmptyGrokString(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(grokStringValue(value)); text != "" {
			return text
		}
	}
	return ""
}

func firstNonNilGrokJSONValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func sanitizeGrokMessageContent(content any) (any, bool) {
	switch value := content.(type) {
	case nil:
		return nil, false
	case string:
		return value, strings.TrimSpace(value) != ""
	case []any:
		filtered := make([]any, 0, len(value))
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				if text, isText := rawPart.(string); !isText || strings.TrimSpace(text) != "" {
					filtered = append(filtered, rawPart)
				}
				continue
			}
			switch strings.ToLower(strings.TrimSpace(grokStringValue(part["type"]))) {
			case "text", "input_text", "output_text":
				if strings.TrimSpace(grokStringValue(part["text"])) == "" {
					continue
				}
			case "image_url", "input_image":
				if !grokContentPartHasImageURL(part) {
					continue
				}
			}
			filtered = append(filtered, part)
		}
		return filtered, len(filtered) > 0
	default:
		return content, true
	}
}

func grokContentPartHasImageURL(part map[string]any) bool {
	for _, field := range []string{"file_id", "file_data"} {
		if strings.TrimSpace(grokStringValue(part[field])) != "" {
			return true
		}
	}
	raw := firstNonNilGrokJSONValue(part["image_url"], part["file_url"])
	switch value := raw.(type) {
	case string:
		value = strings.TrimSpace(value)
		return value != "" && !isEmptyBase64DataURI(value)
	case map[string]any:
		url := strings.TrimSpace(grokStringValue(value["url"]))
		return url != "" && !isEmptyBase64DataURI(url)
	default:
		return false
	}
}

func grokStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func grokModelInputString(value any, fallback string) string {
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return fallback
		}
		return text
	}
	if value == nil {
		return fallback
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || string(encoded) == "null" {
		return fallback
	}
	return string(encoded)
}

func grokIsCompleteOutputMessage(item map[string]any) bool {
	return strings.TrimSpace(grokStringValue(item["id"])) != "" &&
		strings.TrimSpace(grokStringValue(item["status"])) != ""
}

func collapseGrokAssistantOutputText(content any) (string, bool) {
	parts, ok := content.([]any)
	if !ok || len(parts) == 0 {
		return "", false
	}
	var text strings.Builder
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(grokStringValue(part["type"]))) != "output_text" {
			return "", false
		}
		value, ok := part["text"].(string)
		if !ok {
			return "", false
		}
		_, _ = text.WriteString(value)
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", false
	}
	return text.String(), true
}
