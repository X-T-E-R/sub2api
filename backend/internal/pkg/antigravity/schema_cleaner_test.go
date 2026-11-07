package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func cleanRawJSONSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var in map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &in))
	return CleanJSONSchema(in)
}

func TestCleanJSONSchema_ResolvesJSONPointerAndSelfRefBySegment(t *testing.T) {
	cleaned := cleanRawJSONSchema(t, `{
		"type": "object",
		"$defs": {"Mode": {"type": "string", "enum": ["x", "y"]}},
		"properties": {
			"a": {"type": "object", "properties": {"n": {"type": "integer"}}},
			"ab": {
				"type": "object",
				"properties": {
					"inner": {"$ref": "#/properties/a"},
					"mode": {"$ref": "#/components/schemas/Mode"}
				}
			},
			"func": {
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"triggers": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"kind": {"type": "string", "enum": ["command", "prompt"]},
								"self": {"$ref": "#/properties/func/properties/triggers/items"}
							},
							"required": ["kind"]
						}
					}
				}
			},
			"again": {"$ref": "#/properties/func/properties/triggers/items"}
		}
	}`)

	props := cleaned["properties"].(map[string]any)
	ab := props["ab"].(map[string]any)["properties"].(map[string]any)
	inner := ab["inner"].(map[string]any)
	require.Equal(t, "object", inner["type"])
	require.Equal(t, "integer", inner["properties"].(map[string]any)["n"].(map[string]any)["type"])
	mode := ab["mode"].(map[string]any)
	require.Equal(t, "string", mode["type"])
	require.Equal(t, []any{"x", "y"}, mode["enum"])

	triggersItems := props["func"].(map[string]any)["properties"].(map[string]any)["triggers"].(map[string]any)["items"].(map[string]any)
	selfProp := triggersItems["properties"].(map[string]any)["self"].(map[string]any)
	require.Equal(t, "object", selfProp["type"])

	again := props["again"].(map[string]any)
	require.Equal(t, "object", again["type"])
	require.Equal(t, []any{"kind"}, again["required"])
	require.Equal(t, []any{"command", "prompt"}, again["properties"].(map[string]any)["kind"].(map[string]any)["enum"])
}

func TestCleanJSONSchema_RootAndNestedUnionKeepsOwnObjectShape(t *testing.T) {
	// Philo rg tool reproduction (#4046): root object with properties + constraint-only oneOf.
	rg := cleanRawJSONSchema(t, `{
		"type": "object",
		"properties": {
			"pattern": {"type": "string"},
			"cursor": {"type": "string"}
		},
		"oneOf": [
			{"required": ["pattern"], "not": {"required": ["cursor"]}},
			{"required": ["cursor"], "not": {"required": ["pattern"]}}
		]
	}`)
	require.Equal(t, "object", rg["type"])
	rgProps := rg["properties"].(map[string]any)
	require.NotNil(t, rgProps["pattern"])
	require.NotNil(t, rgProps["cursor"])
	require.Nil(t, rg["required"])

	// Union branches add fields and intersect required fields across non-null branches.
	folded := cleanRawJSONSchema(t, `{
		"type": "object",
		"properties": {"kind": {"type": "string"}},
		"required": ["kind"],
		"oneOf": [
			{"properties": {"kind": {"const": "a"}, "x": {"type": "string"}, "common": {"type": "string"}}, "required": ["kind", "x", "common"]},
			{"properties": {"kind": {"const": "b"}, "y": {"type": "number"}, "common": {"type": "string"}}, "required": ["kind", "y", "common"]}
		]
	}`)
	require.Equal(t, "object", folded["type"])
	foldedProps := folded["properties"].(map[string]any)
	require.NotNil(t, foldedProps["kind"])
	require.NotNil(t, foldedProps["x"])
	require.NotNil(t, foldedProps["y"])
	require.NotNil(t, foldedProps["common"])
	require.Equal(t, []any{"kind", "common"}, folded["required"])
}

func TestCleanJSONSchema_ArraysAlwaysHaveItemsAndPrefixItems(t *testing.T) {
	cleaned := cleanRawJSONSchema(t, `{
		"type": "object",
		"$defs": {"Tags": {"type": "array", "description": "tags"}},
		"properties": {
			"batch": {"type": "array"},
			"grid": {"type": "array", "items": {"type": "array"}},
			"maybe": {"anyOf": [{"type": "null"}, {"type": "array"}]},
			"tags": {"$ref": "#/$defs/Tags"},
			"opt": {"type": ["array", "null"]},
			"homogeneous_prefix": {"type": "array", "prefixItems": [{"type": "number", "description": "x"}, {}, {"type": "number", "description": "x"}]},
			"heterogeneous_prefix": {"type": "array", "prefixItems": [{"type": "string"}, {"type": "number"}]}
		}
	}`)
	props := cleaned["properties"].(map[string]any)
	for _, name := range []string{"batch", "maybe", "tags", "opt", "heterogeneous_prefix"} {
		prop := props[name].(map[string]any)
		require.Equal(t, "array", prop["type"], name)
		require.Equal(t, map[string]any{}, prop["items"], name)
	}
	grid := props["grid"].(map[string]any)
	require.Equal(t, "array", grid["type"])
	gridItems := grid["items"].(map[string]any)
	require.Equal(t, "array", gridItems["type"])
	require.Equal(t, map[string]any{}, gridItems["items"])

	homo := props["homogeneous_prefix"].(map[string]any)
	require.Equal(t, "array", homo["type"])
	homoItems := homo["items"].(map[string]any)
	require.Equal(t, "number", homoItems["type"])
	require.Equal(t, "x", homoItems["description"])
}

func TestCleanJSONSchema_BooleanSchemasBlankEnumsConstAndStrayKeywords(t *testing.T) {
	cleaned := cleanRawJSONSchema(t, `{
		"type": "object",
		"properties": {
			"allowed_any": true,
			"forbidden_prop": false,
			"matrix": {"type": "array", "items": {"type": "array", "items": true}},
			"empty_tuple": {"type": "array", "items": false},
			"status": {"type": "string", "enum": ["running", "", "   ", "stopped"]},
			"blank_only": {"type": "string", "enum": ["", "  "]},
			"mode": {"const": "read"},
			"display": {"type": "string", "enum": ["a", "b"], "items": {"type": "string"}, "properties": {"bad": {"type": "string"}}}
		},
		"required": ["allowed_any", "forbidden_prop", "status"]
	}`)

	props := cleaned["properties"].(map[string]any)
	require.NotContains(t, props, "forbidden_prop")
	require.Equal(t, []any{"allowed_any", "status"}, cleaned["required"])

	matrixItems := props["matrix"].(map[string]any)["items"].(map[string]any)
	require.Equal(t, "array", matrixItems["type"])
	require.Equal(t, map[string]any{}, matrixItems["items"])

	emptyTuple := props["empty_tuple"].(map[string]any)
	require.Equal(t, "array", emptyTuple["type"])
	require.Equal(t, map[string]any{}, emptyTuple["items"])
	require.Contains(t, emptyTuple["description"], "maxItems: 0")

	status := props["status"].(map[string]any)
	require.Equal(t, []any{"running", "stopped"}, status["enum"])

	blankOnly := props["blank_only"].(map[string]any)
	require.NotContains(t, blankOnly, "enum")

	mode := props["mode"].(map[string]any)
	require.Equal(t, "string", mode["type"])
	require.Equal(t, []any{"read"}, mode["enum"])

	display := props["display"].(map[string]any)
	require.Equal(t, "string", display["type"])
	require.NotContains(t, display, "items")
	require.NotContains(t, display, "properties")
}

func TestCollectAndStreamStripDefaultAPIToolNamespace(t *testing.T) {
	resp := NewNonStreamingProcessor().Process(&GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Role: "model", Parts: []GeminiPart{
				{FunctionCall: &GeminiFunctionCall{ID: "call_colon", Name: "default_api:bash", Args: map[string]any{"command": "ls"}}},
				{FunctionCall: &GeminiFunctionCall{Name: "default_api.read_file", Args: map[string]any{"path": "a.txt"}}},
				{FunctionCall: &GeminiFunctionCall{ID: "call_ bare", Name: "default_api", Args: map[string]any{}}},
			}},
		}},
	}, "resp", "gemini-3-flash")
	require.Len(t, resp.Content, 3)
	require.Equal(t, "bash", resp.Content[0].Name)
	require.Equal(t, "call_colon", resp.Content[0].ID)
	require.Equal(t, "read_file", resp.Content[1].Name)
	require.True(t, strings.HasPrefix(resp.Content[1].ID, "read_file-"), "expected stripped name prefix in generated ID, got %q", resp.Content[1].ID)
	require.Equal(t, "default_api", resp.Content[2].Name)

	p := NewStreamingProcessor("gemini-3-flash")
	payload := streamChunk(t, p, `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"default_api:bash","args":{"command":"ls"}}},{"functionCall":{"name":"default_api.edit","args":{"path":"a.go"}}}]},"finishReason":"STOP"}]}}`)
	final, _ := p.Finish()
	payload = append(payload, final...)
	blocks := streamBlocks(t, payload)
	require.Len(t, blocks, 2)
	require.Equal(t, "bash", blocks[0]["name"])
	require.Equal(t, "call_1", blocks[0]["id"])
	require.Equal(t, "edit", blocks[1]["name"])
	require.True(t, strings.HasPrefix(blocks[1]["id"].(string), "edit-"))
}

func TestCleanJSONSchema_ResolvesPointerIntoDefsAndEscapedDefinitions(t *testing.T) {
	cleaned := cleanRawJSONSchema(t, `{
		"type": "object",
		"$defs": {
			"Foo": {
				"type": "object",
				"properties": {
					"x": {"type": "string", "enum": ["alpha", "beta"]}
				}
			}
		},
		"definitions": {
			"ns/item~1": {
				"type": "object",
				"properties": {
					"a/b~c": {"type": "integer", "minimum": 5}
				}
			}
		},
		"properties": {
			"direct_x": {"$ref": "#/$defs/Foo/properties/x"},
			"escaped_prop": {"$ref": "#/definitions/ns~1item~01/properties/a~1b~0c"}
		},
		"required": ["direct_x", "escaped_prop"]
	}`)

	require.NotContains(t, cleaned, "$defs")
	require.NotContains(t, cleaned, "definitions")
	props := cleaned["properties"].(map[string]any)

	directX := props["direct_x"].(map[string]any)
	require.Equal(t, "string", directX["type"])
	require.Equal(t, []any{"alpha", "beta"}, directX["enum"])

	escapedProp := props["escaped_prop"].(map[string]any)
	require.Equal(t, "integer", escapedProp["type"])
	require.Contains(t, escapedProp["description"], "min: 5")
	require.Equal(t, []any{"direct_x", "escaped_prop"}, cleaned["required"])
}

func TestCleanJSONSchema_NestedAllOfWithRefAndRequiredOnlyBranches(t *testing.T) {
	// Counterexample 1: exact nested allOf shape reported in review.
	nested := cleanRawJSONSchema(t, `{
		"type": "object",
		"allOf": [
			{
				"allOf": [
					{
						"type": "object",
						"properties": {"x": {"type": "string"}},
						"required": ["x"]
					}
				]
			}
		]
	}`)
	require.Equal(t, "object", nested["type"])
	nestedProps := nested["properties"].(map[string]any)
	require.Equal(t, "string", nestedProps["x"].(map[string]any)["type"])
	require.NotContains(t, nestedProps, "reason")
	require.Equal(t, []any{"x"}, nested["required"])

	// Counterexample 2: nested allOf combining $ref branches, required-only branches, and constraint-only branches.
	complexAllOf := cleanRawJSONSchema(t, `{
		"type": "object",
		"$defs": {
			"Shared": {
				"allOf": [
					{"type": "object", "properties": {"y": {"type": "integer"}, "z": {"type": "boolean"}}},
					{"required": ["y"]}
				]
			},
			"ReqZ": {
				"required": ["z", "missing"]
			}
		},
		"properties": {
			"kept": {"type": "string"}
		},
		"required": ["kept"],
		"allOf": [
			{"allOf": [{"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]}]},
			{"$ref": "#/$defs/Shared"},
			{"$ref": "#/$defs/ReqZ"},
			{"not": {"required": ["ignored"]}}
		]
	}`)
	require.Equal(t, "object", complexAllOf["type"])
	cProps := complexAllOf["properties"].(map[string]any)
	require.Len(t, cProps, 4)
	require.Equal(t, "string", cProps["kept"].(map[string]any)["type"])
	require.Equal(t, "string", cProps["x"].(map[string]any)["type"])
	require.Equal(t, "integer", cProps["y"].(map[string]any)["type"])
	require.Equal(t, "boolean", cProps["z"].(map[string]any)["type"])
	require.NotContains(t, cProps, "reason")
	require.Equal(t, []any{"kept", "x", "y", "z"}, complexAllOf["required"])
}
