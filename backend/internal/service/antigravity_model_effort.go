package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type antigravityEffortTemplate struct{ prefix, defaultEffort string }

// Suffix-only templates opt in explicitly. Ordinary literal mappings are never
// interpreted as effort families. An omitted default uses the global setting.
func parseAntigravityEffortTemplate(target string) (antigravityEffortTemplate, bool) {
	marker := strings.Index(target, "-{effort")
	if marker <= 0 || strings.Count(target, "{effort") != 1 || strings.ContainsAny(target[:marker], "{} \t\r\n") {
		return antigravityEffortTemplate{}, false
	}
	t := antigravityEffortTemplate{prefix: target[:marker]}
	tail := target[marker:]
	if tail == "-{effort}" {
		return t, true
	}
	if strings.HasPrefix(tail, "-{effort:") && strings.HasSuffix(tail, "}") {
		t.defaultEffort = strings.TrimSuffix(strings.TrimPrefix(tail, "-{effort:"), "}")
		if antigravityEffortName.MatchString(t.defaultEffort) {
			return t, true
		}
	}
	return antigravityEffortTemplate{}, false
}

func (t antigravityEffortTemplate) model(effort string, p *AntigravityModelEffortSettings) string {
	if effort == "" {
		effort = t.defaultEffort
		if effort == "" {
			effort = p.DefaultEffort
		}
	}
	if !p.supports(effort) {
		return ""
	}
	return t.prefix + "-" + effort
}

func ValidateAntigravityEffortModelMapping(credentials map[string]any) error {
	p := currentAntigravityModelEffortSettings()
	for source, target := range stringMappingFromRaw(credentials["model_mapping"]) {
		if !strings.Contains(target, "{effort") {
			continue
		}
		t, valid := parseAntigravityEffortTemplate(target)
		if !valid || strings.Contains(source, "*") || strings.TrimSpace(source) == "" || (t.defaultEffort != "" && !p.supports(t.defaultEffort)) {
			return fmt.Errorf("invalid model_mapping effort template for %q: use an exact base key, target-{effort}, and a globally enabled default", source)
		}
	}
	return nil
}

type antigravityRequestEffortKey struct{}

// Bind once at ingress; retries and forwarding retain the same policy snapshot.
// Budgets stay budgets because the protocols define no budget-to-level map.
func WithAntigravityRequestEffort(ctx context.Context, body []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Value(antigravityEffortPolicyKey{}) == nil {
		ctx = context.WithValue(ctx, antigravityEffortPolicyKey{}, currentAntigravityModelEffortSettings())
	}
	effort := ""
	for _, path := range []string{"reasoning.effort", "reasoning_effort", "output_config.effort", "generationConfig.thinkingConfig.thinkingLevel", "generation_config.thinking_config.thinking_level"} {
		value := gjson.GetBytes(body, path)
		if value.Exists() {
			effort = strings.ToLower(strings.TrimSpace(value.String()))
			break
		}
	}
	return context.WithValue(ctx, antigravityRequestEffortKey{}, effort)
}

func antigravityRequestEffort(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if effort, ok := ctx.Value(antigravityRequestEffortKey{}).(string); ok {
		return effort
	}
	if effort := RequestedReasoningEffortFromContext(ctx); effort != nil {
		return *effort
	}
	return ""
}

func resolveAntigravityEffortMapping(mapping map[string]string, requested, effort string) (string, bool) {
	return resolveAntigravityEffortMappingWithPolicy(mapping, requested, effort, currentAntigravityModelEffortSettings())
}

// Exact suffixes/aliases and wildcards win. Match configured template base IDs
// plus a complete enabled level, never an arbitrary last hyphen token.
func resolveAntigravityEffortMappingWithPolicy(mapping map[string]string, requested, effort string, p *AntigravityModelEffortSettings) (string, bool) {
	if target, matched := resolveRequestedModelInMapping(mapping, requested); matched {
		if t, valid := parseAntigravityEffortTemplate(target); valid {
			if _, exact := mapping[requested]; !exact {
				return "", true
			}
			if effort != "" {
				if !p.supports(effort) {
					return "", true
				}
				if override, matched := resolveRequestedModelInMapping(mapping, requested+"-"+effort); matched {
					if rule, valid := parseAntigravityEffortTemplate(override); valid {
						return rule.model("", p), true
					}
					if strings.Contains(override, "{effort") {
						return "", true
					}
					return override, true
				}
			}
			return t.model(effort, p), true
		}
		if strings.Contains(target, "{effort") {
			return "", true
		}
		return target, true
	}
	// Complete level names are longest-first. An overlapping base must not
	// turn extra-low into low; ties between bases remain deterministic.
	bases := make([]string, 0, len(mapping))
	for base, target := range mapping {
		if _, valid := parseAntigravityEffortTemplate(target); valid && !strings.Contains(base, "*") {
			bases = append(bases, base)
		}
	}
	sort.Slice(bases, func(i, j int) bool {
		if len(bases[i]) != len(bases[j]) {
			return len(bases[i]) > len(bases[j])
		}
		return bases[i] < bases[j]
	})
	for _, level := range p.Levels {
		for _, base := range bases {
			if requested == base+"-"+level {
				t, _ := parseAntigravityEffortTemplate(mapping[base])
				return t.model(level, p), true
			}
		}
	}
	return requested, false
}

func antigravityEffortRequestError(ctx context.Context, account *Account, requested string) string {
	mapping := account.GetModelMapping()
	target, exists := mapping[strings.TrimPrefix(strings.TrimSpace(requested), "models/")]
	if !exists {
		return ""
	}
	if t, valid := parseAntigravityEffortTemplate(target); valid {
		p := antigravityEffortPolicyFromContext(ctx)
		effort := antigravityRequestEffort(ctx)
		if effort != "" && !p.supports(effort) {
			return fmt.Sprintf("reasoning effort %q is not enabled in Antigravity model effort settings", effort)
		}
		if t.model(effort, p) == "" {
			return "model template default is not enabled in Antigravity model effort settings"
		}
	}
	return ""
}

// Conservative compaction for explicitly configured templates. Exceptions and
// wildcard conflicts remain unless an operator deliberately rebuilds the table.
func CompactAntigravityEffortMapping(mapping map[string]string) (map[string]string, []string) {
	p := currentAntigravityModelEffortSettings()
	result := make(map[string]string, len(mapping))
	for key, value := range mapping {
		result[key] = value
	}
	conflicts := []string{}
	for base, target := range mapping {
		t, valid := parseAntigravityEffortTemplate(target)
		if !valid {
			continue
		}
		for _, level := range p.Levels {
			variant := base + "-" + level
			value, exists := mapping[variant]
			if !exists {
				continue
			}
			wildcardConflict := false
			for pattern, target := range mapping {
				if strings.Contains(pattern, "*") && matchWildcard(pattern, variant) && target != value {
					wildcardConflict = true
				}
			}
			if value == variant && t.model(level, p) == variant && !wildcardConflict {
				delete(result, variant)
			} else {
				conflicts = append(conflicts, variant)
			}
		}
	}
	sort.Strings(conflicts)
	return result, conflicts
}

// RebuildAntigravityModelMapping intentionally drops legacy aliases, keeping
// canonical targets and independent IDs. The caller explicitly selects naming
// families/defaults; this migration policy is never applied on the request path.
func RebuildAntigravityModelMapping(mapping map[string]string, familyDefaults map[string]string) (map[string]string, error) {
	p := currentAntigravityModelEffortSettings()
	result := map[string]string{}
	for _, target := range mapping {
		if _, template := parseAntigravityEffortTemplate(target); template {
			return nil, fmt.Errorf("rebuild requires a legacy literal table")
		}
		if strings.TrimSpace(target) == "" || strings.ContainsAny(target, "{}*? \t\r\n") {
			return nil, fmt.Errorf("cannot rebuild an invalid canonical target")
		}
		result[target] = target
	}
	for base, def := range familyDefaults {
		if !p.supports(def) {
			return nil, fmt.Errorf("default %q is not globally enabled", def)
		}
		found := false
		if _, ok := result[base]; ok {
			found = true
			delete(result, base)
		}
		for _, level := range p.Levels {
			variant := base + "-" + level
			if _, ok := result[variant]; ok {
				found = true
				delete(result, variant)
			}
		}
		if !found {
			return nil, fmt.Errorf("family %q has no existing canonical target", base)
		}
		target := base + "-{effort}"
		if def != p.DefaultEffort {
			target = base + "-{effort:" + def + "}"
		}
		result[base] = target
	}
	return result, nil
}

// Task19 capture authorization stays fixed; configurable effort names do not
// authorize capturing a new model or a new session.
func isGeminiCaptureFinalModel(model string) bool {
	return model == GeminiCaptureTargetModel || model == GeminiCaptureTargetModel+"-low" || model == GeminiCaptureTargetModel+"-medium" || model == GeminiCaptureTargetModel+"-high"
}
