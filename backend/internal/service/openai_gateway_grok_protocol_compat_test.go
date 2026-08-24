//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type grokCompatibilityStaticFixtures struct {
	Model                string                               `json:"model"`
	Controls             []json.RawMessage                    `json:"controls"`
	AgentMessageVariants []grokCompatibilityAgentFixture      `json:"agent_message_variants"`
	ToolModes            []grokCompatibilityToolModeFixture   `json:"tool_modes"`
	SchemaCases          []grokCompatibilitySchemaCaseFixture `json:"schema_cases"`
}

type grokCompatibilityAgentFixture struct {
	Name string          `json:"name"`
	Item json.RawMessage `json:"item"`
}

type grokCompatibilityToolModeFixture struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

type grokCompatibilitySchemaCaseFixture struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
}

func TestNormalizeGrokResponsesProtocolCompatibilityStaticAgentMatrix(t *testing.T) {
	fixtures := loadGrokCompatibilityStaticFixtures(t)
	cases := 0
	for _, messageFixture := range fixtures.AgentMessageVariants {
		for _, toolMode := range fixtures.ToolModes {
			t.Run(messageFixture.Name+"/"+toolMode.Name, func(t *testing.T) {
				input := append([]json.RawMessage(nil), fixtures.Controls...)
				input = append(input, messageFixture.Item)
				body := map[string]any{"model": fixtures.Model, "input": input}
				if toolMode.Name != "absent" {
					var value any
					decodeGrokCompatibilityJSON(t, toolMode.Value, &value)
					body["tools"] = value
				}
				raw := marshalGrokCompatibilityJSON(t, body)
				patched, _, err := patchGrokResponsesBodyWithClientTools(raw, fixtures.Model)
				require.NoError(t, err)
				normalized, report, err := normalizeGrokResponsesProtocolCompatibility(patched)
				require.NoError(t, err)
				require.Equal(t, 1, report.AgentItems)
				assertGrokCompatibilityAgentProjection(t, messageFixture.Item, gjson.GetBytes(normalized, "input.4"))
				require.False(t, bytes.Contains(normalized, []byte(`"type":"agent_message"`)))

				built := buildGrokCompatibilityRequest(t, normalized)
				require.Equal(t, normalized, built)
				second, secondReport, err := normalizeGrokResponsesProtocolCompatibility(normalized)
				require.NoError(t, err)
				require.Equal(t, normalized, second)
				require.Zero(t, secondReport.AgentItems)
			})
			cases++
		}
	}
	require.Equal(t, 28, cases)
}

func TestNormalizeGrokResponsesProtocolCompatibilityStaticSchemaControls(t *testing.T) {
	fixtures := loadGrokCompatibilityStaticFixtures(t)
	for _, schemaFixture := range fixtures.SchemaCases {
		t.Run(schemaFixture.Name, func(t *testing.T) {
			body := []byte(`{"model":"grok-4.5","input":[],"tools":[{"type":"function","name":"schema_control","strict":true,"parameters":` + string(schemaFixture.Parameters) + `}]}`)
			patched, _, report, err := patchGrokResponsesBodyWithClientToolsCompatibility(body, fixtures.Model, true)
			require.NoError(t, err)
			require.NotEmpty(t, report.Schemas)
			parameters := gjson.GetBytes(patched, "tools.0.parameters")
			require.Equal(t, "object", parameters.Get("type").String())
			assertNoGrokRootRefOrNull(t, parameters.Value())
			require.Equal(t, patched, buildGrokCompatibilityRequest(t, patched))
		})
	}
}

func TestNormalizeGrokResponsesProtocolCompatibilityAgentExactPreservationAndValidation(t *testing.T) {
	item := json.RawMessage(`{"type":"agent_message","author":"/root/\"child","recipient":"/root\\main","id":"private","internal_chat_message_metadata_passthrough":{"secret":"do-not-copy"},"content":[{"type":"input_text","text":""},{"type":"encrypted_content","encrypted_content":"opaque\n\u4e2d\u6587"},{"type":"input_text","text":"last"}]}`)
	body := []byte(`{"model":"grok-4.5","input":[` + string(item) + `],"sentinel":900719925474099312345}`)
	normalized, report, err := normalizeGrokResponsesProtocolCompatibility(body)
	require.NoError(t, err)
	require.Equal(t, 1, report.AgentMixed)
	require.Equal(t, 3, report.AgentParts)
	assertGrokCompatibilityAgentProjection(t, item, gjson.GetBytes(normalized, "input.0"))
	require.Equal(t, "900719925474099312345", gjson.GetBytes(normalized, "sentinel").Raw)
	require.False(t, gjson.GetBytes(normalized, "input.0.id").Exists())
	require.False(t, gjson.GetBytes(normalized, "input.0.internal_chat_message_metadata_passthrough").Exists())

	invalidCases := []struct {
		name string
		item string
		path string
	}{
		{"missing author", `{"type":"agent_message","recipient":"/root","content":[]}`, "input[0].author"},
		{"recipient type", `{"type":"agent_message","author":"/root","recipient":1,"content":[]}`, "input[0].recipient"},
		{"content type", `{"type":"agent_message","author":"/root","recipient":"/root","content":{}}`, "input[0].content"},
		{"unknown part", `{"type":"agent_message","author":"/root","recipient":"/root","content":[{"type":"output_text","text":"x"}]}`, "input[0].content[0].type"},
		{"missing tagged string", `{"type":"agent_message","author":"/root","recipient":"/root","content":[{"type":"encrypted_content"}]}`, "input[0].content[0].encrypted_content"},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte(`{"input":[` + testCase.item + `]}`)
			result, _, err := normalizeGrokResponsesProtocolCompatibility(raw)
			require.Nil(t, result)
			var compatibilityErr *GrokResponsesCompatibilityError
			require.ErrorAs(t, err, &compatibilityErr)
			require.Equal(t, "invalid_agent_message", compatibilityErr.Code)
			require.Equal(t, testCase.path, compatibilityErr.Path)
			require.Equal(t, `{"input":[`+testCase.item+`]}`, string(raw))
		})
	}
}

func TestNormalizeGrokResponsesProtocolCompatibilityKeepsMultipleAgentPositionsAndEmptyContent(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[]},{"type":"agent_message","author":"/root/a","recipient":"/root","content":[]},{"type":"reasoning","summary":[]},{"type":"agent_message","author":"/root/b","recipient":"/root","content":[{"type":"input_text","text":"second"}]}]}`)
	normalized, report, err := normalizeGrokResponsesProtocolCompatibility(body)
	require.NoError(t, err)
	require.Equal(t, 2, report.AgentItems)
	require.Equal(t, 1, report.AgentEmpty)
	require.Equal(t, 1, report.AgentPlaintext)
	require.Len(t, gjson.GetBytes(normalized, "input").Array(), 4)
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.0.type").String())
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.1.type").String())
	require.Len(t, gjson.GetBytes(normalized, "input.1.content").Array(), 1)
	require.Equal(t, "reasoning", gjson.GetBytes(normalized, "input.2.type").String())
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.3.type").String())
	require.Equal(t, "second", gjson.GetBytes(normalized, "input.3.content.1.text").String())
}

func TestNormalizeGrokResponsesProtocolCompatibilitySchemaMatrix(t *testing.T) {
	pureObject := []byte("{\n \"type\": \"object\", \"properties\": {\"x\": {\"type\": \"string\"}}, \"required\": [\"x\"], \"additionalProperties\": false\n}")
	pureBody := []byte(`{"tools":[{"type":"function","name":"pure","strict":true,"parameters":` + string(pureObject) + `}]}`)
	pureNormalized, pureReport, err := sanitizeGrokResponsesTools(pureBody, true)
	require.NoError(t, err)
	require.Equal(t, pureBody, pureNormalized)
	require.Equal(t, 1, pureReport.SchemaUnchanged)

	tests := []struct {
		name            string
		schema          string
		wantOutcome     string
		wantReason      string
		wantStrict      bool
		wantBranchCount int
	}{
		{"root ref chain", `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"type":"object","properties":{"x":{"type":["string","null"]}},"required":["x"],"additionalProperties":false}},"$ref":"#/$defs/a"}`, "inlined", "local_root_ref", true, 0},
		{"root ref assertion sibling", `{"$defs":{"a":{"type":"object","properties":{"x":{"type":"string"}}}},"$ref":"#/$defs/a","required":["x"],"description":"kept"}`, "inlined", "local_root_ref", true, 0},
		{"rfc6901 ref", `{"$defs":{"a/b~c":{"type":"object","properties":{},"additionalProperties":false}},"$ref":"#/$defs/a~1b~0c"}`, "inlined", "local_root_ref", true, 0},
		{"object oneof", `{"oneOf":[{"type":"object","properties":{"mode":{"const":"a"}},"required":["mode"],"additionalProperties":false},{"type":"object","properties":{"mode":{"const":"b"}},"required":["mode"],"additionalProperties":false}]}`, "object_union", "object_variants_preserved", true, 2},
		{"object anyof", `{"anyOf":[{"type":"object","required":["a"]},{"type":"object","required":["b"]}]}`, "object_union", "object_variants_preserved", true, 2},
		{"object null", `{"oneOf":[{"type":"object","properties":{"nested":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["nested"],"additionalProperties":false},{"type":"null"}]}`, "null_pruned", "root_null_unreachable", true, 0},
		{"object type null", `{"type":["object","null"],"properties":{"nested":{"type":["string","null"]}},"required":["nested"],"additionalProperties":false}`, "null_pruned", "root_null_unreachable", true, 0},
		{"cross constraint object only", `{"type":["object","string"],"allOf":[{"type":"object","required":["x"]}],"properties":{"x":{"type":"string"}},"additionalProperties":false}`, "canonicalized", "implicit_object_domain", true, 0},
		{"mixed nonobject", `{"anyOf":[{"type":"object","required":["x"]},{"type":"string"},{"type":"null"}]}`, "fallback", "mixed_non_object_root", false, 0},
		{"string root", `{"type":"string"}`, "fallback", "non_object_root", false, 0},
		{"array root", `{"type":"array","items":{"type":"string"}}`, "fallback", "non_object_root", false, 0},
		{"number root", `{"type":"number"}`, "fallback", "non_object_root", false, 0},
		{"boolean root", `{"type":"boolean"}`, "fallback", "non_object_root", false, 0},
		{"null root", `{"type":"null"}`, "fallback", "null_only_root", false, 0},
		{"scalar const", `{"const":"scalar"}`, "fallback", "non_object_root", false, 0},
		{"object enum null", `{"enum":[{"mode":"a"},null]}`, "null_pruned", "root_null_unreachable", true, 0},
		{"implicit object", `{"properties":{"x":{"type":"string"}},"required":["x"]}`, "canonicalized", "implicit_object_domain", true, 0},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"tools":[{"type":"function","name":"schema_a","strict":true,"parameters":` + testCase.schema + `}]}`)
			normalized, report, err := sanitizeGrokResponsesTools(body, true)
			require.NoError(t, err)
			require.Len(t, report.Schemas, 1)
			require.Equal(t, testCase.wantOutcome, report.Schemas[0].Outcome)
			require.Equal(t, testCase.wantReason, report.Schemas[0].Reason)
			require.Equal(t, testCase.wantStrict, gjson.GetBytes(normalized, "tools.0.strict").Bool())
			parameters := gjson.GetBytes(normalized, "tools.0.parameters")
			require.Equal(t, "object", parameters.Get("type").String())
			assertNoGrokRootRefOrNull(t, parameters.Value())
			if testCase.wantBranchCount > 0 {
				branches := parameters.Get("oneOf")
				if !branches.IsArray() {
					branches = parameters.Get("anyOf")
				}
				require.Len(t, branches.Array(), testCase.wantBranchCount)
			}
			second, secondReport, err := sanitizeGrokResponsesTools(normalized, true)
			require.NoError(t, err)
			require.Equal(t, normalized, second)
			require.NotEqual(t, "fallback", compatibilitySingleSchemaOutcome(secondReport))
		})
	}

	first := []byte(`{"tools":[{"type":"function","name":"first","strict":true,"parameters":{"type":"string"}}]}`)
	second := []byte(`{"tools":[{"type":"function","name":"unrelated_name","strict":true,"parameters":{"type":"string"}}]}`)
	firstNormalized, _, err := sanitizeGrokResponsesTools(first, true)
	require.NoError(t, err)
	secondNormalized, _, err := sanitizeGrokResponsesTools(second, true)
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(firstNormalized, "tools.0.parameters").Raw, gjson.GetBytes(secondNormalized, "tools.0.parameters").Raw)

	for _, schema := range []string{`{}`, `true`} {
		body := []byte(`{"tools":[{"type":"function","name":"canonical","strict":true,"parameters":` + schema + `}]}`)
		normalized, report, err := sanitizeGrokResponsesTools(body, true)
		require.NoError(t, err)
		require.Equal(t, "canonicalized", compatibilitySingleSchemaOutcome(report))
		require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(normalized, "tools.0.parameters").Raw)
		require.True(t, gjson.GetBytes(normalized, "tools.0.strict").Bool())
	}
}

func TestNormalizeGrokResponsesProtocolCompatibilityCanonicalizesCrossConstraintObjectDomain(t *testing.T) {
	schema := `{"type":["object","string"],"allOf":[{"type":"object","required":["x"]}],"properties":{"x":{"type":"string","anyOf":[{"type":"string"},{"type":"null"}]}},"additionalProperties":false}`
	body := []byte(`{"tools":[{"type":"function","name":"cross_constraint","strict":true,"parameters":` + schema + `}]}`)
	normalized, report, err := sanitizeGrokResponsesTools(body, true)
	require.NoError(t, err)
	require.Equal(t, "canonicalized", compatibilitySingleSchemaOutcome(report))
	parameters := gjson.GetBytes(normalized, "tools.0.parameters")
	require.Equal(t, "object", parameters.Get("type").String())
	require.True(t, parameters.Get("allOf").IsArray())
	require.Equal(t, "x", parameters.Get("allOf.0.required.0").String())
	require.Equal(t, "null", parameters.Get("properties.x.anyOf.1.type").String(), "nested nullable constraint must not be pruned")
	require.False(t, parameters.Get("additionalProperties").Bool())
}

func TestGrokResponsesSchemaCompatibilityFallsBackPerToolAndKeepsAgentBoundary(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		reason string
	}{
		{"missing", `{"$ref":"#/$defs/missing","$defs":{}}`, "missing_ref"},
		{"cycle", `{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}}}`, "cyclic_ref"},
		{"external", `{"$ref":"https://example.invalid/schema"}`, "external_ref_unsupported"},
		{"dynamic", `{"$dynamicRef":"#node"}`, "dynamic_ref_unsupported"},
		{"scope", `{"$id":"https://example.invalid/schema","$ref":"#/$defs/value","$defs":{"value":{"type":"object"}}}`, "schema_scope_unsupported"},
		{"false", `false`, "false_schema"},
		{"malformed combinator", `{"oneOf":[]}`, "malformed_combinator"},
		{"unsatisfiable", `{"type":"object","const":"not-an-object"}`, "unsatisfiable_root"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"agent_message","author":"/root/a","recipient":"/root","content":[{"type":"input_text","text":"must-not-leak"}]}],"tools":[{"type":"function","name":"good","description":"keep-good","strict":true,"parameters":{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false},"x_outer":1},{"type":"function","name":"broken","description":"keep-bad","strict":true,"parameters":` + testCase.schema + `,"x_outer":2},{"type":"web_search","x_outer":3},{"type":"function","name":"after","strict":true,"parameters":{"type":"object","properties":{}}}]}`)
			original := append([]byte(nil), body...)
			normalized, report, err := sanitizeGrokResponsesTools(body, true)
			require.NoError(t, err)
			require.Equal(t, original, body)
			require.Equal(t, "agent_message", gjson.GetBytes(normalized, "input.0.type").String(), "shared schema stage must not project agent items")
			require.Len(t, gjson.GetBytes(normalized, "tools").Array(), 4)
			require.Equal(t, "good", gjson.GetBytes(normalized, "tools.0.name").String())
			require.Equal(t, "broken", gjson.GetBytes(normalized, "tools.1.name").String())
			require.Equal(t, "web_search", gjson.GetBytes(normalized, "tools.2.type").String())
			require.Equal(t, "after", gjson.GetBytes(normalized, "tools.3.name").String())
			require.Equal(t, "keep-bad", gjson.GetBytes(normalized, "tools.1.description").String())
			require.Equal(t, int64(2), gjson.GetBytes(normalized, "tools.1.x_outer").Int())
			require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(normalized, "tools.1.parameters").Raw)
			require.False(t, gjson.GetBytes(normalized, "tools.1.strict").Bool())
			require.Equal(t, "x", gjson.GetBytes(normalized, "tools.0.parameters.required.0").String())
			require.True(t, gjson.GetBytes(normalized, "tools.3.strict").Bool(), "later valid tool must still be processed independently")
			require.Equal(t, 1, report.SchemaFallback)
			require.Zero(t, report.SchemaErrors)
			require.Len(t, report.Schemas, 3)
			require.Equal(t, testCase.reason, report.Schemas[1].Reason)
			require.NotContains(t, report.Schemas[1].Fingerprint, "must-not-leak")

			projected, agentReport, err := normalizeGrokResponsesProtocolCompatibility(normalized)
			require.NoError(t, err)
			require.Equal(t, 1, agentReport.AgentItems)
			require.Empty(t, agentReport.Schemas)
			require.Equal(t, "message", gjson.GetBytes(projected, "input.0.type").String())
		})
	}
}

func TestNormalizeGrokResponsesProtocolCompatibilitySchemaBudgets(t *testing.T) {
	high := grokSchemaCompatibilityLimits{
		maxInputBytes: 1 << 20, maxDepth: 100, maxWork: 10000, maxRefs: 10000,
		maxProducedNodes: 100000, maxProducedBytes: 1 << 20, maxOutputBytes: 1 << 20,
	}
	tests := []struct {
		name   string
		schema string
		limits func() grokSchemaCompatibilityLimits
		reason string
	}{
		{
			name: "input size", schema: `{"type":"object","description":"long"}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxInputBytes = 8; return limits },
			reason: "compatibility_input_size_limit_exceeded",
		},
		{
			name: "depth", schema: `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/c"},"c":{"type":"object"}},"$ref":"#/$defs/a"}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxDepth = 2; return limits },
			reason: "compatibility_depth_limit_exceeded",
		},
		{
			name: "resolved refs", schema: grokCompatibilityDoublingDAGSchema(t, 4),
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxRefs = 3; return limits },
			reason: "compatibility_ref_limit_exceeded",
		},
		{
			name: "visited work", schema: `{"allOf":[{"type":"object"},{"type":"object"},{"type":"object"}]}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxWork = 2; return limits },
			reason: "compatibility_work_limit_exceeded",
		},
		{
			name: "produced nodes", schema: `{"allOf":[{"type":"object"},{"type":"object"}]}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxProducedNodes = 2; return limits },
			reason: "compatibility_node_limit_exceeded",
		},
		{
			name: "produced size", schema: `{"$defs":{"a":{"type":"object","description":"abcdefghijklmnopqrstuvwxyz"}},"$ref":"#/$defs/a"}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxProducedBytes = 32; return limits },
			reason: "compatibility_produced_size_limit_exceeded",
		},
		{
			name: "output size", schema: `{"properties":{"x":{"type":"string"}},"description":"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"}`,
			limits: func() grokSchemaCompatibilityLimits { limits := high; limits.maxOutputBytes = 48; return limits },
			reason: "compatibility_output_size_limit_exceeded",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			result, changed, outcome, reason, err := normalizeGrokFunctionParametersWithLimits([]byte(testCase.schema), "tools[2].parameters", testCase.limits())
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, "fallback", outcome)
			require.Equal(t, testCase.reason, reason)
			require.JSONEq(t, string(permissiveGrokObjectSchema()), string(result))
		})
	}

	adversarial := grokCompatibilityDoublingDAGSchema(t, 10)
	require.Less(t, len(adversarial), 4096, "adversarial input must stay small")
	result, _, outcome, reason, err := normalizeGrokFunctionParameters([]byte(adversarial), "tools[0].parameters")
	require.NoError(t, err)
	require.Equal(t, "fallback", outcome)
	require.Equal(t, "compatibility_ref_limit_exceeded", reason)
	require.JSONEq(t, string(permissiveGrokObjectSchema()), string(result))

	result, _, outcome, reason, err = normalizeGrokFunctionParameters([]byte(`{"type":"object"} trailing`), "tools[0].parameters")
	require.NoError(t, err)
	require.Equal(t, "fallback", outcome)
	require.Equal(t, "malformed_schema", reason)
	require.JSONEq(t, string(permissiveGrokObjectSchema()), string(result))
}

func TestNormalizeGrokResponsesProtocolCompatibilityAutomationUpdateSemantics(t *testing.T) {
	rawSchema, err := os.ReadFile(filepath.Join("testdata", "grok_automation_update_parameters.json"))
	require.NoError(t, err)
	body := []byte(`{"tools":[{"type":"function","name":"automation_update","strict":true,"parameters":` + string(rawSchema) + `}]}`)
	normalizedBody, report, err := sanitizeGrokResponsesTools(body, true)
	require.NoError(t, err)
	require.Len(t, report.Schemas, 1)
	require.Equal(t, "inlined", report.Schemas[0].Outcome)
	require.NotZero(t, report.Schemas[0].SizeDeltaByte)
	normalizedRaw := []byte(gjson.GetBytes(normalizedBody, "tools.0.parameters").Raw)
	secondBody, secondReport, err := sanitizeGrokResponsesTools(normalizedBody, true)
	require.NoError(t, err)
	require.Equal(t, normalizedBody, secondBody)
	require.Equal(t, "unchanged", compatibilitySingleSchemaOutcome(secondReport))

	var originalSchema map[string]any
	var normalizedSchema map[string]any
	decodeGrokCompatibilityJSON(t, rawSchema, &originalSchema)
	decodeGrokCompatibilityJSON(t, normalizedRaw, &normalizedSchema)
	require.Equal(t, "object", normalizedSchema["type"])
	branches, ok := normalizedSchema["oneOf"].([]any)
	require.True(t, ok)
	require.Len(t, branches, 4)
	assertNoGrokRootRefOrNull(t, normalizedSchema)

	cases := []struct {
		name     string
		instance string
		valid    bool
	}{
		{"view", `{"mode":"view","id":"automation-id"}`, true},
		{"delete", `{"mode":"delete","id":"automation-id"}`, true},
		{"create cron nullable", `{"mode":"create","name":"daily","prompt":"run","rrule":"FREQ=DAILY","status":"ACTIVE","kind":"cron","projectId":null,"model":"gpt-5","reasoningEffort":"high","executionEnvironment":"local","notificationPolicy":null}`, true},
		{"create heartbeat", `{"mode":"suggested_create","name":"heartbeat","prompt":"check","rrule":"FREQ=HOURLY","status":"PAUSED","kind":"heartbeat"}`, true},
		{"update cron", `{"mode":"update","id":"automation-id","name":"daily","prompt":"run","rrule":"FREQ=DAILY","status":"ACTIVE","kind":"cron","projectId":"project","model":"gpt-5","reasoningEffort":"medium","executionEnvironment":"worktree","localEnvironmentConfigPath":null}`, true},
		{"missing required", `{"mode":"view"}`, false},
		{"wrong discriminator", `{"mode":"unknown","id":"automation-id"}`, false},
		{"extra property", `{"mode":"delete","id":"automation-id","extra":true}`, false},
		{"nested nullable wrong type", `{"mode":"create","name":"daily","prompt":"run","rrule":"FREQ=DAILY","status":"ACTIVE","kind":"cron","projectId":7,"model":"gpt-5","reasoningEffort":"high","executionEnvironment":"local"}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var instance any
			decodeGrokCompatibilityJSON(t, []byte(testCase.instance), &instance)
			before := evaluateGrokCompatibilitySchema(originalSchema, originalSchema, instance)
			after := evaluateGrokCompatibilitySchema(normalizedSchema, normalizedSchema, instance)
			require.Equal(t, testCase.valid, before, "independent evaluator fixture verdict")
			require.Equal(t, before, after, "normalization must preserve object-domain verdict")
		})
	}

	originalDefs := originalSchema["$defs"].(map[string]any)
	normalizedDefs := normalizedSchema["$defs"].(map[string]any)
	require.Equal(t, originalDefs["__schema9"], normalizedDefs["__schema9"], "nested notificationPolicy nullable ref must be untouched")
	require.Equal(t, originalDefs["__schema12"], normalizedDefs["__schema12"], "nested projectId nullable ref must be untouched")
	require.Equal(t, originalDefs["__schema21"], normalizedDefs["__schema21"], "nested update oneOf must be untouched")
}

func TestGrokResponsesProtocolCompatibilityKillSwitchAndCacheProjection(t *testing.T) {
	enabled := &Account{Platform: PlatformGrok}
	disabled := &Account{Platform: PlatformGrok, Extra: map[string]any{grokResponsesProtocolCompatibilityExtraKey: false}}
	require.True(t, isGrokResponsesProtocolCompatibilityEnabled(enabled))
	require.False(t, isGrokResponsesProtocolCompatibilityEnabled(disabled))
	require.False(t, isGrokResponsesProtocolCompatibilityEnabled(&Account{Platform: PlatformOpenAI}))

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 42})
	bodyA := []byte(`{"model":"grok-4.5","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"string"}}],"input":[{"type":"agent_message","author":"/root/a","recipient":"/root","content":[{"type":"input_text","text":"one"}]}]}`)
	bodyB := []byte(`{"model":"grok-4.5","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object","properties":{"x":{"type":"string"}}}}],"input":[{"type":"agent_message","author":"/root/b","recipient":"/root","content":[{"type":"input_text","text":"two"}]}]}`)
	normalizedA, _, err := patchGrokResponsesBodyWithCompatibility(bodyA, "grok-4.5", true)
	require.NoError(t, err)
	normalizedA, _, err = normalizeGrokResponsesProtocolCompatibility(normalizedA)
	require.NoError(t, err)
	normalizedB, _, err := patchGrokResponsesBodyWithCompatibility(bodyB, "grok-4.5", true)
	require.NoError(t, err)
	normalizedB, _, err = normalizeGrokResponsesProtocolCompatibility(normalizedB)
	require.NoError(t, err)
	identityA := resolveGrokCacheIdentity(c, normalizedA, "", "grok-4.5")
	identityB := resolveGrokCacheIdentity(c, normalizedB, "", "grok-4.5")
	require.NotEmpty(t, identityA)
	require.NotEmpty(t, identityB)
	require.NotEqual(t, identityA, identityB)
}

func TestResolveGrokWSCacheIdentityUsesNormalizedEffectiveSeed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 43})
	account := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}
	seed := []byte(`{"type":"response.create","generate":true,"model":"grok","stream":true,"input":[{"type":"agent_message","author":"/root/a","recipient":"/root","content":[{"type":"input_text","text":"seed"}]}],"tools":[{"type":"function","name":"seed_tool","strict":true,"parameters":{"anyOf":[{"type":"object"},{"type":"string"}]}}]}`)

	actual, err := resolveGrokWSCacheIdentity(c, account, seed, seed, "grok")
	require.NoError(t, err)
	require.NotEmpty(t, actual)
	effective, err := prepareOpenAIWSHTTPBridgeBody(seed)
	require.NoError(t, err)
	preCompatibility, _, _, err := patchGrokResponsesBodyWithClientToolsCompatibility(effective, "grok-4.5", false)
	require.NoError(t, err)
	preCompatibilityIdentity := resolveGrokCacheIdentity(c, preCompatibility, "", "grok-4.5")
	effective, _, _, err = patchGrokResponsesBodyWithClientToolsCompatibility(effective, "grok-4.5", true)
	require.NoError(t, err)
	effective, _, err = normalizeGrokResponsesProtocolCompatibility(effective)
	require.NoError(t, err)
	want := resolveGrokCacheIdentity(c, effective, "", "grok-4.5")
	require.Equal(t, want, actual)
	require.NotEqual(t, preCompatibilityIdentity, actual)

	account.Extra = map[string]any{grokResponsesProtocolCompatibilityExtraKey: false}
	disabled, err := resolveGrokWSCacheIdentity(c, account, seed, seed, "grok")
	require.NoError(t, err)
	require.Equal(t, preCompatibilityIdentity, disabled)
}

func TestForwardGrokResponsesProtocolCompatibilityHTTPAuthParity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		account func(t *testing.T) (*Account, *GrokTokenProvider, AccountRepository)
	}{
		{
			name: "api key",
			account: func(t *testing.T) (*Account, *GrokTokenProvider, AccountRepository) {
				return &Account{ID: 801, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://grok.example.test/v1"}}, nil, nil
			},
		},
		{
			name: "oauth",
			account: func(t *testing.T) (*Account, *GrokTokenProvider, AccountRepository) {
				account := healthyGrokOAuthGatewayTestAccount(802, "access-token")
				repo := &grokQuotaAccountRepo{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}}
				return account, NewGrokTokenProvider(repo, nil), repo
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"model":"grok","stream":true,"input":[{"type":"agent_message","author":"/root/child","recipient":"/root","content":[{"type":"encrypted_content","encrypted_content":"known parsed message"}]}],"tools":[{"type":"function","name":"mixed_schema","strict":true,"parameters":{"anyOf":[{"type":"object","required":["x"]},{"type":"string"}]}}]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Set("api_key", &APIKey{ID: 987})
			account, tokenProvider, accountRepo := testCase.account(t)
			upstream := &httpUpstreamRecorder{resp: grokCompatibilityStreamingResponse("resp_http_compat")}
			svc := &OpenAIGatewayService{httpUpstream: upstream, grokTokenProvider: tokenProvider, accountRepo: accountRepo}

			result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", true, time.Now())
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
			require.Equal(t, "user", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
			require.Equal(t, "known parsed message", gjson.GetBytes(upstream.lastBody, "input.0.content.1.text").String())
			require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.type").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
			patched, _, err := patchGrokResponsesBodyWithClientTools(body, "grok-4.5")
			require.NoError(t, err)
			normalized, _, err := normalizeGrokResponsesProtocolCompatibility(patched)
			require.NoError(t, err)
			wantIdentity := resolveGrokCacheIdentity(c, normalized, "", "grok-4.5")
			require.NotEmpty(t, wantIdentity)
			require.Equal(t, wantIdentity, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
			require.Equal(t, wantIdentity, upstream.lastReq.Header.Get(grokConversationIDHeader))
		})
	}
}

func TestProxyOpenAIWSHTTPBridgeTurnProtocolCompatibilityParity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: grokCompatibilityStreamingResponse("resp_ws_compat")}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
				httpUpstream: upstream,
			}
			credentials := map[string]any{"base_url": xai.DefaultCLIBaseURL}
			if accountType == AccountTypeAPIKey {
				credentials = map[string]any{"base_url": "https://grok.example.test/v1", "api_key": "test-key"}
			}
			account := &Account{ID: 900, Platform: PlatformGrok, Type: accountType, Concurrency: 1, Credentials: credentials}
			payload := []byte(`{"type":"response.create","generate":true,"model":"grok","stream":true,"input":[{"type":"agent_message","author":"/root/child","recipient":"/root","content":[{"type":"input_text","text":"ws message"}]}],"tools":[{"type":"function","name":"nullable","strict":true,"parameters":{"oneOf":[{"type":"object","properties":{"x":{"type":["string","null"]}}},{"type":"null"}]}}]}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			var events [][]byte

			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
				context.Background(), c, account, "transport-token", payload, len(payload),
				"grok", "", "", "", "ws-cache-identity", 1,
				func(message []byte) error {
					events = append(events, append([]byte(nil), message...))
					return nil
				},
			)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, events, 2)
			require.Equal(t, "message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
			require.Equal(t, "ws message", gjson.GetBytes(upstream.lastBody, "input.0.content.1.text").String())
			require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.type").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.parameters.oneOf").Exists())
			require.Equal(t, "ws-cache-identity", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
		})
	}
}

func TestProxyOpenAIWSHTTPBridgeTurnCompatibilityErrorIsLocalClientError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	before := snapshotGrokResponsesCompatibilityMetrics()
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream}
	account := &Account{
		ID: 901, Platform: PlatformGrok, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"base_url": "https://grok.example.test/v1", "api_key": "test-key"},
	}
	payload := []byte(`{"type":"response.create","generate":true,"model":"grok","stream":true,"input":[{"type":"agent_message","author":"/root","recipient":"/root","content":[{"type":"unknown","text":"secret-ws-payload"}]}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	var events [][]byte

	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		context.Background(), c, account, "transport-token", payload, len(payload),
		"grok", "", "", "", "", 1,
		func(message []byte) error {
			events = append(events, append([]byte(nil), message...))
			return nil
		},
	)
	require.Nil(t, result)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	require.Contains(t, closeErr.Reason(), "input[0].content[0].type")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Empty(t, upstream.requests)
	require.Equal(t, StatusActive, account.Status)
	require.True(t, account.Schedulable)
	require.Len(t, events, 1)
	require.Equal(t, "error", gjson.GetBytes(events[0], "type").String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(events[0], "error.type").String())
	require.Equal(t, "input[0].content[0].type", gjson.GetBytes(events[0], "error.param").String())
	require.NotContains(t, string(events[0]), "secret-ws-payload")
	after := snapshotGrokResponsesCompatibilityMetrics()
	require.Equal(t, before.CompatibilityTotal["agent:error"]+1, after.CompatibilityTotal["agent:error"], "effective-body compatibility error must be observed exactly once")
}

func TestResolveGrokWSCacheIdentitySchemaFallbackIsStableAndNotDoubleObserved(t *testing.T) {
	gin.SetMode(gin.TestMode)
	before := snapshotGrokResponsesCompatibilityMetrics()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 44})
	account := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}
	seed := []byte(`{"type":"response.create","generate":true,"model":"grok","input":[],"tools":[{"type":"function","name":"broken","parameters":{"$ref":"#/$defs/missing","$defs":{"secret-schema":"must-not-leak"}}}]}`)

	identity, err := resolveGrokWSCacheIdentity(c, account, seed, seed, "grok")
	require.NoError(t, err)
	require.NotEmpty(t, identity)
	secondIdentity, err := resolveGrokWSCacheIdentity(c, account, seed, seed, "grok")
	require.NoError(t, err)
	require.Equal(t, identity, secondIdentity)
	afterResolve := snapshotGrokResponsesCompatibilityMetrics()
	require.Equal(t, before.CompatibilityTotal["schema:fallback"], afterResolve.CompatibilityTotal["schema:fallback"], "successful first-identity projection must not observe schema outcomes")
	require.Equal(t, before.FallbackTotal["missing_ref"], afterResolve.FallbackTotal["missing_ref"])

	prepared, err := prepareOpenAIWSHTTPBridgeBody(seed)
	require.NoError(t, err)
	patched, _, report, err := patchGrokResponsesBodyWithClientToolsCompatibility(prepared, "grok-4.5", true)
	require.NoError(t, err)
	require.Equal(t, 1, report.SchemaFallback)
	require.Equal(t, "missing_ref", report.Schemas[0].Reason)
	require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(patched, "tools.0.parameters").Raw)
	require.False(t, bytes.Contains(patched, []byte("must-not-leak")), "fallback body must remove the incompatible schema payload")
}

func TestForwardGrokResponsesProtocolCompatibilityRejectsBeforeEgress(t *testing.T) {
	body := []byte(`{"model":"grok","input":[{"type":"agent_message","author":"/root","recipient":"/root","content":[{"type":"unknown","text":"private"}]}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{ID: 803, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://grok.example.test/v1"}}

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", false, time.Now())
	require.Nil(t, result)
	require.Error(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "input[0].content[0].type", gjson.Get(recorder.Body.String(), "error.param").String())
	require.NotContains(t, recorder.Body.String(), "private")
}

func TestForwardGrokResponsesProtocolCompatibilitySchemaFallbackReachesEgress(t *testing.T) {
	body := []byte(`{"model":"grok","input":[],"tools":[{"type":"web_search"},{"type":"x_search"},{"type":"function","name":"broken","parameters":{"$ref":"#/$defs/missing","$defs":{}}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: grokCompatibilityStreamingResponse("resp_schema_fallback")}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{ID: 805, Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://grok.example.test/v1"}}

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.JSONEq(t, string(permissiveGrokObjectSchema()), gjson.GetBytes(upstream.lastBody, "tools.2.parameters").Raw)
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools.2.strict").Bool())
	require.NotContains(t, string(upstream.lastBody), string(gjson.GetBytes(body, "tools.2.parameters").Raw))
}

func TestForwardGrokResponsesProtocolCompatibilityKillSwitchSkipsOnlyNewStage(t *testing.T) {
	body := []byte(`{"model":"grok","stream":true,"input":[{"type":"agent_message","author":"/root","recipient":"/root","content":[{"type":"input_text","text":"rollback"}]}],"tools":[{"type":"function","name":"rollback_tool","strict":true,"parameters":{"$ref":"#/$defs/missing","$defs":{}}}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: grokCompatibilityStreamingResponse("resp_kill_switch")}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 804, Platform: PlatformGrok, Type: AccountTypeAPIKey, Concurrency: 1,
		Extra:       map[string]any{grokResponsesProtocolCompatibilityExtraKey: false},
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://grok.example.test/v1"},
	}

	result, err := svc.forwardGrokResponses(context.Background(), c, account, body, "grok", true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "agent_message", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
	require.Equal(t, "#/$defs/missing", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.$ref").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
	require.Equal(t, "grok-4.5", gjson.GetBytes(upstream.lastBody, "model").String(), "existing Grok base patch remains active")
}

func TestObserveGrokResponsesProtocolCompatibilityIsStructuredAndPayloadFree(t *testing.T) {
	body := []byte(`{"authorization":"secret-auth-value","input":[{"type":"agent_message","author":"secret-author","recipient":"secret-recipient","content":[{"type":"encrypted_content","encrypted_content":"secret-message"}]}],"tools":[{"type":"function","name":"schema_tool","description":"secret-description","strict":true,"parameters":{"anyOf":[{"type":"object"},{"type":"string"}],"title":"secret-schema-title"}}]}`)
	schemaBody, schemaReport, err := sanitizeGrokResponsesTools(body, true)
	require.NoError(t, err)
	_, agentReport, err := normalizeGrokResponsesProtocolCompatibility(schemaBody)
	require.NoError(t, err)
	report := mergeGrokResponsesCompatibilityReports(schemaReport, agentReport)
	before := snapshotGrokResponsesCompatibilityMetrics()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	observeGrokResponsesProtocolCompatibility(nil, "unit", report, nil)
	after := snapshotGrokResponsesCompatibilityMetrics()

	require.Greater(t, after.CompatibilityTotal["agent:encrypted_tagged"], before.CompatibilityTotal["agent:encrypted_tagged"])
	require.Greater(t, after.CompatibilityTotal["schema:fallback"], before.CompatibilityTotal["schema:fallback"])
	require.Greater(t, after.FallbackTotal["mixed_non_object_root"], before.FallbackTotal["mixed_non_object_root"])
	require.Equal(t, before.SchemaSizeDelta.Count+1, after.SchemaSizeDelta.Count)
	require.Equal(t, before.SchemaSizeDelta.Buckets["decrease"]+1, after.SchemaSizeDelta.Buckets["decrease"])
	require.Contains(t, logs.String(), `"reason":"mixed_non_object_root"`)
	require.Contains(t, logs.String(), `"schema_sha256":`)
	require.Contains(t, logs.String(), `"version":"v2"`)
	require.Contains(t, logs.String(), `"transport":"unit"`)
	for _, secret := range []string{"secret-auth-value", "secret-author", "secret-recipient", "secret-message", "secret-description", "secret-schema-title", "schema_tool"} {
		require.NotContains(t, logs.String(), secret)
	}

	productionReadout := SnapshotOpenAICompatibilityFallbackMetrics().GrokResponsesProtocol
	encodedReadout := string(marshalGrokCompatibilityJSON(t, productionReadout))
	expectedMetricNames := []string{
		"grok_responses_compat_total",
		"grok_responses_schema_fallback_total",
		"grok_responses_compat_error_total",
		"grok_responses_schema_size_delta_bytes",
	}
	require.Equal(t, expectedMetricNames, []string{
		GrokResponsesCompatibilityTotalMetricName, GrokResponsesSchemaFallbackTotalMetricName,
		GrokResponsesCompatibilityErrorTotalMetricName, GrokResponsesSchemaSizeDeltaBytesMetricName,
	})
	for _, metricName := range expectedMetricNames {
		require.Contains(t, encodedReadout, metricName)
	}
	expectedBuckets := []string{"decrease", "zero", "increase_le_256", "increase_le_1024", "increase_le_4096", "increase_gt_4096"}
	require.ElementsMatch(t, expectedBuckets, mapKeysStringUint64(productionReadout.SchemaSizeDelta.Buckets))
	require.Contains(t, productionReadout.CompatibilityTotal, "agent:error")
	require.Contains(t, productionReadout.CompatibilityTotal, "schema:fallback")
}

func grokCompatibilityStreamingResponse(responseID string) *http.Response {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"` + responseID + `","model":"grok-4.5"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"` + responseID + `","model":"grok-4.5","usage":{"input_tokens":2,"output_tokens":1}}}`,
		"",
	}, "\n")
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func loadGrokCompatibilityStaticFixtures(t *testing.T) grokCompatibilityStaticFixtures {
	t.Helper()
	path := filepath.Join("testdata", "grok_protocol_compat_static_fixtures.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var fixtures grokCompatibilityStaticFixtures
	decodeGrokCompatibilityJSON(t, raw, &fixtures)
	return fixtures
}

func assertGrokCompatibilityAgentProjection(t *testing.T, original json.RawMessage, normalized gjson.Result) {
	t.Helper()
	require.True(t, normalized.IsObject())
	var output map[string]json.RawMessage
	decodeGrokCompatibilityJSON(t, []byte(normalized.Raw), &output)
	require.ElementsMatch(t, []string{"type", "role", "content"}, mapKeysGrokCompatibility(output))
	require.Equal(t, "message", gjson.Get(normalized.Raw, "type").String())
	require.Equal(t, "user", gjson.Get(normalized.Raw, "role").String())

	var source struct {
		Author    string `json:"author"`
		Recipient string `json:"recipient"`
		Content   []struct {
			Type             string  `json:"type"`
			Text             *string `json:"text"`
			EncryptedContent *string `json:"encrypted_content"`
		} `json:"content"`
	}
	decodeGrokCompatibilityJSON(t, original, &source)
	content := normalized.Get("content").Array()
	require.Len(t, content, len(source.Content)+1)
	prefix := content[0].Get("text").String()
	require.True(t, strings.HasPrefix(prefix, grokAgentMessageProvenancePrefix))
	var provenance grokAgentMessageProvenance
	decodeGrokCompatibilityJSON(t, []byte(strings.TrimPrefix(prefix, grokAgentMessageProvenancePrefix)), &provenance)
	require.Equal(t, source.Author, provenance.Author)
	require.Equal(t, source.Recipient, provenance.Recipient)

	wantTypes := make([]string, 0, len(source.Content))
	wantStrings := make([]string, 0, len(source.Content))
	for _, part := range source.Content {
		wantTypes = append(wantTypes, part.Type)
		if part.Type == "input_text" {
			require.NotNil(t, part.Text)
			wantStrings = append(wantStrings, *part.Text)
		} else {
			require.NotNil(t, part.EncryptedContent)
			wantStrings = append(wantStrings, *part.EncryptedContent)
		}
	}
	require.Equal(t, wantTypes, provenance.SourceContentTypes)
	gotStrings := make([]string, 0, len(content)-1)
	for _, part := range content[1:] {
		require.Equal(t, "input_text", part.Get("type").String())
		gotStrings = append(gotStrings, part.Get("text").String())
	}
	require.Equal(t, wantStrings, gotStrings)
}

func assertNoGrokRootRefOrNull(t *testing.T, raw any) {
	t.Helper()
	node, ok := raw.(map[string]any)
	require.True(t, ok)
	require.NotContains(t, node, "$ref")
	if rootType, present := node["type"]; present {
		require.Equal(t, "object", rootType)
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		branches, _ := node[keyword].([]any)
		for _, branch := range branches {
			branchNode, ok := branch.(map[string]any)
			require.True(t, ok)
			require.NotEqual(t, "null", branchNode["type"])
			require.NotContains(t, branchNode, "$ref")
			assertNoGrokRootRefOrNull(t, branchNode)
		}
	}
}

func buildGrokCompatibilityRequest(t *testing.T, body []byte) []byte {
	t.Helper()
	account := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://grok.example.test/v1/"}}
	req, err := buildGrokResponsesRequest(context.Background(), nil, account, body, "synthetic-token", "", nil)
	require.NoError(t, err)
	built, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	return built
}

func decodeGrokCompatibilityJSON(t *testing.T, raw []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(target))
}

func marshalGrokCompatibilityJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func mapKeysGrokCompatibility(input map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	return keys
}

func mapKeysStringUint64(input map[string]uint64) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	return keys
}

func compatibilitySingleSchemaOutcome(report GrokResponsesCompatibilityReport) string {
	if len(report.Schemas) != 1 {
		return ""
	}
	return report.Schemas[0].Outcome
}

func grokCompatibilityDoublingDAGSchema(t *testing.T, depth int) string {
	t.Helper()
	defs := map[string]any{"n0": map[string]any{"type": "object", "properties": map[string]any{}}}
	for index := 1; index <= depth; index++ {
		previous := "#/$defs/n" + strconv.Itoa(index-1)
		defs["n"+strconv.Itoa(index)] = map[string]any{
			"anyOf": []any{map[string]any{"$ref": previous}, map[string]any{"$ref": previous}},
		}
	}
	raw := marshalGrokCompatibilityJSON(t, map[string]any{"$defs": defs, "$ref": "#/$defs/n" + strconv.Itoa(depth)})
	return string(raw)
}

// evaluateGrokCompatibilitySchema is an intentionally separate, small
// draft-2020-12 evaluator for the fixture keywords used by automation_update.
// It does not call production normalization or classification code.
func evaluateGrokCompatibilitySchema(schema any, document map[string]any, instance any) bool {
	if boolean, ok := schema.(bool); ok {
		return boolean
	}
	node, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	if reference, ok := node["$ref"].(string); ok {
		resolved, found := resolveGrokCompatibilityFixturePointer(document, reference)
		if !found || !evaluateGrokCompatibilitySchema(resolved, document, instance) {
			return false
		}
	}
	for _, keyword := range []string{"allOf"} {
		if branches, ok := node[keyword].([]any); ok {
			for _, branch := range branches {
				if !evaluateGrokCompatibilitySchema(branch, document, instance) {
					return false
				}
			}
		}
	}
	if branches, ok := node["anyOf"].([]any); ok {
		matches := false
		for _, branch := range branches {
			matches = matches || evaluateGrokCompatibilitySchema(branch, document, instance)
		}
		if !matches {
			return false
		}
	}
	if branches, ok := node["oneOf"].([]any); ok {
		matches := 0
		for _, branch := range branches {
			if evaluateGrokCompatibilitySchema(branch, document, instance) {
				matches++
			}
		}
		if matches != 1 {
			return false
		}
	}
	if rawType, present := node["type"]; present && !grokCompatibilityInstanceMatchesType(rawType, instance) {
		return false
	}
	if constant, present := node["const"]; present && !reflect.DeepEqual(constant, instance) {
		return false
	}
	if enum, ok := node["enum"].([]any); ok {
		matched := false
		for _, option := range enum {
			matched = matched || reflect.DeepEqual(option, instance)
		}
		if !matched {
			return false
		}
	}
	object, isObject := instance.(map[string]any)
	if !isObject {
		return true
	}
	if required, ok := node["required"].([]any); ok {
		for _, rawName := range required {
			name, ok := rawName.(string)
			if !ok {
				return false
			}
			if _, present := object[name]; !present {
				return false
			}
		}
	}
	properties, _ := node["properties"].(map[string]any)
	for name, propertySchema := range properties {
		if value, present := object[name]; present && !evaluateGrokCompatibilitySchema(propertySchema, document, value) {
			return false
		}
	}
	if additional, present := node["additionalProperties"].(bool); present && !additional {
		for name := range object {
			if _, known := properties[name]; !known {
				return false
			}
		}
	}
	return true
}

func grokCompatibilityInstanceMatchesType(rawType, instance any) bool {
	var types []any
	switch typed := rawType.(type) {
	case string:
		types = []any{typed}
	case []any:
		types = typed
	default:
		return false
	}
	for _, raw := range types {
		name, _ := raw.(string)
		switch name {
		case "object":
			_, ok := instance.(map[string]any)
			if ok {
				return true
			}
		case "string":
			_, ok := instance.(string)
			if ok {
				return true
			}
		case "array":
			_, ok := instance.([]any)
			if ok {
				return true
			}
		case "boolean":
			_, ok := instance.(bool)
			if ok {
				return true
			}
		case "number", "integer":
			_, ok := instance.(json.Number)
			if ok {
				return true
			}
		case "null":
			if instance == nil {
				return true
			}
		}
	}
	return false
}

func resolveGrokCompatibilityFixturePointer(document map[string]any, reference string) (any, bool) {
	if !strings.HasPrefix(reference, "#/") {
		return nil, false
	}
	var current any = document
	for _, encoded := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		node, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = node[token]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
