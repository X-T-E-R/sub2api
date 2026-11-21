package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
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

// antigravityMalformedRetryKey hashes the caller scope and the full Claude history
// (system, tools, every message) without materializing a serialized copy of it.
// Raw JSON pieces are compacted one at a time so whitespace differences between an
// initial request and its retry still map to the same entry; the scratch buffer is
// bounded by the largest single piece instead of the whole multi-MiB history.
func antigravityMalformedRetryKey(apiKeyID int64, session, wireModel string, req *antigravity.ClaudeRequest) ([sha256.Size]byte, error) {
	h := sha256.New()
	var scratch bytes.Buffer
	var lenPrefix [8]byte
	writeField := func(b []byte) {
		binary.BigEndian.PutUint64(lenPrefix[:], uint64(len(b)))
		_, _ = h.Write(lenPrefix[:])
		_, _ = h.Write(b)
	}
	writeRaw := func(raw json.RawMessage) error {
		if len(bytes.TrimSpace(raw)) == 0 {
			writeField([]byte("null"))
			return nil
		}
		scratch.Reset()
		if err := json.Compact(&scratch, raw); err != nil {
			return err
		}
		writeField(scratch.Bytes())
		return nil
	}
	writeField([]byte(strconv.FormatInt(apiKeyID, 10)))
	writeField([]byte(session))
	writeField([]byte(req.Model))
	writeField([]byte(wireModel))
	if err := writeRaw(req.System); err != nil {
		return [sha256.Size]byte{}, err
	}
	tools, err := json.Marshal(req.Tools)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	writeField(tools)
	writeField([]byte(strconv.Itoa(len(req.Messages))))
	for i := range req.Messages {
		writeField([]byte(req.Messages[i].Role))
		if err := writeRaw(req.Messages[i].Content); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key, nil
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
	key, err := antigravityMalformedRetryKey(apiKeyID, session, model, req)
	if err != nil {
		return nil, nil, err
	}
	s.malformedFunctionRetriesOnce.Do(func() {
		if s.malformedFunctionRetries == nil {
			s.malformedFunctionRetries = newAntigravityMalformedRetryCache()
		}
	})
	recovery := &antigravityMalformedRecovery{cache: s.malformedFunctionRetries, key: key, effectiveMode: mode}
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

// reapply restores active malformed-function recovery rewrites (mode relaxation
// and, for stage-2 repeated failures, the single-tool recovery note) onto a
// body rebuilt after a signature or budget 400 retry.
func (r *antigravityMalformedRecovery) reapply(body []byte) ([]byte, error) {
	if r == nil || r.autoReason == "" {
		return body, nil
	}
	var err error
	if gjson.GetBytes(body, antigravityFunctionModePath).String() != "AUTO" {
		body, err = antigravityAutoFunctionMode(body)
		if err != nil {
			return nil, err
		}
	}
	if r.autoReason == antigravityMalformedSingleToolReason {
		body, err = appendAntigravityMalformedSingleToolNote(body)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
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
