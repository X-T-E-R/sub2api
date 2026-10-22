package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func transformContract(t *testing.T, body, model string) (V1InternalRequest, string) {
	t.Helper()
	var req ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	raw, err := TransformClaudeToGeminiWithOptions(&req, "project", model, TransformOptions{})
	require.NoError(t, err)
	var doc V1InternalRequest
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc, string(raw)
}

func declarationNameOf(t *testing.T, doc V1InternalRequest) string {
	t.Helper()
	require.NotEmpty(t, doc.Request.Tools)
	require.NotEmpty(t, doc.Request.Tools[0].FunctionDeclarations)
	return doc.Request.Tools[0].FunctionDeclarations[0].Name
}

func historicalToolSignature(t *testing.T, doc V1InternalRequest) string {
	t.Helper()
	for _, content := range doc.Request.Contents {
		if content.Role != "model" {
			continue
		}
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				return part.ThoughtSignature
			}
		}
	}
	return ""
}

func cachePrefixOf(doc V1InternalRequest) (tools []GeminiToolDeclaration, system *GeminiContent, first GeminiContent) {
	var firstContent GeminiContent
	if len(doc.Request.Contents) > 0 {
		firstContent = doc.Request.Contents[0]
	}
	return doc.Request.Tools, doc.Request.SystemInstruction, firstContent
}

const toolHistoryBody = `{
  "model":"client-alias",
  "max_tokens":128,
  "tools":[{"name":"get_time","input_schema":{"type":"object","properties":{"tz":{"type":"string"}}}}],
  "messages":[
    {"role":"user","content":"What time is it?"},
    {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_time","input":{"tz":"UTC"},"signature":"real-signature-value"}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"12:00 UTC"}]}
  ]
}`

func TestExplicitToolChoiceNoneKeepsDeclarationsAndSignature(t *testing.T) {
	body := strings.Replace(toolHistoryBody, `"max_tokens":128,`, `"max_tokens":128,"tool_choice":{"type":"none"},`, 1)
	doc, _ := transformContract(t, body, "gemini-3-flash")
	require.Equal(t, "NONE", doc.Request.ToolConfig.FunctionCallingConfig.Mode)
	require.Equal(t, "get_time", declarationNameOf(t, doc))
	require.Equal(t, "real-signature-value", historicalToolSignature(t, doc))
	require.Empty(t, doc.Request.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
}

func TestToolChoiceStringNoneAutoAny(t *testing.T) {
	cases := []struct {
		raw  string
		mode string
	}{
		{raw: `"none"`, mode: "NONE"},
		{raw: `"auto"`, mode: "AUTO"},
		{raw: `"any"`, mode: "ANY"},
		{raw: `{"type":"auto"}`, mode: "AUTO"},
		{raw: `{"type":"any"}`, mode: "ANY"},
	}
	for _, tc := range cases {
		t.Run(tc.mode+" "+tc.raw, func(t *testing.T) {
			body := strings.Replace(toolHistoryBody, `"max_tokens":128,`, `"max_tokens":128,"tool_choice":`+tc.raw+`,`, 1)
			doc, _ := transformContract(t, body, "gemini-3-flash")
			require.Equal(t, tc.mode, doc.Request.ToolConfig.FunctionCallingConfig.Mode)
			require.Equal(t, "get_time", declarationNameOf(t, doc))
			require.Equal(t, "real-signature-value", historicalToolSignature(t, doc))
		})
	}
}

func TestOmittedToolChoiceStaysValidated(t *testing.T) {
	doc, _ := transformContract(t, toolHistoryBody, "gemini-3-flash")
	require.Equal(t, "VALIDATED", doc.Request.ToolConfig.FunctionCallingConfig.Mode)
	require.Equal(t, "get_time", declarationNameOf(t, doc))
	require.Equal(t, "real-signature-value", historicalToolSignature(t, doc))
	require.Empty(t, doc.Request.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
}

func TestOmittedToolChoicePrefixMatchesExplicitModes(t *testing.T) {
	omitted, _ := transformContract(t, toolHistoryBody, "gemini-3-flash")
	omitTools, omitSystem, omitFirst := cachePrefixOf(omitted)
	for _, choice := range []string{`{"type":"none"}`, `{"type":"auto"}`, `{"type":"any"}`, `{"type":"tool","name":"get_time"}`} {
		body := strings.Replace(toolHistoryBody, `"max_tokens":128,`, `"max_tokens":128,"tool_choice":`+choice+`,`, 1)
		doc, _ := transformContract(t, body, "gemini-3-flash")
		tools, system, first := cachePrefixOf(doc)
		require.Equal(t, omitTools, tools, choice)
		require.Equal(t, omitSystem, system, choice)
		require.Equal(t, omitFirst, first, choice)
	}
}

func TestNamedToolChoiceUsesSanitizedDeclarationName(t *testing.T) {
	body := `{
	  "model":"client-alias",
	  "max_tokens":64,
	  "tool_choice":{"type":"tool","name":"get time"},
	  "tools":[
	    {"name":"get time","input_schema":{"type":"object"}},
	    {"name":"9lives","input_schema":{"type":"object"}}
	  ],
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get time","input":{},"signature":"real-signature-value"}]}
	  ]
	}`
	doc, _ := transformContract(t, body, "gemini-3-flash")
	calling := doc.Request.ToolConfig.FunctionCallingConfig
	require.Equal(t, "ANY", calling.Mode)
	require.Equal(t, []string{"get_time"}, calling.AllowedFunctionNames)
	require.Equal(t, "get_time", doc.Request.Tools[0].FunctionDeclarations[0].Name)
	require.Equal(t, "_9lives", doc.Request.Tools[0].FunctionDeclarations[1].Name)
	require.Equal(t, "get_time", doc.Request.Contents[1].Parts[0].FunctionCall.Name)
	require.Equal(t, "real-signature-value", doc.Request.Contents[1].Parts[0].ThoughtSignature)

	nested := strings.Replace(body, `{"type":"tool","name":"get time"}`, `{"type":"function","function":{"name":"get time"}}`, 1)
	nestedDoc, _ := transformContract(t, nested, "gemini-3-flash")
	require.Equal(t, []string{"get_time"}, nestedDoc.Request.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
}

func TestClaudeRequestKeepsToolChoiceAndEffortJSON(t *testing.T) {
	var req ClaudeRequest
	require.NoError(t, json.Unmarshal([]byte(`{
	  "model":"client-alias",
	  "tool_choice":{"type":"tool","name":"get_time"},
	  "output_config":{"effort":"high"}
	}`), &req))
	require.JSONEq(t, `{"type":"tool","name":"get_time"}`, string(req.ToolChoice))
	require.NotNil(t, req.OutputConfig)
	require.Equal(t, "high", req.OutputConfig.Effort)
}

func TestUnknownExplicitModelDoesNotInheritNonReasoningStopSequences(t *testing.T) {
	body := `{"model":"client-alias","max_tokens":64,"temperature":0,"top_p":0.2,"top_k":1,"messages":[{"role":"user","content":"hi"}]}`
	doc, raw := transformContract(t, body, "gemini-explicit-custom")
	require.NotContains(t, raw, "stopSequences")
	require.Empty(t, doc.Request.GenerationConfig.StopSequences)
	require.NotNil(t, doc.Request.GenerationConfig.Temperature)
	require.Equal(t, 0.0, *doc.Request.GenerationConfig.Temperature)
	require.NotNil(t, doc.Request.GenerationConfig.TopP)
	require.Equal(t, 0.2, *doc.Request.GenerationConfig.TopP)
	require.NotNil(t, doc.Request.GenerationConfig.TopK)
	require.Equal(t, 1, *doc.Request.GenerationConfig.TopK)
	require.Nil(t, doc.Request.GenerationConfig.ThinkingConfig)
}

func TestUnknownModelWithoutEffortDoesNotInventThinkingLevel(t *testing.T) {
	body := `{
	  "model":"client-alias",
	  "max_tokens":64,
	  "thinking":{"type":"adaptive"},
	  "messages":[{"role":"user","content":"hi"}]
	}`
	doc, raw := transformContract(t, body, "gemini-explicit-custom")
	require.NotContains(t, raw, "stopSequences")
	require.NotContains(t, raw, "thinkingLevel")
	require.NotNil(t, doc.Request.GenerationConfig.ThinkingConfig)
	require.Equal(t, -1, doc.Request.GenerationConfig.ThinkingConfig.ThinkingBudget)
	require.Empty(t, doc.Request.GenerationConfig.ThinkingConfig.ThinkingLevel)
	require.Nil(t, doc.Request.GenerationConfig.Temperature)
}

func TestExplicitEffortBecomesThinkingLevelWithoutCatalog(t *testing.T) {
	body := `{
	  "model":"client-alias",
	  "max_tokens":256,
	  "output_config":{"effort":"high"},
	  "thinking":{"type":"adaptive"},
	  "messages":[{"role":"user","content":"hi"}]
	}`
	doc, raw := transformContract(t, body, "gemini-explicit-custom")
	require.NotContains(t, raw, "stopSequences")
	require.NotContains(t, raw, "thinkingBudget")
	require.Contains(t, raw, `"thinkingLevel":"high"`)
	require.Equal(t, "high", doc.Request.GenerationConfig.ThinkingConfig.ThinkingLevel)
	require.Equal(t, 0, doc.Request.GenerationConfig.ThinkingConfig.ThinkingBudget)
	require.True(t, doc.Request.GenerationConfig.ThinkingConfig.IncludeThoughts)
	require.Equal(t, 256, doc.Request.GenerationConfig.MaxOutputTokens)
}

func TestMappedEffortSuffixIsNotWrittenAgainAsThinkingLevel(t *testing.T) {
	body := `{
	  "model":"client-alias",
	  "max_tokens":128,
	  "output_config":{"effort":"high"},
	  "thinking":{"type":"adaptive"},
	  "messages":[{"role":"user","content":"hi"}]
	}`
	doc, raw := transformContract(t, body, "custom-base-high")
	require.NotContains(t, raw, "thinkingLevel")
	require.Equal(t, -1, doc.Request.GenerationConfig.ThinkingConfig.ThinkingBudget)
	require.Empty(t, doc.Request.GenerationConfig.ThinkingConfig.ThinkingLevel)
}

func TestUnknownModelDoesNotEmitBudgetAndLevelTogether(t *testing.T) {
	adaptive := `{
	  "model":"client-alias",
	  "max_tokens":128,
	  "output_config":{"effort":"High"},
	  "thinking":{"type":"adaptive","budget_tokens":4000},
	  "messages":[{"role":"user","content":"hi"}]
	}`
	doc, raw := transformContract(t, adaptive, "gemini-explicit-custom")
	require.Contains(t, raw, `"thinkingLevel":"high"`)
	require.NotContains(t, raw, "thinkingBudget")
	require.Equal(t, "high", doc.Request.GenerationConfig.ThinkingConfig.ThinkingLevel)

	enabled := `{
	  "model":"client-alias",
	  "max_tokens":100,
	  "output_config":{"effort":"high"},
	  "thinking":{"type":"enabled","budget_tokens":5000},
	  "messages":[{"role":"user","content":"hi"}]
	}`
	budgetDoc, budgetRaw := transformContract(t, enabled, "gemini-explicit-custom")
	require.NotContains(t, budgetRaw, "thinkingLevel")
	require.Equal(t, 5000, budgetDoc.Request.GenerationConfig.ThinkingConfig.ThinkingBudget)
	require.Equal(t, 6000, budgetDoc.Request.GenerationConfig.MaxOutputTokens)
}

func TestKnownModelsKeepBudgetStopsAndSampling(t *testing.T) {
	t.Run("non-reasoning gemini keeps stops and explicit temperature", func(t *testing.T) {
		body := `{
		  "model":"client-alias",
		  "max_tokens":64,
		  "temperature":0.3,
		  "output_config":{"effort":"medium"},
		  "thinking":{"type":"adaptive"},
		  "messages":[{"role":"user","content":"hi"}]
		}`
		doc, raw := transformContract(t, body, "gemini-3-flash")
		require.Equal(t, DefaultStopSequences, doc.Request.GenerationConfig.StopSequences)
		require.NotContains(t, raw, "thinkingLevel")
		require.Equal(t, -1, doc.Request.GenerationConfig.ThinkingConfig.ThinkingBudget)
		require.NotNil(t, doc.Request.GenerationConfig.Temperature)
		require.Equal(t, 0.3, *doc.Request.GenerationConfig.Temperature)
	})

	t.Run("gemini 2.5 flash keeps budget cap", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:        "gemini-2.5-flash",
			MaxTokens:    100,
			Temperature:  floatPtr(0.1),
			Thinking:     &ThinkingConfig{Type: "enabled", BudgetTokens: 30000},
			OutputConfig: &ClaudeOutputConfig{Effort: "high"},
		}
		cfg := buildGenerationConfig(req)
		require.Equal(t, DefaultStopSequences, cfg.StopSequences)
		require.Equal(t, Gemini25FlashThinkingBudgetLimit, cfg.ThinkingConfig.ThinkingBudget)
		require.Empty(t, cfg.ThinkingConfig.ThinkingLevel)
		require.Equal(t, Gemini25FlashThinkingBudgetLimit+MaxTokensBudgetPadding, cfg.MaxOutputTokens)
		require.NotNil(t, cfg.Temperature)
	})

	t.Run("known reasoning model keeps budget and drops sampling", func(t *testing.T) {
		temp := 0.4
		req := &ClaudeRequest{
			Model:        "gemini-3-pro-high",
			MaxTokens:    128,
			Temperature:  &temp,
			Thinking:     &ThinkingConfig{Type: "adaptive"},
			OutputConfig: &ClaudeOutputConfig{Effort: "high"},
		}
		cfg := buildGenerationConfig(req)
		require.Empty(t, cfg.StopSequences)
		require.Nil(t, cfg.Temperature)
		require.Equal(t, -1, cfg.ThinkingConfig.ThinkingBudget)
		require.Empty(t, cfg.ThinkingConfig.ThinkingLevel)
	})

	t.Run("opus adaptive keeps 24576 and max_tokens above budget", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:        "claude-opus-4-6",
			MaxTokens:    1000,
			Thinking:     &ThinkingConfig{Type: "adaptive", BudgetTokens: 20000},
			OutputConfig: &ClaudeOutputConfig{Effort: "max"},
		}
		cfg := buildGenerationConfig(req)
		require.Equal(t, DefaultStopSequences, cfg.StopSequences)
		require.Equal(t, ClaudeAdaptiveHighThinkingBudgetTokens, cfg.ThinkingConfig.ThinkingBudget)
		require.Empty(t, cfg.ThinkingConfig.ThinkingLevel)
		require.Equal(t, ClaudeAdaptiveHighThinkingBudgetTokens+MaxTokensBudgetPadding, cfg.MaxOutputTokens)
	})

	t.Run("claude sonnet adaptive stays on dynamic budget", func(t *testing.T) {
		req := &ClaudeRequest{
			Model:        "claude-sonnet-4-5-thinking",
			Thinking:     &ThinkingConfig{Type: "adaptive"},
			OutputConfig: &ClaudeOutputConfig{Effort: "high"},
		}
		cfg := buildGenerationConfig(req)
		require.Equal(t, -1, cfg.ThinkingConfig.ThinkingBudget)
		require.Empty(t, cfg.ThinkingConfig.ThinkingLevel)
		require.Equal(t, DefaultStopSequences, cfg.StopSequences)
	})
}

func TestReadOnlySchemaKeywordAlreadyStripped(t *testing.T) {
	cleaned := CleanJSONSchema(map[string]any{
		"type":     "object",
		"readOnly": true,
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "readOnly": true},
			"readOnly": map[string]any{
				"type":        "boolean",
				"description": "Legitimate property named readOnly",
				"readOnly":    true,
			},
		},
	})
	_, ok := cleaned["readOnly"]
	require.False(t, ok)
	props := cleaned["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	_, ok = name["readOnly"]
	require.False(t, ok)
	kept := props["readOnly"].(map[string]any)
	require.Equal(t, "boolean", kept["type"])
	require.Equal(t, "Legitimate property named readOnly", kept["description"])
	_, ok = kept["readOnly"]
	require.False(t, ok)
}

func floatPtr(v float64) *float64 { return &v }
