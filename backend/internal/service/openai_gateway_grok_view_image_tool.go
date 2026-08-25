package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	grokViewImageToolName      = "view_image"
	grokReadFileToolName       = "read_file"
	grokViewImagePathArgument  = "path"
	grokReadFileTargetArgument = "target_file"
)

var grokReadFileImageAliasDeclaration = map[string]any{
	"type":        "function",
	"name":        grokReadFileToolName,
	"description": "Load one local image file for visual inspection. Use only for image files; source, markup, and other text files are not supported.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			grokReadFileTargetArgument: map[string]any{
				"type":        "string",
				"description": "Path of the image file to inspect.",
			},
		},
		"required":             []any{grokReadFileTargetArgument},
		"additionalProperties": false,
	},
}

func grokViewImageReadFileAliasCandidate(
	body []byte,
	mapping apicompat.ResponsesClientToolMapping,
) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var requestBody map[string]any
	if err := decoder.Decode(&requestBody); err != nil {
		return false
	}
	tools, ok := requestBody["tools"].([]any)
	if !ok || mapping.CustomTools[grokViewImageToolName] || mapping.NamespaceTools[grokViewImageToolName].Namespace != "" {
		return false
	}
	viewImageFunctions := 0
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringValue(tool["name"]))
		switch name {
		case grokReadFileToolName:
			return false
		case grokViewImageToolName:
			if strings.TrimSpace(stringValue(tool["type"])) != "function" {
				return false
			}
			viewImageFunctions++
		}
	}
	return viewImageFunctions == 1
}

func adaptGrokViewImageReadFileAlias(
	body []byte,
	mapping apicompat.ResponsesClientToolMapping,
	enabled bool,
) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	if !enabled {
		return body, mapping, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var requestBody map[string]any
	if err := decoder.Decode(&requestBody); err != nil {
		return body, mapping, fmt.Errorf("decode Grok view_image alias request: %w", err)
	}
	if !apicompat.AdaptResponsesFunctionToolAlias(
		requestBody,
		&mapping,
		grokViewImageToolName,
		grokReadFileImageAliasDeclaration,
		grokViewImagePathArgument,
		grokReadFileTargetArgument,
	) {
		return body, mapping, nil
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, fmt.Errorf("encode Grok view_image alias request: %w", err)
	}
	return rebuilt, mapping, nil
}
