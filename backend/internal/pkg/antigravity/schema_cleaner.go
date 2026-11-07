package antigravity

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// CleanJSONSchema 清理 JSON Schema，移除 Antigravity/Gemini 不支持的字段。
// 参考 Antigravity-Manager/src-tauri/src/proxy/common/json_schema.rs
// 并吸收 Magpie (github.com/yetone/magpie) internal/gateway/codeassist.go
// plainSchema / foldUnion 实现：
//   - 支持 JSON Pointer $ref（#/properties/...、#/$defs/Foo/properties/x、~0/~1）与按完整路径段的自引用环收敛
//   - 支持 boolean schema（true -> {}，false -> 过滤属性/空数组）
//   - 支持嵌套 allOf、allOf 内 $ref 分支及 required-only 分支，同时保持 required 顺序稳定
//   - 节点自身具形状（ownShape）时折叠 anyOf/oneOf 属性与公共 required，保留根/节点形态
//   - 任意深度 type=="array" 缺失 items 时补齐（含 prefixItems 同构提取），清理非对应类型的越界属性
//   - 过滤空白 enum 值并将非空 string const 转为 enum
//
// Magpie Attribution & License:
//
//	MIT License
//	Copyright (c) 2026 yetone
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE.
func CleanJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	// Keep an intact read-only copy of root (including $defs and definitions)
	// until recursive expansion finishes, so JSON Pointers into definitions
	// such as "#/$defs/Foo/properties/x" or "#/definitions/a~1b/properties/c~0d"
	// resolve their full path instead of falling back to the last segment name.
	root := deepCopy(schema).(map[string]any)
	defs := collectDefs(root)

	// localRef 解析 root 内的本地 $ref：支持 JSON Pointer ("#", "#/properties/...")
	// 以及 $defs / definitions 名称回退。
	localRef := func(ref string) (any, bool) {
		if rest, ok := strings.CutPrefix(ref, "#/"); ok {
			cur := any(root)
			for _, tok := range strings.Split(rest, "/") {
				tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
				switch m := cur.(type) {
				case map[string]any:
					cur = m[tok]
				case []any:
					n, err := strconv.Atoi(tok)
					if err != nil || n < 0 || n >= len(m) {
						return nil, false
					}
					cur = m[n]
				default:
					return nil, false
				}
				if cur == nil {
					d, ok := defs[ref[strings.LastIndex(ref, "/")+1:]]
					return d, ok
				}
			}
			return cur, true
		}
		if ref == "#" {
			return root, true
		}
		d, ok := defs[ref[strings.LastIndex(ref, "/")+1:]]
		return d, ok
	}

	resolve := func(v any) map[string]any {
		m, _ := v.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			if d, ok := localRef(ref); ok {
				if dm, ok := d.(map[string]any); ok {
					cp := deepCopy(dm).(map[string]any)
					for k, val := range m {
						if k != "$ref" {
							cp[k] = deepCopy(val)
						}
					}
					return cp
				}
			}
			return nil
		}
		return m
	}

	// selfRef 按完整 JSON Pointer 路径段判定自引用（"#/properties/a" 不是 "/properties/ab" 的祖先）
	selfRef := func(ref, path string) bool {
		if strings.HasPrefix(ref, "#") {
			r := ref[1:]
			return r == "" || path == r || strings.HasPrefix(path, r+"/")
		}
		return false
	}

	refPath := func(ref, path string) string {
		if strings.HasPrefix(ref, "#") {
			return ref[1:]
		}
		return path
	}

	expanding := map[string]bool{}

	ctx := &schemaCleanContext{
		localRef:  localRef,
		resolve:   resolve,
		selfRef:   selfRef,
		refPath:   refPath,
		expanding: expanding,
	}

	cleaned := ctx.cleanRecursive(schema, 0, "")
	result, ok := cleaned.(map[string]any)
	if !ok {
		return nil
	}
	return result
}

type schemaCleanContext struct {
	localRef  func(string) (any, bool)
	resolve   func(any) map[string]any
	selfRef   func(string, string) bool
	refPath   func(string, string) string
	expanding map[string]bool
}

func schemaTypeOf(m map[string]any) string {
	if t, ok := m["type"].(string); ok && t != "" {
		return strings.ToLower(t)
	}
	if m["properties"] != nil {
		return "object"
	}
	if m["items"] != nil || m["prefixItems"] != nil {
		return "array"
	}
	return "object"
}

// oneSays 提取元组（items 数组或 prefixItems）中所有非空成员一致的 schema；
// 若成员互不相同或均为空则返回 nil。
func oneSays(members []any) any {
	var one map[string]any
	for _, m := range members {
		s, ok := m.(map[string]any)
		if !ok || len(s) == 0 {
			continue
		}
		if one != nil && !reflect.DeepEqual(one, s) {
			return nil
		}
		one = s
	}
	if one == nil {
		return nil
	}
	return deepCopy(one)
}

// ownShape 判断节点除 anyOf/oneOf 外是否已经声明了自身形态（非 null 的 type、properties 或 items）。
func ownShape(x map[string]any) bool {
	if x["properties"] != nil || x["items"] != nil {
		return true
	}
	switch t := x["type"].(type) {
	case string:
		return strings.ToLower(t) != "null" && t != ""
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && strings.ToLower(s) != "null" && s != "" {
				return true
			}
		}
	}
	return false
}

// foldUnion 在节点具备自身形态（ownShape）时将 anyOf/oneOf 分支折叠进当前节点：
// 保留节点自身属性，合并分支新增的 properties，仅当所有非 null 分支都要求某字段时才将其加入 required。
func foldUnion(x map[string]any, resolve func(any) map[string]any) map[string]any {
	out := make(map[string]any, len(x))
	for k, v := range x {
		if k != "anyOf" && k != "oneOf" {
			out[k] = v
		}
	}
	object := x["type"] == "object" || x["properties"] != nil
	if ts, ok := x["type"].([]any); ok {
		for _, t := range ts {
			if s, ok := t.(string); ok && strings.EqualFold(s, "object") {
				object = true
			}
		}
	}
	props := map[string]any{}
	if own, ok := x["properties"].(map[string]any); ok {
		for k, v := range own {
			props[k] = v
		}
	}
	var common []string
	branches := 0
	for _, k := range []string{"anyOf", "oneOf"} {
		alts, _ := x[k].([]any)
		for _, a := range alts {
			m := resolve(a)
			if m == nil {
				continue
			}
			if s, ok := m["type"].(string); ok && strings.EqualFold(s, "null") {
				out["nullable"] = true
				continue
			}
			if !object {
				continue
			}
			bp, _ := m["properties"].(map[string]any)
			for _, pk := range slices.Sorted(maps.Keys(bp)) {
				if _, has := props[pk]; !has {
					props[pk] = deepCopy(bp[pk])
				}
			}
			br := requiredStringNames(m["required"])
			if branches == 0 {
				common = append([]string(nil), br...)
			} else {
				common = slices.DeleteFunc(common, func(n string) bool { return !slices.Contains(br, n) })
			}
			branches++
		}
	}
	if object {
		if len(props) > 0 {
			out["properties"] = props
		}
		ownReq := requiredStringNames(x["required"])
		if len(ownReq)+len(common) > 0 {
			mergedReq := make([]any, 0, len(ownReq)+len(common))
			seen := make(map[string]bool, len(ownReq)+len(common))
			for _, r := range ownReq {
				if !seen[r] {
					seen[r] = true
					mergedReq = append(mergedReq, r)
				}
			}
			for _, r := range common {
				if !seen[r] {
					seen[r] = true
					mergedReq = append(mergedReq, r)
				}
			}
			out["required"] = mergedReq
		}
	}
	return out
}

func requiredStringNames(v any) []string {
	switch l := v.(type) {
	case []any:
		var out []string
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), l...)
	default:
		return nil
	}
}

// collectDefs 仅收集 root 中的 $defs 与 definitions 供末段名回退使用，
// 不修改 root，确保指向 #/$defs/... 或 #/definitions/... 内部的 JSON Pointer 可完整寻址。
func collectDefs(root map[string]any) map[string]any {
	defs := make(map[string]any)
	for _, k := range []string{"$defs", "definitions"} {
		if d, ok := root[k].(map[string]any); ok {
			for n, v := range d {
				defs[n] = v
			}
		}
	}
	return defs
}

// deepCopy 深拷贝 (简单实现，仅针对 JSON 类型)
func deepCopy(src any) any {
	if src == nil {
		return nil
	}
	switch v := src.(type) {
	case map[string]any:
		dst := make(map[string]any, len(v))
		for k, val := range v {
			dst[k] = deepCopy(val)
		}
		return dst
	case []any:
		dst := make([]any, len(v))
		for i, val := range v {
			dst[i] = deepCopy(val)
		}
		return dst
	default:
		return src
	}
}

func (c *schemaCleanContext) cleanRecursive(value any, depth int, path string) any {
	switch x := value.(type) {
	case bool:
		if x {
			return map[string]any{}
		}
		return nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = c.cleanRecursive(item, depth, path)
		}
		return out
	case map[string]any:
		return c.cleanMap(x, depth, path)
	default:
		return value
	}
}

func (c *schemaCleanContext) cleanMap(schemaMap map[string]any, depth int, path string) map[string]any {
	if depth > 32 {
		return map[string]any{"type": "object"}
	}

	delete(schemaMap, "$defs")
	delete(schemaMap, "definitions")

	// 0. 展开 $ref（支持 JSON Pointer 与自引用环打断）
	if ref, ok := schemaMap["$ref"].(string); ok {
		delete(schemaMap, "$ref")
		if d, found := c.localRef(ref); found {
			if c.expanding[ref] || c.selfRef(ref, path) {
				targetType := "object"
				if dm, ok := d.(map[string]any); ok {
					targetType = schemaTypeOf(dm)
				}
				schemaMap["type"] = targetType
				if targetType != "object" {
					delete(schemaMap, "properties")
					delete(schemaMap, "required")
				}
				return c.cleanMap(schemaMap, depth+1, path)
			}
			if defMap, ok := d.(map[string]any); ok {
				for k, v := range defMap {
					if _, has := schemaMap[k]; !has {
						schemaMap[k] = deepCopy(v)
					}
				}
				c.expanding[ref] = true
				res := c.cleanMap(schemaMap, depth+1, c.refPath(ref, path))
				delete(c.expanding, ref)
				return res
			}
		}
		if _, hasType := schemaMap["type"]; !hasType {
			schemaMap["type"] = "object"
		}
	}

	// 1. 合并 allOf（逐分支递归展开并保留 required-only 子项与稳定 required 顺序）
	if _, hasAllOf := schemaMap["allOf"].([]any); hasAllOf {
		c.mergeAllOf(schemaMap, depth, path)
		return c.cleanMap(schemaMap, depth+1, path)
	}

	// 2. 处理 anyOf / oneOf
	if (schemaMap["anyOf"] != nil || schemaMap["oneOf"] != nil) && ownShape(schemaMap) {
		folded := foldUnion(schemaMap, c.resolve)
		for k := range schemaMap {
			delete(schemaMap, k)
		}
		for k, v := range folded {
			schemaMap[k] = v
		}
	} else {
		var unionArray []any
		var unionKey string
		if anyOf, ok := schemaMap["anyOf"].([]any); ok {
			unionArray = anyOf
			unionKey = "anyOf"
		} else if oneOf, ok := schemaMap["oneOf"].([]any); ok {
			unionArray = oneOf
			unionKey = "oneOf"
		}
		if len(unionArray) > 0 {
			nullable := false
			resolvedBranches := make([]any, 0, len(unionArray))
			for _, a := range unionArray {
				m := c.resolve(a)
				if m == nil {
					continue
				}
				if s, ok := m["type"].(string); ok && strings.EqualFold(s, "null") {
					nullable = true
					continue
				}
				resolvedBranches = append(resolvedBranches, m)
			}
			delete(schemaMap, "anyOf")
			delete(schemaMap, "oneOf")
			if nullable {
				schemaMap["nullable"] = true
			}
			if bestBranch := extractBestSchemaFromUnion(resolvedBranches); bestBranch != nil {
				if bestMap, ok := bestBranch.(map[string]any); ok {
					walked := c.cleanMap(deepCopy(bestMap).(map[string]any), depth+1, path+"/"+unionKey)
					for k, v := range walked {
						if k == "properties" {
							targetProps, _ := schemaMap["properties"].(map[string]any)
							if targetProps == nil {
								targetProps = make(map[string]any)
								schemaMap["properties"] = targetProps
							}
							if sourceProps, ok := v.(map[string]any); ok {
								for pk, pv := range sourceProps {
									if _, exists := targetProps[pk]; !exists {
										targetProps[pk] = deepCopy(pv)
									}
								}
							}
						} else if k == "required" {
							targetReq, _ := schemaMap["required"].([]any)
							if sourceReq, ok := v.([]any); ok {
								for _, rv := range sourceReq {
									if !slices.Contains(targetReq, rv) {
										targetReq = append(targetReq, rv)
									}
								}
								schemaMap["required"] = targetReq
							}
						} else if _, exists := schemaMap[k]; !exists {
							schemaMap[k] = deepCopy(v)
						}
					}
				}
			}
			if _, hasType := schemaMap["type"]; !hasType && schemaMap["properties"] == nil && schemaMap["items"] == nil {
				schemaMap["type"] = "string"
			}
		}
	}

	// 3. 递归处理 properties / items / prefixItems
	if props, ok := schemaMap["properties"].(map[string]any); ok {
		for pk, pv := range props {
			at := strings.ReplaceAll(strings.ReplaceAll(pk, "~", "~0"), "/", "~1")
			if b, isBool := pv.(bool); isBool {
				if b {
					props[pk] = map[string]any{}
				} else {
					delete(props, pk)
				}
				continue
			}
			cleanedProp := c.cleanRecursive(pv, depth+1, path+"/properties/"+at)
			if cleanedProp == nil {
				delete(props, pk)
			} else {
				props[pk] = cleanedProp
			}
		}
	}

	if items, exists := schemaMap["items"]; exists {
		switch it := items.(type) {
		case bool:
			if it {
				schemaMap["items"] = map[string]any{}
			} else {
				schemaMap["items"] = map[string]any{}
				if schemaMap["maxItems"] == nil {
					schemaMap["maxItems"] = 0
				}
			}
		case []any:
			if same := oneSays(it); same != nil {
				schemaMap["items"] = c.cleanRecursive(same, depth+1, path+"/items")
			} else if best := extractBestSchemaFromUnion(it); best != nil {
				schemaMap["items"] = c.cleanRecursive(deepCopy(best), depth+1, path+"/items")
			} else {
				schemaMap["items"] = map[string]any{}
			}
		case map[string]any:
			if len(it) == 0 {
				schemaMap["items"] = map[string]any{}
			} else {
				schemaMap["items"] = c.cleanRecursive(it, depth+1, path+"/items")
			}
		default:
			schemaMap["items"] = c.cleanRecursive(items, depth+1, path+"/items")
		}
	}

	var prefixItemSchema any
	if pfx, ok := schemaMap["prefixItems"].([]any); ok && len(pfx) > 0 {
		if s := oneSays(pfx); s != nil {
			prefixItemSchema = c.cleanRecursive(s, depth+1, path+"/items")
		}
	}

	// 检查当前对象是否为 JSON Schema 节点
	looksLikeSchema := hasKey(schemaMap, "type") ||
		hasKey(schemaMap, "properties") ||
		hasKey(schemaMap, "items") ||
		hasKey(schemaMap, "prefixItems") ||
		hasKey(schemaMap, "enum") ||
		hasKey(schemaMap, "const") ||
		hasKey(schemaMap, "anyOf") ||
		hasKey(schemaMap, "oneOf") ||
		hasKey(schemaMap, "allOf")

	if looksLikeSchema {
		// 4. 非空 string const 转 enum
		if cVal, ok := schemaMap["const"]; ok && schemaMap["enum"] == nil {
			if s, ok := cVal.(string); ok && strings.TrimSpace(s) != "" {
				schemaMap["enum"] = []any{s}
			}
		}

		// 5. 约束迁移到 description
		migrateConstraints(schemaMap)

		isEffectivelyNullable, _ := schemaMap["nullable"].(bool)

		// 6. 白名单过滤
		allowedFields := map[string]bool{
			"type":        true,
			"description": true,
			"properties":  true,
			"required":    true,
			"items":       true,
			"enum":        true,
			"title":       true,
		}
		for k := range schemaMap {
			if !allowedFields[k] {
				delete(schemaMap, k)
			}
		}

		// 7. 处理 type 字段 (Lowercase + Nullable 提取 + 多类型根据 properties/items 优选)
		if typeVal, exists := schemaMap["type"]; exists {
			var selectedType string
			switch v := typeVal.(type) {
			case string:
				lower := strings.ToLower(v)
				if lower == "null" {
					isEffectivelyNullable = true
					selectedType = "string"
				} else {
					selectedType = lower
				}
			case []any:
				for _, t := range v {
					if ts, ok := t.(string); ok {
						lower := strings.ToLower(ts)
						if lower == "null" {
							isEffectivelyNullable = true
						} else if selectedType == "" {
							selectedType = lower
						}
					}
				}
				for _, t := range v {
					if ts, ok := t.(string); ok {
						lower := strings.ToLower(ts)
						if (lower == "array" && (schemaMap["items"] != nil || prefixItemSchema != nil)) ||
							(lower == "object" && schemaMap["properties"] != nil) {
							selectedType = lower
							break
						}
					}
				}
				if selectedType == "" {
					selectedType = "string"
				}
			}
			schemaMap["type"] = selectedType
		} else {
			switch {
			case hasKey(schemaMap, "properties"):
				schemaMap["type"] = "object"
			case hasKey(schemaMap, "items") || prefixItemSchema != nil:
				schemaMap["type"] = "array"
			case hasKey(schemaMap, "enum"):
				schemaMap["type"] = "string"
			default:
				schemaMap["type"] = "object"
			}
		}

		// 8. 任意深度 type=="array" 必须有 items (#2826 / plugins#375)
		if schemaMap["type"] == "array" && schemaMap["items"] == nil {
			if prefixItemSchema != nil {
				schemaMap["items"] = prefixItemSchema
			} else {
				schemaMap["items"] = map[string]any{}
			}
		}

		// 9. 清理非对应 type 的越界关键字（防 $type == Type.ARRAY / OBJECT 400）
		if t, ok := schemaMap["type"].(string); ok {
			if t != "array" {
				delete(schemaMap, "items")
			}
			if t != "object" {
				delete(schemaMap, "properties")
				delete(schemaMap, "required")
			}
		}

		// 10. 处理空 Object
		if t, _ := schemaMap["type"].(string); t == "object" {
			hasProps := false
			if props, ok := schemaMap["properties"].(map[string]any); ok && len(props) > 0 {
				hasProps = true
			}
			if !hasProps {
				schemaMap["properties"] = map[string]any{
					"reason": map[string]any{
						"type":        "string",
						"description": "Reason for calling this tool",
					},
				}
				schemaMap["required"] = []any{"reason"}
			}
		}

		// 11. Required 字段对齐
		if props, ok := schemaMap["properties"].(map[string]any); ok {
			if req, ok := schemaMap["required"].([]any); ok {
				var validReq []any
				for _, r := range req {
					if rStr, ok := r.(string); ok {
						if _, exists := props[rStr]; exists {
							validReq = append(validReq, rStr)
						}
					}
				}
				if len(validReq) > 0 {
					schemaMap["required"] = validReq
				} else {
					delete(schemaMap, "required")
				}
			}
		}

		if isEffectivelyNullable {
			desc, _ := schemaMap["description"].(string)
			if !strings.Contains(desc, "nullable") {
				if desc != "" {
					desc += " "
				}
				desc += "(nullable)"
				schemaMap["description"] = desc
			}
		}

		// 12. Enum 值强制转字符串并过滤空白串 (#5235)
		if enumVals, ok := schemaMap["enum"].([]any); ok {
			hasNonString := false
			filtered := make([]any, 0, len(enumVals))
			for _, val := range enumVals {
				if s, isStr := val.(string); isStr {
					if strings.TrimSpace(s) != "" {
						filtered = append(filtered, s)
					}
				} else {
					hasNonString = true
					if val == nil {
						filtered = append(filtered, "null")
					} else {
						s := fmt.Sprintf("%v", val)
						if strings.TrimSpace(s) != "" {
							filtered = append(filtered, s)
						}
					}
				}
			}
			if len(filtered) > 0 {
				schemaMap["enum"] = filtered
				if hasNonString {
					schemaMap["type"] = "string"
				}
			} else {
				delete(schemaMap, "enum")
			}
		}
	}

	return schemaMap
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func migrateConstraints(m map[string]any) {
	constraints := []struct {
		key   string
		label string
	}{
		{"minLength", "minLen"},
		{"maxLength", "maxLen"},
		{"pattern", "pattern"},
		{"minimum", "min"},
		{"maximum", "max"},
		{"multipleOf", "multipleOf"},
		{"exclusiveMinimum", "exclMin"},
		{"exclusiveMaximum", "exclMax"},
		{"minItems", "minItems"},
		{"maxItems", "maxItems"},
		{"propertyNames", "propertyNames"},
		{"format", "format"},
	}

	var hints []string
	for _, c := range constraints {
		if val, ok := m[c.key]; ok && val != nil {
			hints = append(hints, fmt.Sprintf("%s: %v", c.label, val))
		}
	}

	if len(hints) > 0 {
		suffix := fmt.Sprintf(" [Constraint: %s]", strings.Join(hints, ", "))
		desc, _ := m["description"].(string)
		if !strings.Contains(desc, suffix) {
			m["description"] = desc + suffix
		}
	}
}

// collectAllOfRequired 递归收集 allOf 分支（含 $ref 与嵌套 allOf）声明的 required 字段，
// 确保仅含 required 而无 properties 的分支在独立 walk 过滤前不会丢失 required 声明。
func (c *schemaCleanContext) collectAllOfRequired(node any, depth int, seen map[string]bool) {
	if depth > 32 {
		return
	}
	m := c.resolve(node)
	if m == nil {
		return
	}
	for _, r := range requiredStringNames(m["required"]) {
		seen[r] = true
	}
	if nested, ok := m["allOf"].([]any); ok {
		for _, sub := range nested {
			c.collectAllOfRequired(sub, depth+1, seen)
		}
	}
}

// branchHasDeclaredProperties 判断 allOf 分支（含 $ref 与嵌套 allOf/anyOf/oneOf）是否真正声明过 properties，
// 避免仅含 required 或 not 的空分支在独立 walk 时因空对象兜底生成的合成 "reason" 属性污染外层对象。
func (c *schemaCleanContext) branchHasDeclaredProperties(node any, depth int) bool {
	if depth > 32 {
		return false
	}
	m := c.resolve(node)
	if m == nil {
		return false
	}
	if props, ok := m["properties"].(map[string]any); ok && len(props) > 0 {
		return true
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if list, ok := m[key].([]any); ok {
			for _, sub := range list {
				if c.branchHasDeclaredProperties(sub, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

// mergeAllOf 逐分支 walk 并合并 allOf（支持嵌套 allOf、本地 $ref 子分支与 required-only 分支，
// 同时按字典序稳定追加新增 required 以保证缓存前缀确定性）。
func (c *schemaCleanContext) mergeAllOf(m map[string]any, depth int, path string) {
	allOf, ok := m["allOf"].([]any)
	if !ok {
		return
	}
	delete(m, "allOf")

	mergedProps := make(map[string]any)
	mergedReq := make(map[string]bool)
	otherFields := make(map[string]any)

	for _, sub := range allOf {
		c.collectAllOfRequired(sub, depth+1, mergedReq)
		subMap, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		hasRealProps := c.branchHasDeclaredProperties(sub, depth+1)
		walked := c.cleanMap(deepCopy(subMap).(map[string]any), depth+1, path+"/allOf")
		if hasRealProps {
			for _, r := range requiredStringNames(walked["required"]) {
				mergedReq[r] = true
			}
			if props, ok := walked["properties"].(map[string]any); ok {
				for k, v := range props {
					mergedProps[k] = v
				}
			}
		}
		for k, v := range walked {
			if k != "properties" && k != "required" && k != "allOf" {
				if _, exists := otherFields[k]; !exists {
					otherFields[k] = v
				}
			}
		}
	}

	// Apply
	for k, v := range otherFields {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	if len(mergedProps) > 0 {
		existProps, _ := m["properties"].(map[string]any)
		if existProps == nil {
			existProps = make(map[string]any)
			m["properties"] = existProps
		}
		for k, v := range mergedProps {
			if _, exists := existProps[k]; !exists {
				existProps[k] = v
			}
		}
	}
	if len(mergedReq) > 0 {
		existReq, _ := m["required"].([]any)
		var validReqs []any
		for _, r := range existReq {
			if s, ok := r.(string); ok {
				validReqs = append(validReqs, s)
				delete(mergedReq, s) // already exists
			}
		}
		// Map iteration must not reshuffle the tokenized tools prefix on each
		// request. Keep existing order and append newly merged names canonically.
		newRequired := make([]string, 0, len(mergedReq))
		for r := range mergedReq {
			newRequired = append(newRequired, r)
		}
		sort.Strings(newRequired)
		for _, r := range newRequired {
			validReqs = append(validReqs, r)
		}
		m["required"] = validReqs
	}
}

// extractBestSchemaFromUnion 从 anyOf/oneOf 中选取最佳分支
func extractBestSchemaFromUnion(unionArray []any) any {
	var bestOption any
	bestScore := -1

	for _, item := range unionArray {
		score := scoreSchemaOption(item)
		if score > bestScore {
			bestScore = score
			bestOption = item
		}
	}
	return bestOption
}

func scoreSchemaOption(val any) int {
	m, ok := val.(map[string]any)
	if !ok {
		return 0
	}
	typeStr, _ := m["type"].(string)

	if hasKey(m, "properties") || typeStr == "object" {
		return 3
	}
	if hasKey(m, "items") || typeStr == "array" {
		return 2
	}
	if typeStr != "" && typeStr != "null" {
		return 1
	}
	return 0
}

// DeepCleanUndefined 深度清理值为 "[undefined]" 的字段
func DeepCleanUndefined(value any) {
	if value == nil {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		for k, val := range v {
			if s, ok := val.(string); ok && s == "[undefined]" {
				delete(v, k)
				continue
			}
			DeepCleanUndefined(val)
		}
	case []any:
		for _, val := range v {
			DeepCleanUndefined(val)
		}
	}
}
