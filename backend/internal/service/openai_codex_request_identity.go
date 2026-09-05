package service

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexRequestIdentityContextKey = "openai_codex_request_identity"

// codexRequestIdentity is an immutable projection of original request carriers.
// Applying it more than once never hashes an already projected value.
type codexRequestIdentity struct {
	originalSession  string
	accountID        int64
	values           map[string]string
	cacheKey         string
	originalCacheKey string
	lifecycle        map[string]any
	scoped           bool
	omitted          map[string]bool
	canonical        bool
	memoryRequest    bool
}

var codexRequestIdentityFields = []struct{ name, kind string }{
	{"installation_id", "installation"}, {"x-codex-installation-id", "installation"},
	{"session_id", "session"}, {"session-id", "session"},
	{"thread_id", "thread"}, {"thread-id", "thread"},
	{"x-client-request-id", "request"},
	{"turn_id", "turn"}, {"turn-id", "turn"},
	{"parent_thread_id", "parent-thread"}, {"parent-thread-id", "parent-thread"},
	{"x-codex-parent-thread-id", "parent-thread"},
	{"root_thread_id", "root-thread"}, {"root-thread-id", "root-thread"},
	{"forked_from_thread_id", "fork-thread"}, {"forked-from-thread-id", "fork-thread"},
	{"parent_turn_id", "parent-turn"}, {"parent-turn-id", "parent-turn"},
	{"root_turn_id", "root-turn"}, {"root-turn-id", "root-turn"},
	{"window_id", "window"}, {"x-codex-window-id", "window"},
	{"context_window_id", "context-window"}, {"context-window-id", "context-window"},
}

func codexMetadataObject(raw any) map[string]any {
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return nil
	}
	return result
}

func codexIdentityMetadata(body map[string]any) map[string]any {
	switch m := body["client_metadata"].(type) {
	case map[string]any:
		return m
	case map[string]string:
		n := make(map[string]any, len(m))
		for k, v := range m {
			n[k] = v
		}
		return n
	}
	return nil
}

func newCodexRequestIdentity(account, source *Account, apiKeyID int64, headers http.Header, body map[string]any) *codexRequestIdentity {
	return resolveCodexRequestIdentity(account, source, apiKeyID, headers, body)
}

func resolveCodexRequestIdentity(account, source *Account, apiKeyID int64, headers http.Header, body map[string]any) *codexRequestIdentity {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return nil
	}
	if source == nil {
		source = account
	}
	// Selection owns its saved opt-in mode; credential resolution must not
	// silently enable convergence on an unconfigured shadow row.
	mode := account.GetCodexFingerprintMode()
	seed, seeded := codexFingerprintSeed(account.Extra)
	if !seeded {
		mode = codexFingerprintOff
	}
	// Aliases of a real credential must not split just because their row seeds differ.
	if ns := codexAccountIdentityNamespace(source); ns != "" && seeded {
		seed = ns
	} else if source != account && ns == "" {
		// A shadow row's private seed is not the resolved credential identity.
		mode = codexFingerprintOff
	}
	original := make(map[string]string)
	read := func(values map[string]any) {
		for _, f := range codexRequestIdentityFields {
			if v, ok := values[f.name].(string); ok && strings.TrimSpace(v) != "" && original[f.kind] == "" {
				original[f.kind] = strings.TrimSpace(v)
			}
		}
	}
	flat := make(map[string]any)
	for _, f := range codexRequestIdentityFields {
		if v := headers.Get(f.name); v != "" {
			flat[f.name] = v
		}
	}
	cm := codexIdentityMetadata(body)
	// Native Codex owns the full snapshot in the body; flat keys and the
	// bounded header blob are compatibility projections. A WS handshake must
	// never overwrite a later frame's snapshot.
	bodyMetadata := codexMetadataObject(cm[openAIWSTurnMetadataHeader])
	headerMetadata := codexMetadataObject(headers.Get(openAIWSTurnMetadataHeader))
	read(bodyMetadata)
	// Optional identity in a complete snapshot is absent deliberately. A
	// stale compatibility header must not attach a root turn to an old parent
	// or put an old turn/window back on a compaction request.
	omitted := make(map[string]bool)
	if bodyMetadata != nil {
		for _, kind := range []string{"turn", "parent-thread", "root-thread", "fork-thread", "parent-turn", "root-turn", "context-window"} {
			omitted[kind] = original[kind] == ""
		}
	}
	read(cm)
	read(headerMetadata)
	read(flat)
	for kind, absent := range omitted {
		if absent {
			delete(original, kind)
		}
	}
	// client-request-id is a thread alias, not an HTTP-attempt identity.
	if original["thread"] == "" {
		original["thread"] = original["request"]
	}
	if original["thread"] != "" {
		original["request"] = original["thread"]
	}
	project := func(kind, raw string) string {
		if raw == "" {
			return ""
		}
		switch kind {
		case "session":
			if mode == codexFingerprintSession || mode == codexFingerprintFull {
				return resolveConvergedSessionID(seed)
			}
			kind = "session"
		case "thread", "parent-thread", "root-thread", "fork-thread", "request":
			if mode == codexFingerprintFull {
				return resolveConvergedSessionID(seed)
			}
			kind = "session"
		case "parent-turn", "root-turn":
			kind = "turn"
		case "context-window":
			kind = "window"
		case "installation":
			if mode != codexFingerprintOff {
				return resolveConvergedInstallationID(source, seed)
			}
		}
		return scopeCodexAccountIdentityValue(source, apiKeyID, kind, raw)
	}
	p := &codexRequestIdentity{accountID: account.ID, values: make(map[string]string), lifecycle: make(map[string]any), scoped: codexAccountIdentityNamespace(source) != "", omitted: omitted, canonical: bodyMetadata != nil}
	p.originalSession = original["session"]
	if bodyMetadata != nil {
		p.memoryRequest = bodyMetadata["request_kind"] == "memory"
	} else {
		p.memoryRequest = headerMetadata["request_kind"] == "memory"
	}
	for _, key := range []string{"turn_started_at_unix_ms", "window_number", "forked_from_ordinal_exclusive"} {
		for _, carrier := range []map[string]any{bodyMetadata, cm, headerMetadata} {
			if value, exists := carrier[key]; exists {
				switch value.(type) {
				case string, float64, json.Number, int, int64:
					p.lifecycle[key] = value
				}
				break
			}
			if p.canonical {
				break
			}
		}
	}
	for kind, raw := range original {
		if kind == "window" || kind == "context-window" {
			if prefix, suffix, ok := strings.Cut(raw, ":"); ok && prefix != "" {
				p.values[kind] = project("thread", prefix) + ":" + suffix
				continue
			}
		}
		p.values[kind] = project(kind, raw)
	}
	if mode != codexFingerprintOff {
		p.values["installation"] = resolveConvergedInstallationID(source, seed)
	}
	if raw, ok := body["prompt_cache_key"].(string); ok && strings.TrimSpace(raw) != "" {
		p.originalCacheKey = raw
		// Group by the original cache value, never by whether one request happens
		// to use it as its session default. Equal keys must stay equal across
		// trees, and session/full convergence must not broaden that grouping.
		p.cacheKey = scopeCodexAccountIdentityValue(source, apiKeyID, "session", raw)
	}
	return p
}

func (p *codexRequestIdentity) applyFields(values map[string]any, add bool) bool {
	if p == nil || values == nil {
		return false
	}
	changed := false
	for _, f := range codexRequestIdentityFields {
		if p.omitted[f.kind] {
			if _, exists := values[f.name]; exists {
				delete(values, f.name)
				changed = true
			}
			continue
		}
		v := p.values[f.kind]
		if v == "" {
			continue
		}
		_, exists := values[f.name]
		if !exists && !add {
			continue
		}
		if values[f.name] != v {
			values[f.name] = v
			changed = true
		}
	}
	for _, key := range []string{"turn_started_at_unix_ms", "window_number", "forked_from_ordinal_exclusive"} {
		existing, exists := values[key]
		if !exists {
			continue
		}
		value, known := p.lifecycle[key]
		if !known {
			if p.canonical {
				delete(values, key)
				changed = true
			}
			continue
		}
		// Flat client_metadata compatibility values are strings, whereas the
		// canonical JSON blob uses numbers for these scalar fields.
		if _, stringValue := existing.(string); stringValue {
			if number, ok := value.(float64); ok {
				value = strconv.FormatFloat(number, 'f', -1, 64)
			} else {
				value = fmt.Sprint(value)
			}
		}
		if existing != value {
			values[key] = value
			changed = true
		}
	}
	return changed
}

func (p *codexRequestIdentity) applyEmbedded(values map[string]any) bool {
	if p == nil {
		return false
	}
	m := codexMetadataObject(values[openAIWSTurnMetadataHeader])
	if m == nil {
		if values[openAIWSTurnMetadataHeader] == nil {
			return false
		}
		m = make(map[string]any)
	}
	changed := false
	if p.memoryRequest {
		for _, f := range codexRequestIdentityFields {
			if f.kind == "installation" || f.kind == "session" || f.kind == "thread" || f.kind == "request" || f.kind == "window" {
				if _, exists := m[f.name]; exists {
					delete(m, f.name)
					changed = true
				}
			}
		}
	}
	if p.applyFields(m, false) {
		changed = true
	}
	for key, value := range p.lifecycle {
		if m[key] != value {
			m[key] = value
			changed = true
		}
	}
	for _, f := range codexRequestIdentityFields {
		// Native memory requests deliberately omit core identity in the blob,
		// even though their base flat compatibility fields are still present.
		if p.memoryRequest && (f.kind == "installation" || f.kind == "session" || f.kind == "thread" || f.kind == "request" || f.kind == "window") {
			continue
		}
		if strings.Contains(f.name, "_") && p.values[f.kind] != "" && m[f.name] != p.values[f.kind] {
			m[f.name] = p.values[f.kind]
			changed = true
		}
	}
	if !changed {
		return false
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return false
	}
	values[openAIWSTurnMetadataHeader] = string(raw)
	return true
}

func (p *codexRequestIdentity) applyBody(body map[string]any) bool {
	if p == nil || body == nil {
		return false
	}
	changed := false
	if cm := codexIdentityMetadata(body); cm != nil {
		cm = maps.Clone(cm)
		changed = p.applyFields(cm, false)
		for _, f := range codexRequestIdentityFields {
			if strings.Contains(f.name, "_") && p.values[f.kind] != "" && cm[f.name] != p.values[f.kind] {
				cm[f.name] = p.values[f.kind]
				changed = true
			}
		}
		if v := p.values["window"]; v != "" && cm["x-codex-window-id"] != v {
			cm["x-codex-window-id"] = v
			changed = true
		}
		if p.values["installation"] != "" && cm["x-codex-installation-id"] != p.values["installation"] {
			cm["x-codex-installation-id"] = p.values["installation"]
			changed = true
		}
		if p.applyEmbedded(cm) {
			changed = true
		}
		if changed {
			body["client_metadata"] = cm
		}
	}
	if p.cacheKey != "" && body["prompt_cache_key"] != p.cacheKey {
		body["prompt_cache_key"] = p.cacheKey
		changed = true
	}
	return changed
}

func (p *codexRequestIdentity) applyHeaders(headers http.Header) {
	if p == nil || headers == nil {
		return
	}
	for _, f := range codexRequestIdentityFields {
		if p.omitted[f.kind] {
			headers.Del(f.name)
			continue
		}
		if f.name == "session_id" && !p.scoped {
			continue
		}
		if v := p.values[f.kind]; v != "" && (headers.Get(f.name) != "" || f.name == "x-codex-installation-id") {
			headers.Set(f.name, v)
		}
	}
	for _, f := range []struct{ name, kind string }{{"thread-id", "thread"}, {"turn-id", "turn"}, {"parent-thread-id", "parent-thread"}, {"parent-turn-id", "parent-turn"}, {"root-turn-id", "root-turn"}, {"x-codex-window-id", "window"}, {"context-window-id", "context-window"}, {"x-client-request-id", "request"}} {
		if v := p.values[f.kind]; v != "" {
			headers.Set(f.name, v)
		}
	}
	// Reconcile both session spellings when the client supplied a logical session.
	if v := p.values["session"]; v != "" {
		headers.Set("session-id", v)
		// Without a stable credential namespace, retain the builder's existing
		// downstream API-key isolation rather than replacing it with a raw ID.
		if p.scoped {
			headers.Set("session_id", v)
		}
	}
	if v := p.values["parent-thread"]; v != "" {
		headers.Set("x-codex-parent-thread-id", v)
	}
	metadata := map[string]any{openAIWSTurnMetadataHeader: headers.Get(openAIWSTurnMetadataHeader)}
	if p.applyEmbedded(metadata) {
		if value, ok := metadata[openAIWSTurnMetadataHeader].(string); ok {
			headers.Set(openAIWSTurnMetadataHeader, value)
		}
	}
}

// Builders may receive a projected body; routing isolation must start from
// the original cache key just as it does in the decoded HTTP path.
func codexOriginalPromptCacheKey(c *gin.Context, account *Account, fallback string) string {
	if p := stagedCodexRequestIdentity(c, account); p != nil && p.originalCacheKey != "" {
		return p.originalCacheKey
	}
	return fallback
}

func stageCodexRequestIdentity(c *gin.Context, account *Account, body map[string]any) *codexRequestIdentity {
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header
	}
	p := newCodexRequestIdentity(account, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c), headers, body)
	if c != nil {
		c.Set(codexRequestIdentityContextKey, p)
	}
	return p
}

func stagedCodexRequestIdentity(c *gin.Context, account *Account) *codexRequestIdentity {
	if c == nil || account == nil {
		return nil
	}
	value, _ := c.Get(codexRequestIdentityContextKey)
	p, _ := value.(*codexRequestIdentity)
	if p == nil || p.accountID != account.ID {
		return nil
	}
	return p
}

func projectCodexRequestBody(c *gin.Context, account *Account, body map[string]any) bool {
	return stageCodexRequestIdentity(c, account, body).applyBody(body)
}

func projectCodexRequestBodyRaw(c *gin.Context, account *Account, body []byte) ([]byte, bool, error) {
	return projectCodexIdentityBodyRaw(c, account, body, false)
}

func projectCodexWSRequestBodyRaw(c *gin.Context, account *Account, body []byte) ([]byte, bool, error) {
	return projectCodexIdentityBodyRaw(c, account, body, true)
}

func projectCodexIdentityBodyRaw(c *gin.Context, account *Account, body []byte, frame bool) ([]byte, bool, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}
	small := make(map[string]any)
	for _, key := range []string{"client_metadata", "prompt_cache_key"} {
		value := root.Get(key)
		if value.Exists() {
			var decoded any
			if err := json.Unmarshal([]byte(value.Raw), &decoded); err != nil {
				return body, false, err
			}
			small[key] = decoded
		}
	}
	var headers http.Header
	if c != nil && c.Request != nil {
		headers = c.Request.Header
	}
	p := resolveCodexRequestIdentity(account, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c), headers, small)
	if err := resolveCodexDailySessionProjection(c, account, p); err != nil {
		return body, false, err
	}
	if c != nil {
		c.Set(codexRequestIdentityContextKey, p)
	}
	// Only after resolving the original frame may a compatibility handshake
	// blob be copied into its payload. Promoting it earlier would give stale
	// handshake identity canonical precedence over this frame's flat keys.
	added := false
	if frame && p != nil && strings.TrimSpace(headers.Get(openAIWSTurnMetadataHeader)) != "" {
		cm := codexIdentityMetadata(small)
		if _, exists := cm[openAIWSTurnMetadataHeader]; !exists {
			setOpenAIWSTurnMetadata(small, headers.Get(openAIWSTurnMetadataHeader))
			added = true
		}
	}
	if !p.applyBody(small) && !added {
		return body, false, nil
	}
	next := body
	for key, value := range small {
		var err error
		next, err = sjson.SetBytes(next, key, value)
		if err != nil {
			return body, false, err
		}
	}
	return next, true, nil
}

func applyCodexRequestHeaders(c *gin.Context, account *Account, headers http.Header) {
	p := stagedCodexRequestIdentity(c, account)
	if p == nil {
		var original http.Header
		if c != nil && c.Request != nil {
			original = c.Request.Header
		}
		p = newCodexRequestIdentity(account, codexAccountIdentitySource(c, account), getAPIKeyIDFromContext(c), original, nil)
	}
	p.applyHeaders(headers)
}
