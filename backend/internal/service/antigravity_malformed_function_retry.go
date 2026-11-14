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
	antigravityMalformedRetryTTL   = 5 * time.Minute
	antigravityMalformedRetryLimit = 4096
	antigravityFunctionModePath    = "request.toolConfig.functionCallingConfig.mode"
)

// Only request fingerprints and expiry times are retained; no prompt or session
// payloads. The pointer is shared by service transport copies.
type antigravityMalformedRetryCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]time.Time
	now     func() time.Time
}

func newAntigravityMalformedRetryCache() *antigravityMalformedRetryCache {
	return &antigravityMalformedRetryCache{entries: make(map[[sha256.Size]byte]time.Time), now: time.Now}
}

func (m *antigravityMalformedRetryCache) contains(key [sha256.Size]byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	expires, ok := m.entries[key]
	if ok && !expires.After(m.now()) {
		delete(m.entries, key)
		return false
	}
	return ok
}

func (m *antigravityMalformedRetryCache) mark(key [sha256.Size]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var oldest [sha256.Size]byte
	var oldestExpiry time.Time
	for k, expires := range m.entries {
		if !expires.After(now) {
			delete(m.entries, k)
		} else if oldestExpiry.IsZero() || expires.Before(oldestExpiry) {
			oldest, oldestExpiry = k, expires
		}
	}
	if _, exists := m.entries[key]; !exists && len(m.entries) >= antigravityMalformedRetryLimit {
		delete(m.entries, oldest)
	}
	m.entries[key] = now.Add(antigravityMalformedRetryTTL)
}

func (m *antigravityMalformedRetryCache) clear(key [sha256.Size]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

type antigravityMalformedRecovery struct {
	cache      *antigravityMalformedRetryCache
	key        [sha256.Size]byte
	autoReason string
}

// This recovery belongs only to omitted Messages tool_choice, never an explicit
// NONE/ANY/named tool or the native Gemini configuration contract.
func (s *AntigravityGatewayService) prepareMalformedFunctionRecovery(c *gin.Context, req *antigravity.ClaudeRequest, body []byte) ([]byte, *antigravityMalformedRecovery, error) {
	choice := bytes.TrimSpace(req.ToolChoice)
	if (len(choice) != 0 && !bytes.Equal(choice, []byte("null"))) ||
		!strings.HasPrefix(gjson.GetBytes(body, "model").String(), "gemini-") ||
		gjson.GetBytes(body, antigravityFunctionModePath).String() != "VALIDATED" ||
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
	// Full history and tool/system declarations prevent unrelated turns with the
	// same final message from inheriting a decoder failure. Model/key scope also
	// isolates clients without an explicit conversation identity.
	input, err := json.Marshal([]any{strconv.FormatInt(apiKeyID, 10), session, req.Model,
		gjson.GetBytes(body, "model").String(), req.System, req.Tools, req.Messages})
	if err != nil {
		return nil, nil, err
	}
	s.malformedFunctionRetriesOnce.Do(func() {
		if s.malformedFunctionRetries == nil {
			s.malformedFunctionRetries = newAntigravityMalformedRetryCache()
		}
	})
	recovery := &antigravityMalformedRecovery{cache: s.malformedFunctionRetries, key: sha256.Sum256(input)}
	if recovery.cache.contains(recovery.key) {
		body, err = antigravityAutoFunctionMode(body)
		recovery.autoReason = "recent_malformed_function_call"
	}
	return body, recovery, err
}

// sjson replaces only the mode token. Cache-bearing tools/system/contents and
// every other byte of the prepared envelope remain unchanged.
func antigravityAutoFunctionMode(body []byte) ([]byte, error) {
	return sjson.SetBytes(body, antigravityFunctionModePath, "AUTO")
}

func (r *antigravityMalformedRecovery) finish(g *antigravityThoughtGeneration, successful bool) {
	if r == nil || g == nil {
		return
	}
	if g.finishReason == "MALFORMED_FUNCTION_CALL" {
		r.cache.mark(r.key)
	} else if successful {
		r.cache.clear(r.key)
	}
}
