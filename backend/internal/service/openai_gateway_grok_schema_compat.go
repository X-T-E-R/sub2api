package service

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
)

type grokSchemaDisposition uint8

const (
	grokSchemaUnknown grokSchemaDisposition = iota
	grokSchemaProvenObject
	grokSchemaContradictory
)

type grokSchemaBudget struct {
	MaxSize      int
	MaxDepth     int
	MaxRefVisits int
	refVisits    int
}

var defaultGrokSchemaBudget = grokSchemaBudget{
	MaxSize:      openAIResponsesObjectUnionMaxSize,
	MaxDepth:     openAIResponsesObjectUnionMaxDepth,
	MaxRefVisits: 128,
}

var grokPermissiveFunctionParameters = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{},
	"additionalProperties": true,
}

var grokSchemaRootKeywords = map[string]struct{}{
	"type": {}, "const": {}, "enum": {}, "$ref": {}, "$defs": {}, "definitions": {},
	"properties": {}, "required": {}, "additionalProperties": {}, "minProperties": {},
	"maxProperties": {}, "allOf": {}, "anyOf": {}, "oneOf": {},
}

var grokSchemaAnnotationKeywords = map[string]struct{}{
	"title": {}, "description": {}, "default": {}, "examples": {}, "deprecated": {},
	"readOnly": {}, "writeOnly": {}, "format": {}, "$comment": {}, "$schema": {},
}

func normalizeGrokFunctionParameters(raw json.RawMessage) (json.RawMessage, grokSchemaDisposition) {
	budget := defaultGrokSchemaBudget
	return normalizeGrokFunctionParametersWithBudget(raw, &budget)
}

func normalizeGrokFunctionParametersWithBudget(raw json.RawMessage, budget *grokSchemaBudget) (json.RawMessage, grokSchemaDisposition) {
	if budget == nil {
		copy := defaultGrokSchemaBudget
		budget = &copy
	}
	if budget.MaxSize <= 0 || budget.MaxDepth < 0 || budget.MaxRefVisits <= 0 || len(raw) > budget.MaxSize {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}
	if !grokSchemaValueWithinDepth(value, budget.MaxDepth) {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}

	if truth, ok := value.(bool); ok {
		if truth {
			return grokSchemaFallbackRaw(), grokSchemaUnknown
		}
		return grokSchemaFallbackRaw(), grokSchemaContradictory
	}
	root, ok := value.(map[string]any)
	if !ok {
		return grokSchemaFallbackRaw(), grokSchemaContradictory
	}
	rootDocument := cloneGrokSchemaMap(root)
	normalized, disposition := normalizeGrokSchemaMap(root, rootDocument, budget, 0, map[string]struct{}{})
	if disposition != grokSchemaProvenObject {
		return grokSchemaFallbackRaw(), disposition
	}
	if !grokSchemaValueWithinDepth(normalized, budget.MaxDepth) {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded) > budget.MaxSize {
		return grokSchemaFallbackRaw(), grokSchemaUnknown
	}
	return encoded, grokSchemaProvenObject
}

func grokSchemaValueWithinDepth(value any, maxDepth int) bool {
	type pendingValue struct {
		value any
		depth int
	}
	stack := []pendingValue{{value: value, depth: 0}}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		switch typed := current.value.(type) {
		case map[string]any:
			if current.depth > maxDepth {
				return false
			}
			for _, child := range typed {
				switch child.(type) {
				case map[string]any, []any:
					stack = append(stack, pendingValue{value: child, depth: current.depth + 1})
				}
			}
		case []any:
			if current.depth > maxDepth {
				return false
			}
			for _, child := range typed {
				switch child.(type) {
				case map[string]any, []any:
					stack = append(stack, pendingValue{value: child, depth: current.depth + 1})
				}
			}
		}
	}
	return true
}

func grokSchemaFallbackRaw() json.RawMessage {
	encoded, _ := json.Marshal(grokPermissiveFunctionParameters)
	return encoded
}

func normalizeGrokSchemaMap(
	schema map[string]any,
	rootDocument map[string]any,
	budget *grokSchemaBudget,
	depth int,
	refStack map[string]struct{},
) (map[string]any, grokSchemaDisposition) {
	if depth > budget.MaxDepth {
		return nil, grokSchemaUnknown
	}
	for key := range schema {
		if _, ok := grokSchemaRootKeywords[key]; ok {
			continue
		}
		if _, ok := grokSchemaAnnotationKeywords[key]; ok || strings.HasPrefix(key, "x-") {
			continue
		}
		return nil, grokSchemaUnknown
	}

	working := cloneGrokSchemaMap(schema)
	rootObjectProven := false
	if dialect, exists := working["$schema"]; exists {
		if _, ok := dialect.(string); !ok {
			return nil, grokSchemaUnknown
		}
		delete(working, "$schema")
	}
	if rawRef, exists := working["$ref"]; exists {
		ref, ok := rawRef.(string)
		if !ok || !strings.HasPrefix(ref, "#/") || budget.refVisits >= budget.MaxRefVisits {
			return nil, grokSchemaUnknown
		}
		if len(strings.Split(strings.TrimPrefix(ref, "#/"), "/")) > budget.MaxDepth {
			return nil, grokSchemaUnknown
		}
		if _, cyclic := refStack[ref]; cyclic {
			return nil, grokSchemaUnknown
		}
		budget.refVisits++
		target, ok := resolveGrokLocalSchemaRef(rootDocument, ref)
		if !ok {
			return nil, grokSchemaUnknown
		}
		if targetBool, booleanSchema := target.(bool); booleanSchema {
			if !targetBool {
				return nil, grokSchemaContradictory
			}
			delete(working, "$ref")
		} else {
			targetMap, ok := target.(map[string]any)
			if !ok {
				return nil, grokSchemaUnknown
			}
			nextStack := cloneGrokRefStack(refStack)
			nextStack[ref] = struct{}{}
			normalizedTarget, disposition := normalizeGrokSchemaMap(targetMap, rootDocument, budget, depth+1, nextStack)
			if disposition != grokSchemaProvenObject {
				return nil, disposition
			}
			delete(working, "$ref")
			merged, ok := intersectGrokSchemaMaps(normalizedTarget, working)
			if !ok {
				if grokSchemaMapsProveContradiction(normalizedTarget, working) {
					return nil, grokSchemaContradictory
				}
				return nil, grokSchemaUnknown
			}
			working = merged
		}
	}

	if rawAllOf, exists := working["allOf"]; exists {
		branches, ok := rawAllOf.([]any)
		if !ok || len(branches) == 0 {
			return nil, grokSchemaUnknown
		}
		objectBranches := make([]any, 0, len(branches))
		expandedSize := 0
		for _, branch := range branches {
			normalizedBranch, disposition := normalizeGrokRootObjectBranch(branch, rootDocument, budget, depth+1, refStack)
			if disposition != grokSchemaProvenObject {
				return nil, grokSchemaUnknown
			}
			if objectBranches, ok = appendGrokRootObjectBranchWithinBudget(objectBranches, normalizedBranch, &expandedSize, budget.MaxSize); !ok {
				return nil, grokSchemaUnknown
			}
		}
		working["allOf"] = objectBranches
		rootObjectProven = true
	}

	if rawAnyOf, exists := working["anyOf"]; exists {
		branches, ok := rawAnyOf.([]any)
		if !ok || len(branches) == 0 {
			return nil, grokSchemaUnknown
		}
		objectBranches := make([]any, 0, len(branches))
		expandedSize := 0
		for _, branch := range branches {
			normalizedBranch, disposition := normalizeGrokRootObjectBranch(branch, rootDocument, budget, depth+1, refStack)
			if disposition != grokSchemaProvenObject {
				return nil, grokSchemaUnknown
			}
			if objectBranches, ok = appendGrokRootObjectBranchWithinBudget(objectBranches, normalizedBranch, &expandedSize, budget.MaxSize); !ok {
				return nil, grokSchemaUnknown
			}
		}
		if len(objectBranches) == 0 {
			return nil, grokSchemaContradictory
		}
		working["anyOf"] = objectBranches
		rootObjectProven = true
	}

	if rawOneOf, exists := working["oneOf"]; exists {
		branches, ok := rawOneOf.([]any)
		if !ok || len(branches) == 0 {
			return nil, grokSchemaUnknown
		}
		allObject := true
		_, hasRootConst := working["const"]
		_, hasRootEnum := working["enum"]
		finiteWitnessGrammar := hasRootConst || hasRootEnum
		objectBranches := make([]any, 0, len(branches))
		expandedSize := 0
		for _, branch := range branches {
			normalizedBranch, disposition := normalizeGrokRootObjectBranch(branch, rootDocument, budget, depth+1, refStack)
			if disposition != grokSchemaProvenObject {
				allObject = false
			} else {
				if objectBranches, ok = appendGrokRootObjectBranchWithinBudget(objectBranches, normalizedBranch, &expandedSize, budget.MaxSize); !ok {
					return nil, grokSchemaUnknown
				}
			}
			if branchMap, ok := branch.(map[string]any); ok && len(grokFiniteObjectWitnesses(branchMap)) > 0 {
				finiteWitnessGrammar = true
			}
		}
		if !allObject {
			return nil, grokSchemaUnknown
		}
		if finiteWitnessGrammar {
			rootWitnesses := grokFiniteObjectWitnesses(working)
			_, rootConstFinite := working["const"]
			_, rootEnumFinite := working["enum"]
			rootFinite := rootConstFinite || rootEnumFinite
			branchesFinite := true
			witnesses := rootWitnesses
			for _, branch := range branches {
				branchMap, ok := branch.(map[string]any)
				if !ok {
					return nil, grokSchemaUnknown
				}
				branchWitnesses := grokFiniteObjectWitnesses(branchMap)
				branchesFinite = branchesFinite && len(branchWitnesses) > 0
				if !rootFinite {
					witnesses = append(witnesses, branchWitnesses...)
				}
			}
			provenExclusive := false
			unknownWitness := false
			siblings := cloneGrokSchemaMap(working)
			delete(siblings, "oneOf")
			for _, witness := range uniqueGrokObjectWitnesses(witnesses) {
				siblingDisposition := validateGrokObjectValue(witness, siblings, rootDocument, budget, depth+1, "")
				if siblingDisposition == grokSchemaUnknown {
					unknownWitness = true
					continue
				}
				if siblingDisposition == grokSchemaContradictory {
					continue
				}
				matches := 0
				unknown := false
				for _, branch := range branches {
					result := validateGrokSchemaValue(witness, branch, rootDocument, budget, depth+1)
					switch result {
					case grokSchemaProvenObject:
						matches++
					case grokSchemaUnknown:
						unknown = true
					}
				}
				if !unknown && matches == 1 {
					provenExclusive = true
					break
				}
				unknownWitness = unknownWitness || unknown
			}
			if !provenExclusive {
				if (rootFinite || branchesFinite) && !unknownWitness {
					return nil, grokSchemaContradictory
				}
				return nil, grokSchemaUnknown
			}
		}
		working["oneOf"] = objectBranches
		rootObjectProven = true
	}

	if rawConst, exists := working["const"]; exists {
		_, constObject := rawConst.(map[string]any)
		if !constObject {
			return nil, grokSchemaContradictory
		}
		rootObjectProven = rootObjectProven || constObject
	}
	if rawEnum, exists := working["enum"]; exists {
		if values, ok := rawEnum.([]any); ok && len(values) > 0 {
			objectCount := 0
			for _, value := range values {
				if _, object := value.(map[string]any); object {
					objectCount++
				}
			}
			if objectCount == 0 {
				return nil, grokSchemaContradictory
			}
			if objectCount == len(values) {
				rootObjectProven = true
			} else if _, hasExplicitType := working["type"]; !hasExplicitType && !rootObjectProven {
				return nil, grokSchemaUnknown
			}
		}
	}
	if _, hasExplicitType := working["type"]; hasExplicitType {
		typeDisposition := normalizeGrokObjectRootType(working)
		if typeDisposition != grokSchemaProvenObject {
			return nil, typeDisposition
		}
	} else if !rootObjectProven {
		return nil, grokSchemaUnknown
	} else {
		working["type"] = "object"
	}
	if disposition := validateGrokObjectShape(working, rootDocument, budget, depth); disposition != grokSchemaProvenObject {
		return nil, disposition
	}

	if rawConst, exists := working["const"]; exists {
		if _, ok := rawConst.(map[string]any); !ok {
			return nil, grokSchemaContradictory
		}
		if disposition := validateGrokObjectValue(rawConst.(map[string]any), working, rootDocument, budget, depth+1, "const"); disposition != grokSchemaProvenObject {
			return nil, disposition
		}
	}
	if rawEnum, exists := working["enum"]; exists {
		values, ok := rawEnum.([]any)
		if !ok {
			return nil, grokSchemaUnknown
		}
		if len(values) == 0 {
			return nil, grokSchemaContradictory
		}
		filtered := make([]any, 0, len(values))
		for _, value := range values {
			objectValue, ok := value.(map[string]any)
			if !ok {
				continue
			}
			disposition := validateGrokObjectValue(objectValue, working, rootDocument, budget, depth+1, "enum")
			if disposition == grokSchemaUnknown {
				return nil, disposition
			}
			if disposition == grokSchemaProvenObject {
				filtered = append(filtered, value)
			}
		}
		if len(filtered) == 0 {
			return nil, grokSchemaContradictory
		}
		working["enum"] = filtered
	}
	working["type"] = "object"
	return working, grokSchemaProvenObject
}

func normalizeGrokObjectRootType(schema map[string]any) grokSchemaDisposition {
	rawType, exists := schema["type"]
	if !exists {
		return grokSchemaUnknown
	}
	switch value := rawType.(type) {
	case string:
		if value == "object" {
			return grokSchemaProvenObject
		}
		return grokSchemaContradictory
	case []any:
		if len(value) == 0 {
			return grokSchemaUnknown
		}
		hasObject := false
		for _, raw := range value {
			typeName, ok := raw.(string)
			if !ok {
				return grokSchemaUnknown
			}
			switch typeName {
			case "object":
				hasObject = true
			default:
				return grokSchemaUnknown
			}
		}
		if !hasObject {
			return grokSchemaContradictory
		}
		schema["type"] = "object"
		return grokSchemaProvenObject
	default:
		return grokSchemaUnknown
	}
}

func normalizeGrokRootObjectBranch(
	schema any,
	rootDocument map[string]any,
	budget *grokSchemaBudget,
	depth int,
	refStack map[string]struct{},
) (map[string]any, grokSchemaDisposition) {
	if depth > budget.MaxDepth {
		return nil, grokSchemaUnknown
	}
	if truth, ok := schema.(bool); ok {
		if truth {
			return nil, grokSchemaUnknown
		}
		return nil, grokSchemaContradictory
	}
	branch, ok := schema.(map[string]any)
	if !ok {
		return nil, grokSchemaUnknown
	}
	// Re-enter at the branch root so only root-reachable refs/combinators are
	// expanded. Property schemas and retained definitions remain untouched.
	return normalizeGrokSchemaMap(branch, rootDocument, budget, depth, refStack)
}

func appendGrokRootObjectBranchWithinBudget(branches []any, branch map[string]any, expandedSize *int, maxSize int) ([]any, bool) {
	encoded, err := json.Marshal(branch)
	if err != nil || expandedSize == nil || *expandedSize > maxSize-len(encoded) {
		return branches, false
	}
	*expandedSize += len(encoded)
	return append(branches, branch), true
}

func validateGrokObjectShape(schema map[string]any, root map[string]any, budget *grokSchemaBudget, depth int) grokSchemaDisposition {
	properties := map[string]any{}
	if raw, exists := schema["properties"]; exists {
		var ok bool
		properties, ok = raw.(map[string]any)
		if !ok {
			return grokSchemaUnknown
		}
	}
	required := []string{}
	if raw, exists := schema["required"]; exists {
		items, ok := raw.([]any)
		if !ok {
			return grokSchemaUnknown
		}
		seen := map[string]struct{}{}
		for _, item := range items {
			name, ok := item.(string)
			if !ok {
				return grokSchemaUnknown
			}
			if _, duplicate := seen[name]; duplicate {
				return grokSchemaUnknown
			}
			seen[name] = struct{}{}
			required = append(required, name)
		}
	}
	minProperties, minSet, ok := grokSchemaBound(schema, "minProperties")
	if !ok {
		return grokSchemaUnknown
	}
	maxProperties, maxSet, ok := grokSchemaBound(schema, "maxProperties")
	if !ok {
		return grokSchemaUnknown
	}
	if minSet && maxSet && minProperties > maxProperties || maxSet && len(required) > maxProperties {
		return grokSchemaContradictory
	}
	additional := any(true)
	if raw, exists := schema["additionalProperties"]; exists {
		additional = raw
		switch raw.(type) {
		case bool, map[string]any:
		default:
			return grokSchemaUnknown
		}
	}
	if additional == false && minSet && len(properties) < minProperties {
		return grokSchemaContradictory
	}
	for _, name := range required {
		propertySchema, exists := properties[name]
		if !exists {
			if additional == false {
				return grokSchemaContradictory
			}
			propertySchema = additional
		}
		if impossible, booleanSchema := propertySchema.(bool); booleanSchema && !impossible {
			return grokSchemaContradictory
		}
		if disposition := grokRequiredChildFiniteDisposition(propertySchema); disposition != grokSchemaProvenObject {
			return disposition
		}
	}
	return grokSchemaProvenObject
}

func grokRequiredChildFiniteDisposition(schema any) grokSchemaDisposition {
	child, ok := schema.(map[string]any)
	if !ok {
		return grokSchemaProvenObject
	}
	rawType, hasType := child["type"]
	if !hasType {
		return grokSchemaProvenObject
	}
	if rawConst, hasConst := child["const"]; hasConst {
		return grokValueMatchesDeclaredType(rawConst, rawType)
	}
	if rawEnum, hasEnum := child["enum"]; hasEnum {
		values, ok := rawEnum.([]any)
		if !ok {
			return grokSchemaUnknown
		}
		if len(values) == 0 {
			return grokSchemaContradictory
		}
		unknown := false
		for _, value := range values {
			switch grokValueMatchesDeclaredType(value, rawType) {
			case grokSchemaProvenObject:
				return grokSchemaProvenObject
			case grokSchemaUnknown:
				unknown = true
			}
		}
		if unknown {
			return grokSchemaUnknown
		}
		return grokSchemaContradictory
	}
	return grokSchemaProvenObject
}

func validateGrokSchemaValue(value map[string]any, rawSchema any, root map[string]any, budget *grokSchemaBudget, depth int) grokSchemaDisposition {
	schema, ok := rawSchema.(map[string]any)
	if !ok {
		if truth, boolean := rawSchema.(bool); boolean && truth {
			return grokSchemaProvenObject
		}
		return grokSchemaContradictory
	}
	for key := range schema {
		if _, supported := grokSchemaRootKeywords[key]; !supported {
			if _, annotation := grokSchemaAnnotationKeywords[key]; !annotation && !strings.HasPrefix(key, "x-") {
				return grokSchemaUnknown
			}
		}
	}
	if rawRef, exists := schema["$ref"]; exists {
		ref, ok := rawRef.(string)
		if !ok || !strings.HasPrefix(ref, "#/") || depth > budget.MaxDepth || budget.refVisits >= budget.MaxRefVisits {
			return grokSchemaUnknown
		}
		budget.refVisits++
		target, ok := resolveGrokLocalSchemaRef(root, ref)
		if !ok {
			return grokSchemaUnknown
		}
		targetDisposition := validateGrokSchemaValue(value, target, root, budget, depth+1)
		if targetDisposition == grokSchemaContradictory {
			return targetDisposition
		}
		siblings := cloneGrokSchemaMap(schema)
		delete(siblings, "$ref")
		siblingDisposition := validateGrokObjectValue(value, siblings, root, budget, depth, "")
		if siblingDisposition == grokSchemaContradictory {
			return siblingDisposition
		}
		if targetDisposition == grokSchemaUnknown || siblingDisposition == grokSchemaUnknown {
			return grokSchemaUnknown
		}
		return grokSchemaProvenObject
	}
	return validateGrokObjectValue(value, schema, root, budget, depth, "")
}

func validateGrokObjectValue(value map[string]any, schema map[string]any, root map[string]any, budget *grokSchemaBudget, depth int, skip string) grokSchemaDisposition {
	if depth > budget.MaxDepth {
		return grokSchemaUnknown
	}
	if rawType, exists := schema["type"]; exists && grokValueMatchesDeclaredType(value, rawType) != grokSchemaProvenObject {
		return grokSchemaContradictory
	}
	if skip != "const" {
		if rawConst, exists := schema["const"]; exists && !reflect.DeepEqual(value, rawConst) {
			return grokSchemaContradictory
		}
	}
	if skip != "enum" {
		if rawEnum, exists := schema["enum"]; exists {
			items, ok := rawEnum.([]any)
			if !ok {
				return grokSchemaUnknown
			}
			matched := false
			for _, item := range items {
				matched = matched || reflect.DeepEqual(value, item)
			}
			if !matched {
				return grokSchemaContradictory
			}
		}
	}
	min, minSet, ok := grokSchemaBound(schema, "minProperties")
	if !ok {
		return grokSchemaUnknown
	}
	max, maxSet, ok := grokSchemaBound(schema, "maxProperties")
	if !ok {
		return grokSchemaUnknown
	}
	if minSet && len(value) < min || maxSet && len(value) > max {
		return grokSchemaContradictory
	}
	properties := map[string]any{}
	if raw, exists := schema["properties"]; exists {
		var typed bool
		properties, typed = raw.(map[string]any)
		if !typed {
			return grokSchemaUnknown
		}
	}
	if rawRequired, exists := schema["required"]; exists {
		items, ok := rawRequired.([]any)
		if !ok {
			return grokSchemaUnknown
		}
		for _, item := range items {
			name, ok := item.(string)
			if !ok {
				return grokSchemaUnknown
			}
			if _, exists := value[name]; !exists {
				return grokSchemaContradictory
			}
		}
	}
	additional := any(true)
	if raw, exists := schema["additionalProperties"]; exists {
		additional = raw
	}
	for name, propertyValue := range value {
		propertySchema, exists := properties[name]
		if !exists {
			if additional == false {
				return grokSchemaContradictory
			}
			propertySchema = additional
		}
		if propertySchema == true {
			continue
		}
		if leaf, ok := propertySchema.(map[string]any); ok {
			if disposition := validateGrokLeafValue(propertyValue, leaf); disposition != grokSchemaProvenObject {
				return disposition
			}
			continue
		}
		if propertySchema == false {
			return grokSchemaContradictory
		}
		return grokSchemaUnknown
	}
	return validateGrokObjectCombinators(value, schema, root, budget, depth)
}

func validateGrokObjectCombinators(value map[string]any, schema map[string]any, root map[string]any, budget *grokSchemaBudget, depth int) grokSchemaDisposition {
	if rawAllOf, exists := schema["allOf"]; exists {
		branches, ok := rawAllOf.([]any)
		if !ok || len(branches) == 0 {
			return grokSchemaUnknown
		}
		for _, branch := range branches {
			disposition := validateGrokSchemaValue(value, branch, root, budget, depth+1)
			if disposition != grokSchemaProvenObject {
				return disposition
			}
		}
	}
	if rawAnyOf, exists := schema["anyOf"]; exists {
		branches, ok := rawAnyOf.([]any)
		if !ok || len(branches) == 0 {
			return grokSchemaUnknown
		}
		matches := 0
		unknown := false
		for _, branch := range branches {
			switch validateGrokSchemaValue(value, branch, root, budget, depth+1) {
			case grokSchemaProvenObject:
				matches++
			case grokSchemaUnknown:
				unknown = true
			}
		}
		if matches == 0 {
			if unknown {
				return grokSchemaUnknown
			}
			return grokSchemaContradictory
		}
	}
	if rawOneOf, exists := schema["oneOf"]; exists {
		branches, ok := rawOneOf.([]any)
		if !ok || len(branches) == 0 {
			return grokSchemaUnknown
		}
		matches := 0
		unknown := false
		for _, branch := range branches {
			switch validateGrokSchemaValue(value, branch, root, budget, depth+1) {
			case grokSchemaProvenObject:
				matches++
			case grokSchemaUnknown:
				unknown = true
			}
		}
		if unknown {
			return grokSchemaUnknown
		}
		if matches != 1 {
			return grokSchemaContradictory
		}
	}
	return grokSchemaProvenObject
}

func grokFiniteObjectWitnesses(schema map[string]any) []map[string]any {
	var witnesses []map[string]any
	if value, ok := schema["const"].(map[string]any); ok {
		witnesses = append(witnesses, value)
	}
	if values, ok := schema["enum"].([]any); ok {
		for _, value := range values {
			if object, ok := value.(map[string]any); ok {
				witnesses = append(witnesses, object)
			}
		}
	}
	return witnesses
}

func uniqueGrokObjectWitnesses(values []map[string]any) []map[string]any {
	seen := map[string]struct{}{}
	unique := make([]map[string]any, 0, len(values))
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		key := string(encoded)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func intersectGrokSchemaMaps(left, right map[string]any) (map[string]any, bool) {
	merged := cloneGrokSchemaMap(left)
	for key, rightValue := range right {
		leftValue, exists := merged[key]
		if !exists {
			merged[key] = rightValue
			continue
		}
		if reflect.DeepEqual(leftValue, rightValue) {
			continue
		}
		switch key {
		case "required":
			leftItems, leftOK := leftValue.([]any)
			rightItems, rightOK := rightValue.([]any)
			if !leftOK || !rightOK {
				return nil, false
			}
			seen := map[string]struct{}{}
			combined := make([]any, 0, len(leftItems)+len(rightItems))
			for _, item := range append(append([]any(nil), leftItems...), rightItems...) {
				name, ok := item.(string)
				if !ok {
					return nil, false
				}
				if _, duplicate := seen[name]; duplicate {
					continue
				}
				seen[name] = struct{}{}
				combined = append(combined, name)
			}
			merged[key] = combined
		case "properties", "$defs", "definitions":
			leftMap, leftOK := leftValue.(map[string]any)
			rightMap, rightOK := rightValue.(map[string]any)
			if !leftOK || !rightOK {
				return nil, false
			}
			combined := cloneGrokSchemaMap(leftMap)
			for name, value := range rightMap {
				if existing, exists := combined[name]; exists && !reflect.DeepEqual(existing, value) {
					return nil, false
				}
				combined[name] = value
			}
			merged[key] = combined
		case "minProperties":
			leftN, _, leftOK := grokNonNegativeSchemaInteger(leftValue)
			rightN, _, rightOK := grokNonNegativeSchemaInteger(rightValue)
			if !leftOK || !rightOK {
				return nil, false
			}
			if rightN > leftN {
				merged[key] = rightValue
			}
		case "maxProperties":
			leftN, _, leftOK := grokNonNegativeSchemaInteger(leftValue)
			rightN, _, rightOK := grokNonNegativeSchemaInteger(rightValue)
			if !leftOK || !rightOK {
				return nil, false
			}
			if rightN < leftN {
				merged[key] = rightValue
			}
		case "type":
			intersection, ok := intersectGrokSchemaTypes(leftValue, rightValue)
			if !ok {
				return nil, false
			}
			merged[key] = intersection
		case "const":
			return nil, false
		case "enum":
			leftItems, leftOK := leftValue.([]any)
			rightItems, rightOK := rightValue.([]any)
			if !leftOK || !rightOK {
				return nil, false
			}
			var intersection []any
			for _, l := range leftItems {
				for _, r := range rightItems {
					if reflect.DeepEqual(l, r) {
						intersection = append(intersection, l)
						break
					}
				}
			}
			merged[key] = intersection
		default:
			return nil, false
		}
	}
	return merged, true
}

func grokSchemaMapsProveContradiction(left, right map[string]any) bool {
	if leftType, leftOK := left["type"]; leftOK {
		if rightType, rightOK := right["type"]; rightOK {
			leftTypes, leftValid := grokSchemaTypeSet(leftType)
			rightTypes, rightValid := grokSchemaTypeSet(rightType)
			if leftValid && rightValid {
				for _, leftName := range leftTypes {
					for _, rightName := range rightTypes {
						if leftName == rightName {
							return false
						}
					}
				}
				return true
			}
		}
	}
	if leftConst, leftOK := left["const"]; leftOK {
		if rightConst, rightOK := right["const"]; rightOK && !reflect.DeepEqual(leftConst, rightConst) {
			return true
		}
	}
	return false
}

func intersectGrokSchemaTypes(left, right any) (any, bool) {
	leftTypes, ok := grokSchemaTypeSet(left)
	if !ok {
		return nil, false
	}
	rightTypes, ok := grokSchemaTypeSet(right)
	if !ok {
		return nil, false
	}
	var intersection []string
	for _, name := range leftTypes {
		for _, candidate := range rightTypes {
			if name == candidate {
				intersection = append(intersection, name)
			}
		}
	}
	if len(intersection) == 0 {
		return nil, false
	}
	if len(intersection) == 1 {
		return intersection[0], true
	}
	values := make([]any, len(intersection))
	for i := range intersection {
		values[i] = intersection[i]
	}
	return values, true
}

func grokSchemaTypeSet(raw any) ([]string, bool) {
	if value, ok := raw.(string); ok {
		return []string{value}, true
	}
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil, false
	}
	result := make([]string, 0, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		if !ok {
			return nil, false
		}
		result = append(result, value)
	}
	return result, true
}

func resolveGrokLocalSchemaRef(root map[string]any, ref string) (any, bool) {
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	if len(parts) < 2 || parts[0] != "$defs" && parts[0] != "definitions" {
		return nil, false
	}
	var current any = root
	for _, encoded := range parts {
		part := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func grokNonNegativeSchemaInteger(raw any) (int, bool, bool) {
	if raw == nil {
		return 0, true, false
	}
	number, ok := raw.(json.Number)
	if !ok {
		return 0, true, false
	}
	parsed, err := strconv.ParseInt(string(number), 10, 32)
	if err != nil || parsed < 0 {
		return 0, true, false
	}
	return int(parsed), true, true
}

func grokSchemaBound(schema map[string]any, key string) (int, bool, bool) {
	raw, exists := schema[key]
	if !exists {
		return 0, false, true
	}
	value, _, ok := grokNonNegativeSchemaInteger(raw)
	return value, true, ok
}

func validateGrokLeafValue(value any, schema map[string]any) grokSchemaDisposition {
	for key := range schema {
		switch key {
		case "type", "const", "enum", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly", "format", "$comment":
		default:
			if !strings.HasPrefix(key, "x-") {
				return grokSchemaUnknown
			}
		}
	}
	if rawType, exists := schema["type"]; exists {
		if disposition := grokValueMatchesDeclaredType(value, rawType); disposition != grokSchemaProvenObject {
			return disposition
		}
	}
	if rawConst, exists := schema["const"]; exists && !reflect.DeepEqual(rawConst, value) {
		return grokSchemaContradictory
	}
	if rawEnum, exists := schema["enum"]; exists {
		items, ok := rawEnum.([]any)
		if !ok || len(items) == 0 {
			return grokSchemaUnknown
		}
		for _, item := range items {
			if reflect.DeepEqual(item, value) {
				return grokSchemaProvenObject
			}
		}
		return grokSchemaContradictory
	}
	return grokSchemaProvenObject
}

func grokValueMatchesDeclaredType(value any, rawType any) grokSchemaDisposition {
	if rawType == nil {
		return grokSchemaProvenObject
	}
	types, ok := grokSchemaTypeSet(rawType)
	if !ok {
		return grokSchemaUnknown
	}
	for _, typeName := range types {
		matches := false
		switch typeName {
		case "null":
			matches = value == nil
		case "boolean":
			_, matches = value.(bool)
		case "string":
			_, matches = value.(string)
		case "number":
			_, matches = value.(json.Number)
		case "integer":
			if number, ok := value.(json.Number); ok {
				_, err := strconv.ParseInt(string(number), 10, 64)
				matches = err == nil
			}
		case "array":
			_, matches = value.([]any)
		case "object":
			_, matches = value.(map[string]any)
		default:
			return grokSchemaUnknown
		}
		if matches {
			return grokSchemaProvenObject
		}
	}
	return grokSchemaContradictory
}

func cloneGrokSchemaMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneGrokRefStack(source map[string]struct{}) map[string]struct{} {
	clone := make(map[string]struct{}, len(source)+1)
	for key := range source {
		clone[key] = struct{}{}
	}
	return clone
}
