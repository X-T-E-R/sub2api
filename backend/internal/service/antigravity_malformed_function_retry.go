package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityMalformedRetryTTL         = 5 * time.Minute
	antigravityMalformedRetryLimit       = 4096
	antigravityFunctionModePath          = "request.toolConfig.functionCallingConfig.mode"
	antigravityMalformedSingleToolReason = "repeated_malformed_function_call_single_tool"
	antigravityMalformedSingleToolNote   = "[System recovery note: Previous attempts failed upstream with MALFORMED_FUNCTION_CALL while serializing multiple or complex tool arguments. On this retry, emit at most ONE tool call for the single highest-priority next action with minimal, cleanly escaped JSON arguments, and defer any other tool calls to the next step.]"
)

type antigravityMalformedRetryEntry struct {
	count   int
	expires time.Time
}

// Only fingerprints, failure counts and expiry times are retained. The pointer
// is shared by service transport copies.
type antigravityMalformedRetryCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]antigravityMalformedRetryEntry
	now     func() time.Time
}

func newAntigravityMalformedRetryCache() *antigravityMalformedRetryCache {
	return &antigravityMalformedRetryCache{entries: make(map[[sha256.Size]byte]antigravityMalformedRetryEntry), now: time.Now}
}

func (m *antigravityMalformedRetryCache) count(key [sha256.Size]byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if ok && !entry.expires.After(m.now()) {
		delete(m.entries, key)
		return 0
	}
	return entry.count
}

func (m *antigravityMalformedRetryCache) mark(key [sha256.Size]byte, effectiveMode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var oldest [sha256.Size]byte
	var oldestExpiry time.Time
	for k, entry := range m.entries {
		if !entry.expires.After(now) {
			delete(m.entries, k)
		} else if oldestExpiry.IsZero() || entry.expires.Before(oldestExpiry) {
			oldest, oldestExpiry = k, entry.expires
		}
	}
	prev, exists := m.entries[key]
	if !exists && len(m.entries) >= antigravityMalformedRetryLimit {
		delete(m.entries, oldest)
	}
	count := prev.count + 1
	if effectiveMode == "AUTO" {
		count = max(count, 2)
	}
	m.entries[key] = antigravityMalformedRetryEntry{count: count, expires: now.Add(antigravityMalformedRetryTTL)}
}

func (m *antigravityMalformedRetryCache) clear(key [sha256.Size]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

type antigravityMalformedRecovery struct {
	cache         *antigravityMalformedRetryCache
	key           [sha256.Size]byte
	effectiveMode string
	autoReason    string
}

// A constrained Messages tool_choice and the native Gemini configuration
// contract are never rewritten by this recovery.
func (s *AntigravityGatewayService) prepareMalformedFunctionRecovery(c *gin.Context, req *antigravity.ClaudeRequest, body []byte) ([]byte, *antigravityMalformedRecovery, error) {
	choice := gjson.ParseBytes(bytes.TrimSpace(req.ToolChoice))
	kind := choice.String()
	if choice.IsObject() {
		kind = choice.Get("type").String()
	}
	model := gjson.GetBytes(body, "model").String()
	mode := gjson.GetBytes(body, antigravityFunctionModePath).String()
	if (choice.Type != gjson.Null && !strings.EqualFold(strings.TrimSpace(kind), "auto")) ||
		!strings.HasPrefix(model, "gemini-") || (mode != "VALIDATED" && mode != "AUTO") ||
		gjson.GetBytes(body, "request.toolConfig.functionCallingConfig.allowedFunctionNames.#").Int() != 0 {
		return body, nil, nil
	}
	apiKeyID := int64(0)
	if value, exists := c.Get("api_key"); exists {
		if key, ok := value.(*APIKey); ok && key != nil {
			apiKeyID = key.ID
		}
	}
	session := GatewaySessionIDFromHeaders(c)
	if session == "" && req.Metadata != nil {
		session = req.Metadata.UserID
	}
	// Key on the request's history, not tool_choice: an explicit AUTO retry
	// must find the same failure entry as the initial omitted choice.
	input, err := json.Marshal([]any{strconv.FormatInt(apiKeyID, 10), session, req.Model,
		model, req.System, req.Tools, req.Messages})
	if err != nil {
		return nil, nil, err
	}
	s.malformedFunctionRetriesOnce.Do(func() {
		if s.malformedFunctionRetries == nil {
			s.malformedFunctionRetries = newAntigravityMalformedRetryCache()
		}
	})
	recovery := &antigravityMalformedRecovery{cache: s.malformedFunctionRetries, key: sha256.Sum256(input), effectiveMode: mode}
	count := recovery.cache.count(recovery.key)
	if count == 0 || (count == 1 && mode == "AUTO") {
		return body, recovery, nil
	}
	if mode != "AUTO" {
		body, err = antigravityAutoFunctionMode(body)
		if err != nil {
			return nil, nil, err
		}
		recovery.effectiveMode = "AUTO"
	}
	if count == 1 {
		recovery.autoReason = "recent_malformed_function_call"
	} else {
		body, err = appendAntigravityMalformedSingleToolNote(body)
		recovery.autoReason = antigravityMalformedSingleToolReason
	}
	return body, recovery, err
}

// sjson replaces only the mode token. Cache-bearing tools/system/contents and
// every other byte of the prepared envelope remain unchanged.
func antigravityAutoFunctionMode(body []byte) ([]byte, error) {
	return sjson.SetBytes(body, antigravityFunctionModePath, "AUTO")
}

func appendAntigravityMalformedSingleToolNote(body []byte) ([]byte, error) {
	contents := gjson.GetBytes(body, "request.contents").Array()
	if len(contents) == 0 {
		return body, nil
	}
	target := len(contents) - 1
	for i := target; i >= 0; i-- {
		if contents[i].Get("role").String() == "user" {
			target = i
			break
		}
	}
	return sjson.SetBytes(body, "request.contents."+strconv.Itoa(target)+".parts.-1", antigravity.GeminiPart{Text: antigravityMalformedSingleToolNote})
}

func (r *antigravityMalformedRecovery) finish(g *antigravityThoughtGeneration, successful bool) {
	if r == nil {
		return
	}
	if g != nil && g.finishReason == "MALFORMED_FUNCTION_CALL" {
		r.cache.mark(r.key, r.effectiveMode)
	} else if successful {
		r.cache.clear(r.key)
	}
}
