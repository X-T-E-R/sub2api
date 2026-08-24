package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokResponsesProtocolCompatibilityVersion  = "v2"
	grokResponsesProtocolCompatibilityExtraKey = "grok_responses_protocol_compat_v1"
	grokAgentMessageProvenancePrefix           = "[Message authored by another agent; not a human request or approval.]\nProvenance: "
	grokMissingParametersFingerprintSentinel   = "grok-responses-missing-parameters:v2"

	GrokResponsesCompatibilityTotalMetricName      = "grok_responses_compat_total"
	GrokResponsesSchemaFallbackTotalMetricName     = "grok_responses_schema_fallback_total"
	GrokResponsesCompatibilityErrorTotalMetricName = "grok_responses_compat_error_total"
	GrokResponsesSchemaSizeDeltaBytesMetricName    = "grok_responses_schema_size_delta_bytes"
)

var grokResponsesSchemaSizeDeltaBucketLabels = []string{
	"decrease",
	"zero",
	"increase_le_256",
	"increase_le_1024",
	"increase_le_4096",
	"increase_gt_4096",
}

// GrokResponsesCompatibilityError is a deterministic local request error. It
// deliberately exposes only the failing JSON path and compatibility reason,
// never the message text or raw schema.
type GrokResponsesCompatibilityError struct {
	Code   string
	Path   string
	Reason string
}

func (e *GrokResponsesCompatibilityError) Error() string {
	if e == nil {
		return "grok responses compatibility error"
	}
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Reason)
}

type GrokResponsesCompatibilitySchemaResult struct {
	ToolIndex     int
	ToolName      string
	Outcome       string
	Reason        string
	Fingerprint   string
	StrictBefore  bool
	StrictAfter   bool
	SizeDeltaByte int
}

type GrokResponsesCompatibilityReport struct {
	AgentItems          int
	AgentParts          int
	AgentPlaintext      int
	AgentEncrypted      int
	AgentMixed          int
	AgentEmpty          int
	AgentErrors         int
	SchemaUnchanged     int
	SchemaInlined       int
	SchemaNullPruned    int
	SchemaObjectUnion   int
	SchemaCanonicalized int
	SchemaFallback      int
	SchemaErrors        int
	Schemas             []GrokResponsesCompatibilitySchemaResult
}

type grokResponsesCompatibilityMetricsState struct {
	mu             sync.Mutex
	total          map[string]uint64
	fallbackReason map[string]uint64
	errorReason    map[string]uint64
	sizeBuckets    map[string]uint64
	sizeDeltaCount uint64
	sizeDeltaTotal int64
}

var grokResponsesCompatibilityMetrics = grokResponsesCompatibilityMetricsState{
	total:          make(map[string]uint64),
	fallbackReason: make(map[string]uint64),
	errorReason:    make(map[string]uint64),
	sizeBuckets:    make(map[string]uint64),
}

type GrokResponsesCompatibilitySizeDeltaSnapshot struct {
	Count   uint64            `json:"count"`
	Sum     int64             `json:"sum"`
	Buckets map[string]uint64 `json:"buckets"`
}

// GrokResponsesCompatibilityMetricsSnapshot is the bounded process-local
// readout consumed by the gateway's existing periodic compatibility metrics
// log. Sub2API does not currently register a Prometheus or OpenTelemetry
// exporter, so this follows the established production snapshot convention.
type GrokResponsesCompatibilityMetricsSnapshot struct {
	CompatibilityTotal map[string]uint64                           `json:"grok_responses_compat_total"`
	FallbackTotal      map[string]uint64                           `json:"grok_responses_schema_fallback_total"`
	ErrorTotal         map[string]uint64                           `json:"grok_responses_compat_error_total"`
	SchemaSizeDelta    GrokResponsesCompatibilitySizeDeltaSnapshot `json:"grok_responses_schema_size_delta_bytes"`
}

// SnapshotGrokResponsesCompatibilityMetrics returns an isolated, concurrency-
// safe copy with fixed outcome and size-bucket labels.
func SnapshotGrokResponsesCompatibilityMetrics() GrokResponsesCompatibilityMetricsSnapshot {
	grokResponsesCompatibilityMetrics.mu.Lock()
	defer grokResponsesCompatibilityMetrics.mu.Unlock()
	total := cloneStringUint64Map(grokResponsesCompatibilityMetrics.total)
	for _, label := range []string{
		"agent:plaintext", "agent:encrypted_tagged", "agent:mixed", "agent:empty", "agent:error",
		"schema:unchanged", "schema:inlined", "schema:null_pruned", "schema:object_union", "schema:canonicalized", "schema:fallback", "schema:error",
	} {
		if _, exists := total[label]; !exists {
			total[label] = 0
		}
	}
	buckets := cloneStringUint64Map(grokResponsesCompatibilityMetrics.sizeBuckets)
	for _, label := range grokResponsesSchemaSizeDeltaBucketLabels {
		if _, exists := buckets[label]; !exists {
			buckets[label] = 0
		}
	}
	return GrokResponsesCompatibilityMetricsSnapshot{
		CompatibilityTotal: total,
		FallbackTotal:      cloneStringUint64Map(grokResponsesCompatibilityMetrics.fallbackReason),
		ErrorTotal:         cloneStringUint64Map(grokResponsesCompatibilityMetrics.errorReason),
		SchemaSizeDelta: GrokResponsesCompatibilitySizeDeltaSnapshot{
			Count:   grokResponsesCompatibilityMetrics.sizeDeltaCount,
			Sum:     grokResponsesCompatibilityMetrics.sizeDeltaTotal,
			Buckets: buckets,
		},
	}
}

func snapshotGrokResponsesCompatibilityMetrics() GrokResponsesCompatibilityMetricsSnapshot {
	return SnapshotGrokResponsesCompatibilityMetrics()
}

func cloneStringUint64Map(input map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func isGrokResponsesProtocolCompatibilityEnabled(account *Account) bool {
	if account == nil || account.Platform != PlatformGrok {
		return false
	}
	if account.Extra == nil {
		return true
	}
	enabled, present := account.Extra[grokResponsesProtocolCompatibilityExtraKey]
	if !present {
		return true
	}
	value, ok := enabled.(bool)
	return !ok || value
}

// normalizeGrokResponsesProtocolCompatibility performs the Native/WS-only
// agent_message projection after shared Grok base and tool sanitation. Function
// schema compatibility is owned exclusively by sanitizeGrokResponsesTools.
// This helper is copy-on-write: failures return no candidate body and do not
// mutate the caller-owned bytes.
func normalizeGrokResponsesProtocolCompatibility(body []byte) ([]byte, GrokResponsesCompatibilityReport, error) {
	var report GrokResponsesCompatibilityReport
	if !json.Valid(body) {
		return nil, report, &GrokResponsesCompatibilityError{Code: "invalid_request", Path: "$", Reason: "invalid_json"}
	}

	candidate := append([]byte(nil), body...)
	normalizedInput, inputChanged, err := normalizeGrokAgentMessages(gjson.GetBytes(body, "input"), &report)
	if err != nil {
		return nil, report, err
	}
	if inputChanged {
		candidate, err = sjson.SetRawBytes(candidate, "input", normalizedInput)
		if err != nil {
			return nil, report, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: "input", Reason: "encode_failed"}
		}
	}

	if !inputChanged {
		return body, report, nil
	}
	return candidate, report, nil
}

func mergeGrokResponsesCompatibilityReports(reports ...GrokResponsesCompatibilityReport) GrokResponsesCompatibilityReport {
	var merged GrokResponsesCompatibilityReport
	for _, report := range reports {
		merged.AgentItems += report.AgentItems
		merged.AgentParts += report.AgentParts
		merged.AgentPlaintext += report.AgentPlaintext
		merged.AgentEncrypted += report.AgentEncrypted
		merged.AgentMixed += report.AgentMixed
		merged.AgentEmpty += report.AgentEmpty
		merged.AgentErrors += report.AgentErrors
		merged.SchemaUnchanged += report.SchemaUnchanged
		merged.SchemaInlined += report.SchemaInlined
		merged.SchemaNullPruned += report.SchemaNullPruned
		merged.SchemaObjectUnion += report.SchemaObjectUnion
		merged.SchemaCanonicalized += report.SchemaCanonicalized
		merged.SchemaFallback += report.SchemaFallback
		merged.SchemaErrors += report.SchemaErrors
		merged.Schemas = append(merged.Schemas, report.Schemas...)
	}
	return merged
}

type grokAgentMessageProvenance struct {
	Author             string   `json:"author"`
	Recipient          string   `json:"recipient"`
	SourceContentTypes []string `json:"source_content_types"`
}

type grokStandardInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type grokStandardUserMessage struct {
	Type    string                  `json:"type"`
	Role    string                  `json:"role"`
	Content []grokStandardInputText `json:"content"`
}

func normalizeGrokAgentMessages(input gjson.Result, report *GrokResponsesCompatibilityReport) ([]byte, bool, error) {
	if !input.Exists() || !input.IsArray() {
		return nil, false, nil
	}
	items := input.Array()
	normalized := make([]json.RawMessage, 0, len(items))
	changed := false
	for index, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			normalized = append(normalized, json.RawMessage(item.Raw))
			continue
		}
		converted, category, partCount, err := normalizeGrokAgentMessageItem([]byte(item.Raw), index)
		if err != nil {
			report.AgentErrors++
			return nil, false, err
		}
		report.AgentItems++
		report.AgentParts += partCount
		switch category {
		case "plaintext":
			report.AgentPlaintext++
		case "encrypted_tagged":
			report.AgentEncrypted++
		case "mixed":
			report.AgentMixed++
		case "empty":
			report.AgentEmpty++
		}
		normalized = append(normalized, converted)
		changed = true
	}
	if !changed {
		return nil, false, nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, false, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: "input", Reason: "encode_failed"}
	}
	return encoded, true, nil
}

func normalizeGrokAgentMessageItem(raw []byte, index int) (json.RawMessage, string, int, error) {
	path := fmt.Sprintf("input[%d]", index)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path, Reason: "malformed_object"}
	}
	author, ok := decodeGrokCompatibilityJSONString(fields["author"])
	if !ok {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path + ".author", Reason: "expected_string"}
	}
	recipient, ok := decodeGrokCompatibilityJSONString(fields["recipient"])
	if !ok {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path + ".recipient", Reason: "expected_string"}
	}
	var sourceContent []json.RawMessage
	if len(fields["content"]) == 0 || json.Unmarshal(fields["content"], &sourceContent) != nil || sourceContent == nil {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path + ".content", Reason: "expected_array"}
	}

	tags := make([]string, 0, len(sourceContent))
	values := make([]string, 0, len(sourceContent))
	plainCount := 0
	encryptedCount := 0
	for partIndex, rawPart := range sourceContent {
		partPath := fmt.Sprintf("%s.content[%d]", path, partIndex)
		var partFields map[string]json.RawMessage
		if err := json.Unmarshal(rawPart, &partFields); err != nil || partFields == nil {
			return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: partPath, Reason: "malformed_object"}
		}
		partType, ok := decodeGrokCompatibilityJSONString(partFields["type"])
		if !ok {
			return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: partPath + ".type", Reason: "expected_string"}
		}
		switch partType {
		case "input_text":
			text, valid := decodeGrokCompatibilityJSONString(partFields["text"])
			if !valid {
				return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: partPath + ".text", Reason: "expected_string"}
			}
			tags = append(tags, "input_text")
			values = append(values, text)
			plainCount++
		case "encrypted_content":
			encrypted, valid := decodeGrokCompatibilityJSONString(partFields["encrypted_content"])
			if !valid {
				return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: partPath + ".encrypted_content", Reason: "expected_string"}
			}
			tags = append(tags, "encrypted_content")
			values = append(values, encrypted)
			encryptedCount++
		default:
			return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: partPath + ".type", Reason: "unsupported_content_type"}
		}
	}

	provenance, err := json.Marshal(grokAgentMessageProvenance{
		Author:             author,
		Recipient:          recipient,
		SourceContentTypes: tags,
	})
	if err != nil {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path, Reason: "provenance_encode_failed"}
	}
	outputContent := make([]grokStandardInputText, 0, len(values)+1)
	outputContent = append(outputContent, grokStandardInputText{Type: "input_text", Text: grokAgentMessageProvenancePrefix + string(provenance)})
	for _, value := range values {
		outputContent = append(outputContent, grokStandardInputText{Type: "input_text", Text: value})
	}
	encoded, err := json.Marshal(grokStandardUserMessage{Type: "message", Role: "user", Content: outputContent})
	if err != nil {
		return nil, "", 0, &GrokResponsesCompatibilityError{Code: "invalid_agent_message", Path: path, Reason: "encode_failed"}
	}
	category := "mixed"
	switch {
	case len(values) == 0:
		category = "empty"
	case encryptedCount == 0:
		category = "plaintext"
	case plainCount == 0:
		category = "encrypted_tagged"
	}
	return encoded, category, len(values), nil
}

func decodeGrokCompatibilityJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

type grokRootSchemaState struct {
	inlined       bool
	nullPruned    bool
	union         bool
	canonicalized bool
}

type grokSchemaCompatibilityLimits struct {
	maxInputBytes    int
	maxDepth         int
	maxWork          int
	maxRefs          int
	maxProducedNodes int
	maxProducedBytes int
	maxOutputBytes   int
}

var defaultGrokSchemaCompatibilityLimits = grokSchemaCompatibilityLimits{
	maxInputBytes:    4 << 20,
	maxDepth:         64,
	maxWork:          8192,
	maxRefs:          256,
	maxProducedNodes: 65536,
	maxProducedBytes: 8 << 20,
	maxOutputBytes:   4 << 20,
}

type grokSchemaCompatibilityBudget struct {
	limits        grokSchemaCompatibilityLimits
	work          int
	refs          int
	producedNodes int
	producedBytes int
}

type grokSchemaValueStats struct {
	nodes int
	bytes int
}

type grokRootSchemaDomain struct {
	mask              uint8
	explicitNonObject bool
}

const (
	grokSchemaObject uint8 = 1 << iota
	grokSchemaNull
	grokSchemaNonObject
	grokSchemaAll = grokSchemaObject | grokSchemaNull | grokSchemaNonObject
)

func normalizeGrokFunctionToolSchemas(tools gjson.Result, report *GrokResponsesCompatibilityReport) ([]byte, bool, error) {
	if !tools.Exists() || !tools.IsArray() {
		return nil, false, nil
	}
	rawTools := tools.Array()
	normalized := make([]json.RawMessage, 0, len(rawTools))
	changed := false
	for index, rawTool := range rawTools {
		if strings.TrimSpace(rawTool.Get("type").String()) != "function" {
			normalized = append(normalized, json.RawMessage(rawTool.Raw))
			continue
		}
		parameters := rawTool.Get("parameters")
		parametersMissing := !parameters.Exists()
		originalParameters := []byte(parameters.Raw)
		strictBefore := rawTool.Get("strict").Bool()
		result, schemaChanged, outcome, reason, err := normalizeGrokFunctionParameters(originalParameters, fmt.Sprintf("tools[%d].parameters", index))
		fingerprint := grokSchemaFingerprint(originalParameters)
		if parametersMissing {
			result = permissiveGrokObjectSchema()
			schemaChanged = true
			outcome = "fallback"
			reason = "missing_parameters"
			fingerprint = grokMissingParametersFingerprint()
		} else if parameters.Type == gjson.Null {
			result = permissiveGrokObjectSchema()
			schemaChanged = true
			outcome = "fallback"
			reason = "null_parameters"
		}
		if err != nil {
			report.SchemaErrors++
			report.Schemas = append(report.Schemas, GrokResponsesCompatibilitySchemaResult{
				ToolIndex: index, ToolName: rawTool.Get("name").String(), Outcome: "error", Reason: compatibilityErrorReason(err),
				Fingerprint: fingerprint, StrictBefore: strictBefore, StrictAfter: strictBefore,
			})
			return nil, false, err
		}

		toolBytes := []byte(rawTool.Raw)
		strictAfter := strictBefore
		if schemaChanged {
			var setErr error
			toolBytes, setErr = sjson.SetRawBytes(append([]byte(nil), toolBytes...), "parameters", result)
			if setErr != nil {
				return nil, false, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: fmt.Sprintf("tools[%d].parameters", index), Reason: "encode_failed"}
			}
			if outcome == "fallback" {
				toolBytes, setErr = sjson.SetBytes(toolBytes, "strict", false)
				if setErr != nil {
					return nil, false, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: fmt.Sprintf("tools[%d].strict", index), Reason: "encode_failed"}
				}
				strictAfter = false
			}
			changed = true
		}
		delta := len(result) - len(originalParameters)
		switch outcome {
		case "unchanged":
			report.SchemaUnchanged++
		case "inlined":
			report.SchemaInlined++
		case "null_pruned":
			report.SchemaNullPruned++
		case "object_union":
			report.SchemaObjectUnion++
		case "canonicalized":
			report.SchemaCanonicalized++
		case "fallback":
			report.SchemaFallback++
		}
		report.Schemas = append(report.Schemas, GrokResponsesCompatibilitySchemaResult{
			ToolIndex: index, ToolName: rawTool.Get("name").String(), Outcome: outcome, Reason: reason,
			Fingerprint: fingerprint, StrictBefore: strictBefore, StrictAfter: strictAfter, SizeDeltaByte: delta,
		})
		normalized = append(normalized, json.RawMessage(toolBytes))
	}
	if !changed {
		return nil, false, nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, false, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: "tools", Reason: "encode_failed"}
	}
	return encoded, true, nil
}

func normalizeGrokFunctionParameters(raw []byte, path string) ([]byte, bool, string, string, error) {
	return normalizeGrokFunctionParametersWithLimits(raw, path, defaultGrokSchemaCompatibilityLimits)
}

func normalizeGrokFunctionParametersWithLimits(raw []byte, path string, limits grokSchemaCompatibilityLimits) ([]byte, bool, string, string, error) {
	result, changed, outcome, reason, err := tryNormalizeGrokFunctionParametersWithLimits(raw, path, limits)
	if err == nil || compatibilityErrorReason(err) == "encode_failed" {
		return result, changed, outcome, reason, err
	}
	fallback := permissiveGrokObjectSchema()
	return fallback, !bytes.Equal(raw, fallback), "fallback", compatibilityErrorReason(err), nil
}

func tryNormalizeGrokFunctionParametersWithLimits(raw []byte, path string, limits grokSchemaCompatibilityLimits) ([]byte, bool, string, string, error) {
	if limits.maxInputBytes > 0 && len(raw) > limits.maxInputBytes {
		return nil, false, "error", "compatibility_input_size_limit_exceeded", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_input_size_limit_exceeded"}
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, false, "error", "malformed_schema", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "malformed_schema"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false, "error", "malformed_schema", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "malformed_schema"}
	}
	if boolean, ok := document.(bool); ok {
		if !boolean {
			return nil, false, "error", "false_schema", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "false_schema"}
		}
		canonical := permissiveGrokObjectSchema()
		return canonical, !bytes.Equal(raw, canonical), "canonicalized", "boolean_true", nil
	}
	root, ok := document.(map[string]any)
	if !ok {
		return nil, false, "error", "expected_schema_object", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "expected_schema_object"}
	}
	if len(root) == 0 {
		canonical := permissiveGrokObjectSchema()
		return canonical, !bytes.Equal(raw, canonical), "canonicalized", "empty_schema", nil
	}

	resolver := grokSchemaResolver{
		document:     root,
		path:         path,
		budget:       &grokSchemaCompatibilityBudget{limits: limits},
		pointerCache: make(map[string]any),
		statsCache:   make(map[string]grokSchemaValueStats),
	}
	state := &grokRootSchemaState{}
	normalized, domain, err := resolver.normalizeRootNode(cloneGrokJSONValue(root), path, nil, 0, state)
	if err != nil {
		return nil, false, "error", compatibilityErrorReason(err), err
	}
	if domain.mask&grokSchemaObject == 0 {
		reason := "non_object_root"
		if domain.mask == grokSchemaNull {
			reason = "null_only_root"
		}
		fallback := permissiveGrokObjectSchema()
		return fallback, !bytes.Equal(raw, fallback), "fallback", reason, nil
	}
	if domain.explicitNonObject && domain.mask&grokSchemaNonObject != 0 {
		fallback := permissiveGrokObjectSchema()
		return fallback, !bytes.Equal(raw, fallback), "fallback", "mixed_non_object_root", nil
	}

	normalizedRoot, ok := normalized.(map[string]any)
	if !ok {
		return nil, false, "error", "malformed_schema", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "malformed_schema"}
	}
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, present := root[defsKey]; present {
			normalizedRoot[defsKey] = cloneGrokJSONValue(defs)
		}
	}
	encoded, err := json.Marshal(normalizedRoot)
	if err != nil {
		return nil, false, "error", "encode_failed", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "encode_failed"}
	}
	if limits.maxOutputBytes > 0 && len(encoded) > limits.maxOutputBytes {
		return nil, false, "error", "compatibility_output_size_limit_exceeded", &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_output_size_limit_exceeded"}
	}
	if bytes.Equal(raw, encoded) || (!state.inlined && !state.nullPruned && !state.union && !state.canonicalized) {
		return raw, false, "unchanged", "", nil
	}
	outcome := "canonicalized"
	reason := "implicit_object_domain"
	switch {
	case state.inlined:
		outcome, reason = "inlined", "local_root_ref"
	case state.nullPruned:
		outcome, reason = "null_pruned", "root_null_unreachable"
	case state.union:
		outcome, reason = "object_union", "object_variants_preserved"
	}
	return encoded, !bytes.Equal(raw, encoded), outcome, reason, nil
}

type grokSchemaResolver struct {
	document     map[string]any
	path         string
	budget       *grokSchemaCompatibilityBudget
	pointerCache map[string]any
	statsCache   map[string]grokSchemaValueStats
}

func (r grokSchemaResolver) normalizeRootNode(value any, path string, active []string, depth int, state *grokRootSchemaState) (any, grokRootSchemaDomain, error) {
	if err := r.budget.consumeWork(path, depth); err != nil {
		return nil, grokRootSchemaDomain{}, err
	}
	if boolean, ok := value.(bool); ok {
		if !boolean {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "false_schema"}
		}
		return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}, grokRootSchemaDomain{mask: grokSchemaObject}, nil
	}
	node, ok := value.(map[string]any)
	if !ok {
		return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "malformed_schema_node"}
	}
	if _, exists := node["$dynamicRef"]; exists {
		return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$dynamicRef", Reason: "dynamic_ref_unsupported"}
	}
	if _, exists := node["$id"]; exists {
		return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$id", Reason: "schema_scope_unsupported"}
	}
	if rawRef, exists := node["$ref"]; exists {
		ref, ok := rawRef.(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$ref", Reason: "malformed_ref"}
		}
		if !strings.HasPrefix(ref, "#") {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$ref", Reason: "external_ref_unsupported"}
		}
		for _, current := range active {
			if current == ref {
				return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$ref", Reason: "cyclic_ref"}
			}
		}
		if err := r.budget.consumeRef(path + ".$ref"); err != nil {
			return nil, grokRootSchemaDomain{}, err
		}
		target, resolveErr := r.resolvePointer(ref)
		if resolveErr != nil {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".$ref", Reason: resolveErr.Error()}
		}
		if err := r.reserveRefExpansion(ref, target, path+".$ref"); err != nil {
			return nil, grokRootSchemaDomain{}, err
		}
		resolved, _, err := r.normalizeRootNode(cloneGrokJSONValue(target), path+".$ref", append(active, ref), depth+1, state)
		if err != nil {
			return nil, grokRootSchemaDomain{}, err
		}
		state.inlined = true
		annotations := make(map[string]any)
		assertions := make(map[string]any)
		for key, sibling := range node {
			if key == "$ref" || key == "$defs" || key == "definitions" {
				continue
			}
			if isGrokSchemaAnnotationKeyword(key) {
				annotations[key] = sibling
			} else {
				assertions[key] = sibling
			}
		}
		combined := resolved
		if len(assertions) > 0 {
			combined = map[string]any{"allOf": []any{resolved, assertions}}
		}
		if combinedMap, ok := combined.(map[string]any); ok {
			for key, annotation := range annotations {
				combinedMap[key] = annotation
			}
		}
		return r.normalizeRootNode(combined, path, active, depth+1, state)
	}

	domain, err := grokSchemaDirectDomain(node, path)
	if err != nil {
		return nil, grokRootSchemaDomain{}, err
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		raw, exists := node[keyword]
		if !exists {
			continue
		}
		branches, ok := raw.([]any)
		if !ok || len(branches) == 0 {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + "." + keyword, Reason: "malformed_combinator"}
		}
		normalizedBranches := make([]any, 0, len(branches))
		branchDomains := make([]grokRootSchemaDomain, 0, len(branches))
		for index, branch := range branches {
			normalizedBranch, branchDomain, branchErr := r.normalizeRootNode(branch, fmt.Sprintf("%s.%s[%d]", path, keyword, index), active, depth+1, state)
			if branchErr != nil {
				return nil, grokRootSchemaDomain{}, branchErr
			}
			if keyword != "allOf" && branchDomain.mask == grokSchemaNull {
				state.nullPruned = true
				continue
			}
			normalizedBranches = append(normalizedBranches, normalizedBranch)
			branchDomains = append(branchDomains, branchDomain)
		}
		if len(normalizedBranches) == 0 {
			node = map[string]any{"type": "null"}
			domain = grokRootSchemaDomain{mask: grokSchemaNull}
			break
		}
		if len(normalizedBranches) == 1 && keyword != "allOf" {
			delete(node, keyword)
			node = combineGrokRootSchemaNode(normalizedBranches[0], node)
			domain = intersectGrokSchemaDomain(domain, branchDomains[0])
			continue
		}
		node[keyword] = normalizedBranches
		combinedDomain := branchDomains[0]
		for _, branchDomain := range branchDomains[1:] {
			if keyword == "allOf" {
				combinedDomain = intersectGrokSchemaDomain(combinedDomain, branchDomain)
			} else {
				combinedDomain.mask |= branchDomain.mask
				combinedDomain.explicitNonObject = combinedDomain.explicitNonObject || branchDomain.explicitNonObject
			}
		}
		domain = intersectGrokSchemaDomain(domain, combinedDomain)
		if keyword != "allOf" && domain.mask&grokSchemaObject != 0 && !domain.explicitNonObject && domain.mask&grokSchemaNull == 0 {
			node["type"] = "object"
			state.union = true
		}
	}
	if domain.mask == 0 {
		return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "unsatisfiable_root"}
	}

	if types, exists := node["type"]; exists {
		pruned, prunedValue, pruneErr := pruneGrokRootNullType(types)
		if pruneErr != nil {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".type", Reason: "malformed_type"}
		}
		if pruned {
			node["type"] = prunedValue
			state.nullPruned = true
			domain.mask &^= grokSchemaNull
		}
	}
	if enum, exists := node["enum"]; exists {
		values, ok := enum.([]any)
		if !ok || len(values) == 0 {
			return nil, grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".enum", Reason: "malformed_enum"}
		}
		filtered := make([]any, 0, len(values))
		removedNull := false
		for _, value := range values {
			if value == nil {
				removedNull = true
				continue
			}
			filtered = append(filtered, value)
		}
		if removedNull && len(filtered) > 0 {
			node["enum"] = filtered
			state.nullPruned = true
			domain.mask &^= grokSchemaNull
		}
	}
	if domain.mask&grokSchemaObject != 0 && !domain.explicitNonObject {
		if existingType, exists := node["type"]; !exists {
			node["type"] = "object"
			state.canonicalized = true
		} else if domain.mask == grokSchemaObject {
			typeDomain, typeErr := grokSchemaTypeDomain(existingType)
			if typeErr == nil && typeDomain.mask != grokSchemaObject {
				node["type"] = "object"
				state.canonicalized = true
			}
		}
		domain.mask = grokSchemaObject
	}
	return node, domain, nil
}

func grokSchemaDirectDomain(node map[string]any, path string) (grokRootSchemaDomain, error) {
	domain := grokRootSchemaDomain{mask: grokSchemaAll}
	if rawType, exists := node["type"]; exists {
		typeDomain, err := grokSchemaTypeDomain(rawType)
		if err != nil {
			return grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".type", Reason: "malformed_type"}
		}
		domain = intersectGrokSchemaDomain(domain, typeDomain)
	}
	if value, exists := node["const"]; exists {
		domain = intersectGrokSchemaDomain(domain, grokSchemaValueDomain(value))
	}
	if rawEnum, exists := node["enum"]; exists {
		values, ok := rawEnum.([]any)
		if !ok || len(values) == 0 {
			return grokRootSchemaDomain{}, &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path + ".enum", Reason: "malformed_enum"}
		}
		enumDomain := grokRootSchemaDomain{}
		for _, value := range values {
			valueDomain := grokSchemaValueDomain(value)
			enumDomain.mask |= valueDomain.mask
			enumDomain.explicitNonObject = enumDomain.explicitNonObject || valueDomain.explicitNonObject
		}
		domain = intersectGrokSchemaDomain(domain, enumDomain)
	}
	return domain, nil
}

func grokSchemaTypeDomain(value any) (grokRootSchemaDomain, error) {
	var values []string
	switch typed := value.(type) {
	case string:
		values = []string{typed}
	case []any:
		if len(typed) == 0 {
			return grokRootSchemaDomain{}, fmt.Errorf("empty type")
		}
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return grokRootSchemaDomain{}, fmt.Errorf("non-string type")
			}
			values = append(values, name)
		}
	default:
		return grokRootSchemaDomain{}, fmt.Errorf("invalid type")
	}
	domain := grokRootSchemaDomain{}
	for _, name := range values {
		switch name {
		case "object":
			domain.mask |= grokSchemaObject
		case "null":
			domain.mask |= grokSchemaNull
		case "string", "array", "number", "integer", "boolean":
			domain.mask |= grokSchemaNonObject
			domain.explicitNonObject = true
		default:
			return grokRootSchemaDomain{}, fmt.Errorf("unknown type")
		}
	}
	return domain, nil
}

func grokSchemaValueDomain(value any) grokRootSchemaDomain {
	switch value.(type) {
	case nil:
		return grokRootSchemaDomain{mask: grokSchemaNull}
	case map[string]any:
		return grokRootSchemaDomain{mask: grokSchemaObject}
	default:
		return grokRootSchemaDomain{mask: grokSchemaNonObject, explicitNonObject: true}
	}
}

func intersectGrokSchemaDomain(left, right grokRootSchemaDomain) grokRootSchemaDomain {
	return grokRootSchemaDomain{
		mask:              left.mask & right.mask,
		explicitNonObject: (left.explicitNonObject || right.explicitNonObject) && left.mask&right.mask&grokSchemaNonObject != 0,
	}
}

func pruneGrokRootNullType(value any) (bool, any, error) {
	types, err := grokSchemaTypeDomain(value)
	if err != nil {
		return false, nil, err
	}
	if types.mask&grokSchemaNull == 0 || types.mask == grokSchemaNull {
		return false, value, nil
	}
	var names []string
	switch typed := value.(type) {
	case string:
		return false, typed, nil
	case []any:
		for _, item := range typed {
			name := item.(string)
			if name != "null" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 1 {
		return true, names[0], nil
	}
	result := make([]any, len(names))
	for index, name := range names {
		result[index] = name
	}
	return true, result, nil
}

func combineGrokRootSchemaNode(primary any, siblings map[string]any) map[string]any {
	if len(siblings) == 0 {
		if object, ok := primary.(map[string]any); ok {
			return object
		}
	}
	return map[string]any{"allOf": []any{primary, siblings}}
}

func (b *grokSchemaCompatibilityBudget) consumeWork(path string, depth int) error {
	if b == nil {
		return nil
	}
	if b.limits.maxDepth > 0 && depth > b.limits.maxDepth {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_depth_limit_exceeded"}
	}
	b.work++
	if b.limits.maxWork > 0 && b.work > b.limits.maxWork {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_work_limit_exceeded"}
	}
	b.producedNodes++
	b.producedBytes += 16
	if b.limits.maxProducedNodes > 0 && b.producedNodes > b.limits.maxProducedNodes {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_node_limit_exceeded"}
	}
	if b.limits.maxProducedBytes > 0 && b.producedBytes > b.limits.maxProducedBytes {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_produced_size_limit_exceeded"}
	}
	return nil
}

func (b *grokSchemaCompatibilityBudget) consumeRef(path string) error {
	if b == nil {
		return nil
	}
	b.refs++
	if b.limits.maxRefs > 0 && b.refs > b.limits.maxRefs {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_ref_limit_exceeded"}
	}
	return nil
}

func (r grokSchemaResolver) reserveRefExpansion(ref string, target any, path string) error {
	if r.budget == nil {
		return nil
	}
	stats, cached := r.statsCache[ref]
	if !cached {
		stats = measureGrokSchemaValue(target)
		r.statsCache[ref] = stats
	}
	nextNodes := r.budget.producedNodes + stats.nodes
	nextBytes := r.budget.producedBytes + stats.bytes
	if r.budget.limits.maxProducedNodes > 0 && nextNodes > r.budget.limits.maxProducedNodes {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_node_limit_exceeded"}
	}
	if r.budget.limits.maxProducedBytes > 0 && nextBytes > r.budget.limits.maxProducedBytes {
		return &GrokResponsesCompatibilityError{Code: "invalid_client_tool_schema", Path: path, Reason: "compatibility_produced_size_limit_exceeded"}
	}
	r.budget.producedNodes = nextNodes
	r.budget.producedBytes = nextBytes
	return nil
}

func measureGrokSchemaValue(value any) grokSchemaValueStats {
	stats := grokSchemaValueStats{}
	stack := []any{value}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		stats.nodes++
		switch typed := current.(type) {
		case map[string]any:
			stats.bytes += 2
			for key, child := range typed {
				stats.bytes += len(key) + 4
				stack = append(stack, child)
			}
		case []any:
			stats.bytes += 2 + len(typed)
			stack = append(stack, typed...)
		case string:
			stats.bytes += len(typed) + 2
		case json.Number:
			stats.bytes += len(typed.String())
		case bool:
			stats.bytes += 5
		case nil:
			stats.bytes += 4
		default:
			stats.bytes += 16
		}
	}
	if encoded, err := json.Marshal(value); err == nil {
		stats.bytes = len(encoded)
	}
	return stats
}

func (r grokSchemaResolver) resolvePointer(ref string) (any, error) {
	if cached, ok := r.pointerCache[ref]; ok {
		return cached, nil
	}
	if ref == "#" {
		r.pointerCache[ref] = r.document
		return r.document, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("malformed_ref")
	}
	var current any = r.document
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token, err := decodeGrokJSONPointerToken(encoded)
		if err != nil {
			return nil, fmt.Errorf("malformed_ref")
		}
		switch typed := current.(type) {
		case map[string]any:
			value, exists := typed[token]
			if !exists {
				return nil, fmt.Errorf("missing_ref")
			}
			current = value
		case []any:
			index, parseErr := strconv.Atoi(token)
			if parseErr != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("missing_ref")
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("missing_ref")
		}
	}
	r.pointerCache[ref] = current
	return current, nil
}

func decodeGrokJSONPointerToken(encoded string) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(encoded); index++ {
		if encoded[index] != '~' {
			builder.WriteByte(encoded[index])
			continue
		}
		if index+1 >= len(encoded) {
			return "", fmt.Errorf("invalid escape")
		}
		index++
		switch encoded[index] {
		case '0':
			builder.WriteByte('~')
		case '1':
			builder.WriteByte('/')
		default:
			return "", fmt.Errorf("invalid escape")
		}
	}
	return builder.String(), nil
}

func cloneGrokJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneGrokJSONValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneGrokJSONValue(item)
		}
		return cloned
	default:
		return typed
	}
}

func isGrokSchemaAnnotationKeyword(keyword string) bool {
	switch keyword {
	case "title", "description", "default", "deprecated", "readOnly", "writeOnly", "examples", "$comment":
		return true
	default:
		return false
	}
}

func permissiveGrokObjectSchema() []byte {
	return []byte(`{"type":"object","properties":{},"additionalProperties":true}`)
}

func grokSchemaFingerprint(raw []byte) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	canonical := raw
	if decoder.Decode(&value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			canonical = encoded
		}
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func grokMissingParametersFingerprint() string {
	sum := sha256.Sum256([]byte(grokMissingParametersFingerprintSentinel))
	return hex.EncodeToString(sum[:])
}

func compatibilityErrorReason(err error) string {
	if err == nil {
		return ""
	}
	var compatibilityErr *GrokResponsesCompatibilityError
	if errors.As(err, &compatibilityErr) {
		return compatibilityErr.Reason
	}
	return "unknown"
}

func canonicalGrokCompatibilityMetricReason(reason string) string {
	switch reason {
	case "mixed_non_object_root", "non_object_root", "null_only_root",
		"invalid_json", "malformed_object", "expected_string", "expected_array", "unsupported_content_type",
		"missing_parameters", "null_parameters", "malformed_schema", "false_schema", "expected_schema_object", "malformed_schema_node", "dynamic_ref_unsupported",
		"schema_scope_unsupported", "malformed_ref", "external_ref_unsupported", "cyclic_ref", "missing_ref",
		"malformed_combinator", "malformed_type", "malformed_enum", "unsatisfiable_root", "encode_failed",
		"compatibility_input_size_limit_exceeded", "compatibility_depth_limit_exceeded", "compatibility_work_limit_exceeded",
		"compatibility_ref_limit_exceeded", "compatibility_node_limit_exceeded", "compatibility_produced_size_limit_exceeded",
		"compatibility_output_size_limit_exceeded":
		return reason
	default:
		return "other"
	}
}

func grokCompatibilitySizeDeltaBucket(delta int) string {
	switch {
	case delta < 0:
		return "decrease"
	case delta == 0:
		return "zero"
	case delta <= 256:
		return "increase_le_256"
	case delta <= 1024:
		return "increase_le_1024"
	case delta <= 4096:
		return "increase_le_4096"
	default:
		return "increase_gt_4096"
	}
}

func observeGrokResponsesProtocolCompatibility(c *gin.Context, transport string, report GrokResponsesCompatibilityReport, err error) {
	grokResponsesCompatibilityMetrics.mu.Lock()
	recordCompatibilityCount := func(kind, outcome string, count int) {
		if count > 0 {
			grokResponsesCompatibilityMetrics.total[kind+":"+outcome] += uint64(count)
		}
	}
	recordCompatibilityCount("agent", "plaintext", report.AgentPlaintext)
	recordCompatibilityCount("agent", "encrypted_tagged", report.AgentEncrypted)
	recordCompatibilityCount("agent", "mixed", report.AgentMixed)
	recordCompatibilityCount("agent", "empty", report.AgentEmpty)
	recordCompatibilityCount("agent", "error", report.AgentErrors)
	recordCompatibilityCount("schema", "unchanged", report.SchemaUnchanged)
	recordCompatibilityCount("schema", "inlined", report.SchemaInlined)
	recordCompatibilityCount("schema", "null_pruned", report.SchemaNullPruned)
	recordCompatibilityCount("schema", "object_union", report.SchemaObjectUnion)
	recordCompatibilityCount("schema", "canonicalized", report.SchemaCanonicalized)
	recordCompatibilityCount("schema", "fallback", report.SchemaFallback)
	recordCompatibilityCount("schema", "error", report.SchemaErrors)
	for _, schema := range report.Schemas {
		if schema.Outcome == "error" {
			continue
		}
		grokResponsesCompatibilityMetrics.sizeDeltaCount++
		grokResponsesCompatibilityMetrics.sizeDeltaTotal += int64(schema.SizeDeltaByte)
		grokResponsesCompatibilityMetrics.sizeBuckets[grokCompatibilitySizeDeltaBucket(schema.SizeDeltaByte)]++
		if schema.Outcome == "fallback" {
			grokResponsesCompatibilityMetrics.fallbackReason[canonicalGrokCompatibilityMetricReason(schema.Reason)]++
		}
	}
	if errReason := compatibilityErrorReason(err); errReason != "" {
		grokResponsesCompatibilityMetrics.errorReason[canonicalGrokCompatibilityMetricReason(errReason)]++
	}
	grokResponsesCompatibilityMetrics.mu.Unlock()

	requestID := ""
	if c != nil {
		requestID = c.GetString("request_id")
	}
	slog.Info("grok_responses_protocol_compatibility",
		"version", grokResponsesProtocolCompatibilityVersion,
		"transport", transport,
		"request_id", requestID,
		"agent_items", report.AgentItems,
		"agent_parts", report.AgentParts,
		"agent_plaintext", report.AgentPlaintext,
		"agent_encrypted_tagged", report.AgentEncrypted,
		"agent_mixed", report.AgentMixed,
		"agent_empty", report.AgentEmpty,
		"agent_error", report.AgentErrors,
		"schema_unchanged", report.SchemaUnchanged,
		"schema_inlined", report.SchemaInlined,
		"schema_null_pruned", report.SchemaNullPruned,
		"schema_object_union", report.SchemaObjectUnion,
		"schema_canonicalized", report.SchemaCanonicalized,
		"schema_fallback", report.SchemaFallback,
		"schema_error", report.SchemaErrors,
		"error_reason", compatibilityErrorReason(err),
	)
	for _, schema := range report.Schemas {
		if schema.Outcome != "fallback" && schema.Outcome != "error" {
			continue
		}
		slog.Info("grok_responses_protocol_compatibility_schema",
			"version", grokResponsesProtocolCompatibilityVersion,
			"transport", transport,
			"request_id", requestID,
			"tool_index", schema.ToolIndex,
			"tool_name_sha256", grokToolNameFingerprint(schema.ToolName),
			"outcome", schema.Outcome,
			"reason", schema.Reason,
			"schema_sha256", schema.Fingerprint,
			"strict_before", schema.StrictBefore,
			"strict_after", schema.StrictAfter,
			"size_delta_bytes", schema.SizeDeltaByte,
		)
	}
}

func grokToolNameFingerprint(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}
