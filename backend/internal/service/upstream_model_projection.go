package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Recognize complete globally enabled names, longest first (extra-low before low).
// A single suffix is not naming evidence: it may be a literal/budget model ID.
func splitCatalogEffort(id string, p *AntigravityModelEffortSettings) (string, string) {
	levels := append([]string(nil), p.Levels...)
	sort.Slice(levels, func(i, j int) bool {
		if len(levels[i]) != len(levels[j]) {
			return len(levels[i]) > len(levels[j])
		}
		return levels[i] < levels[j]
	})
	for _, level := range levels {
		if base, found := strings.CutSuffix(id, "-"+level); found && base != "" {
			return base, level
		}
	}
	return "", ""
}

func projectUpstreamModelMapping(account *Account, ids []string, metadata map[string]UpstreamModelMetadata, p *AntigravityModelEffortSettings) (map[string]string, map[string][]string) {
	mapping := map[string]string{}
	families := map[string][]string{}
	for _, id := range dedupeAndSortModelIDs(ids) {
		mapping[id] = id
	}
	if account == nil || account.Platform != PlatformAntigravity {
		return mapping, families
	}
	variants := map[string][]string{}
	for id := range mapping {
		if base, level := splitCatalogEffort(id, p); base != "" {
			variants[base] = append(variants[base], level)
		}
	}
	bases := make([]string, 0, len(variants))
	for base := range variants {
		bases = append(bases, base)
	}
	// Protect nested literal families, e.g. foo-low-high vs foo-low.
	sort.Slice(bases, func(i, j int) bool {
		if len(bases[i]) != len(bases[j]) {
			return len(bases[i]) > len(bases[j])
		}
		return bases[i] < bases[j]
	})
	claimed := map[string]bool{}
	for _, base := range bases {
		levels := variants[base]
		_, hasBase := mapping[base]
		if len(levels) < 2 && !hasBase {
			continue
		}
		if claimed[base] {
			continue
		}
		conflict := false
		for _, level := range levels {
			if claimed[base+"-"+level] {
				conflict = true
			}
		}
		if conflict {
			continue
		}
		sort.Strings(levels)
		def := ""
		for _, level := range levels {
			if level == p.DefaultEffort {
				def = level
			}
		}
		if def == "" {
			candidate := metadata[base].DefaultReasoningLevel
			for _, level := range levels {
				if level == candidate {
					def = level
				}
			}
		}
		if def == "" {
			def = levels[0]
		}
		target := base + "-{effort}"
		if def != p.DefaultEffort {
			target = base + "-{effort:" + def + "}"
		}
		for _, level := range levels {
			delete(mapping, base+"-"+level)
			claimed[base+"-"+level] = true
		}
		mapping[base] = target
		claimed[base] = true
		families[base] = append([]string(nil), levels...)
		// The family capability is a projection of actual ID variants, not a claim
		// that native reasoning levels or globally enabled levels exist upstream.
		entry := metadata[base+"-"+def]
		if entry.ID == "" {
			entry = metadata[base]
		}
		entry.ID = base
		entry.Source = "upstream"
		entry.ReasoningLevelsSource = "catalog_suffix"
		entry.SupportedReasoningLevels = append([]string(nil), levels...)
		entry.DefaultReasoningLevel = def
		metadata[base] = entry
	}
	return mapping, families
}

// Keep selected capability fields only; do not retain quota counters, credentials
// or the raw response in metadata. Thinking budgets do not imply suffix levels.
//
// Antigravity disabled filtering and borrowed displayName disambiguation are
// adapted from Magpie (github.com/yetone/magpie, MIT License)
// internal/provider/antigravity_models.go & google.go (#1918).
func extractAntigravityCatalogMetadata(body []byte) map[string]UpstreamModelMetadata {
	var response struct {
		Models map[string]struct {
			DisplayName              string   `json:"displayName"`
			Model                    string   `json:"model"`
			Disabled                 bool     `json:"disabled"`
			SupportsThinking         *bool    `json:"supportsThinking"`
			SupportsImages           *bool    `json:"supportsImages"`
			MaxTokens                int64    `json:"maxTokens"`
			MaxOutputTokens          int64    `json:"maxOutputTokens"`
			SupportedReasoningLevels []string `json:"supportedReasoningLevels"`
			DefaultReasoningLevel    string   `json:"defaultReasoningLevel"`
		} `json:"models"`
	}
	result := map[string]UpstreamModelMetadata{}
	if json.Unmarshal(body, &response) != nil {
		return result
	}
	spelled := map[string]bool{}
	for id, raw := range response.Models {
		id = strings.TrimSpace(id)
		if id == "" || raw.Disabled {
			continue
		}
		if antigravityDisplayNameSpellsID(raw.DisplayName, id) {
			spelled[raw.DisplayName] = true
		}
	}
	for id, raw := range response.Models {
		id = strings.TrimSpace(id)
		if id == "" || raw.Disabled {
			continue
		}
		displayName := raw.DisplayName
		if displayName != "" && !antigravityDisplayNameSpellsID(displayName, id) {
			if spelled[displayName] || antigravityEnumNamesID(raw.Model, id) {
				displayName = id
			}
		}
		modalities := []string{"text"}
		if raw.SupportsImages != nil && *raw.SupportsImages {
			modalities = append(modalities, "image")
		}
		result[id] = UpstreamModelMetadata{ID: id, DisplayName: displayName, Reasoning: raw.SupportsThinking, InputModalities: modalities, ContextWindow: raw.MaxTokens, MaxOutputTokens: raw.MaxOutputTokens, SupportedReasoningLevels: dedupeAndSortModelIDs(raw.SupportedReasoningLevels), DefaultReasoningLevel: strings.ToLower(strings.TrimSpace(raw.DefaultReasoningLevel))}
	}
	return result
}

func antigravityDisplayNameWithoutEffort(name string) string {
	open := strings.LastIndex(name, " (")
	if open < 0 || !strings.HasSuffix(name, ")") {
		return name
	}
	switch strings.ToLower(name[open+2 : len(name)-1]) {
	case "minimal", "extra low", "low", "medium", "high", "extra high", "xhigh", "max", "ultra":
		return strings.TrimSpace(name[:open])
	default:
		return name
	}
}

func antigravityEffortBaseID(id string) string {
	for _, word := range []string{"extra-high", "extra-low", "xhigh", "minimal", "low", "medium", "high", "max", "ultra"} {
		if base, ok := strings.CutSuffix(id, "-"+word); ok && base != "" {
			return base
		}
	}
	return id
}

func antigravityDisplayNameSpellsID(name, id string) bool {
	baseName := antigravityDisplayNameWithoutEffort(name)
	baseName = strings.Join(strings.Fields(baseName), "-")
	return baseName != "" && strings.EqualFold(baseName, antigravityEffortBaseID(id))
}

func antigravityEnumNamesID(enum, id string) bool {
	e, ok := strings.CutPrefix(strings.TrimSpace(enum), "MODEL_GOOGLE_")
	return ok && e != "" && strings.EqualFold(e, strings.NewReplacer("-", "_", ".", "_").Replace(id))
}

// Saved mappings, including catalog-projected templates, are routing authority.
// Discovery snapshots may lag new models and must not veto a configured target.
// Template syntax and enabled effort levels still follow the request's policy.
func (a *Account) resolveAntigravityCatalogMapping(mapping map[string]string, requested, effort string, p *AntigravityModelEffortSettings) (string, bool) {
	return resolveAntigravityEffortMappingWithPolicy(mapping, requested, effort, p)
}

// Scope list evidence to the account/network identity, not an access token that
// rotates normally. Store only the digest; no upstream credentials are exposed.
func upstreamCatalogIdentity(a *Account) string {
	if a == nil {
		return ""
	}
	identity := []string{fmt.Sprint(a.ID), a.Platform, a.Type, a.GetCredential("project_id"), upstreamModelsProxyURL(a), a.GetCredential("base_url"), a.GetCredential("email"), a.GetCredential("sub")}
	if a.Type == AccountTypeAPIKey {
		identity = append(identity, a.GetCredential("api_key"))
	}
	body, _ := json.Marshal(identity)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}
