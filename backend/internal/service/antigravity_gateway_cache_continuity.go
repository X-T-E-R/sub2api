package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	antigravityContinuityEntries   = 512
	antigravityContinuityContents  = 256
	antigravityContinuityBodyLimit = 8 << 20
	// These are local observation limits, NOT claims about provider cache TTL.
	antigravityContinuityRetention = 2 * time.Hour
	antigravityContinuityShortGap  = 5 * time.Minute
)

type antigravityCacheDigest [sha256.Size]byte

type antigravityCacheContent struct {
	fingerprint antigravityCacheDigest
	bytes       int
}

type antigravityCacheSnapshot struct {
	system, tools, config, model, effort, account, project, session antigravityCacheDigest
	contents                                                        []antigravityCacheContent
	contentBytes                                                    int
	syntheticTail                                                   bool
}

type antigravityCacheBaseline struct {
	snapshot       antigravityCacheSnapshot
	prompt, cached int
}

type antigravityCacheEntry struct {
	lastStart time.Time
	active    int
	overlap   bool
	baseline  *antigravityCacheBaseline
	timer     *time.Timer
}

// Only fixed classifications, numbers, booleans and process-salted HMACs may be
// emitted here. Never add raw identifiers, model aliases or payload fragments.
// HasPrevious/Comparable qualify the *_same fields; false alone is not evidence.
type antigravityCacheEvent struct {
	Class                 string `json:"class"`
	Reason                string `json:"reason"`
	ScopeFP               string `json:"scope_fp,omitempty"`
	AccountFP             string `json:"account_fp,omitempty"`
	ModelFP               string `json:"model_fp,omitempty"`
	EffortFP              string `json:"effort_fp,omitempty"`
	HasPrevious           bool   `json:"has_previous"`
	Comparable            bool   `json:"comparable"`
	IntervalMS            int64  `json:"interval_ms"`
	PromptTokens          int    `json:"prompt_tokens"`
	UncachedTokens        int    `json:"uncached_tokens"`
	CachedTokens          int    `json:"cached_tokens"`
	CacheWriteTokens      int    `json:"cache_write_tokens"`
	UsagePresent          bool   `json:"usage_present"`
	AccountChanged        bool   `json:"account_changed"`
	ModelChanged          bool   `json:"model_changed"`
	EffortChanged         bool   `json:"effort_changed"`
	WireSessionChanged    bool   `json:"wire_session_changed"`
	SystemSame            bool   `json:"system_same"`
	ToolsSame             bool   `json:"tools_same"`
	ConfigSame            bool   `json:"config_same"`
	PrefixSame            bool   `json:"prefix_same"`
	RepeatRequest         bool   `json:"repeat_request"`
	PreviousContents      int    `json:"previous_contents"`
	CurrentContents       int    `json:"current_contents"`
	MatchedContents       int    `json:"matched_contents"`
	AppendedContents      int    `json:"appended_contents"`
	AppendedWireBytes     int    `json:"appended_wire_bytes"`
	PromptGrowthTokens    int    `json:"prompt_growth_tokens"`
	AppendedInputDominant bool   `json:"appended_input_dominant"`
	ReadDroppedToZero     bool   `json:"read_dropped_to_zero"`
}

// Per-service, bounded, no persistence, raw session or request storage.
// In-flight markers cannot be evicted: overload becomes unknown instead of losing
// concurrency detection. One expiry timer per entry removes idle fingerprints even
// without subsequent traffic; an expired in-flight marker has no cached baseline.
type antigravityCacheContinuity struct {
	mu        sync.Mutex
	salt      [32]byte
	entries   map[antigravityCacheDigest]*antigravityCacheEntry
	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer
	emit      func(antigravityCacheEvent)
}

type antigravityCacheTicket struct {
	tracker  *antigravityCacheContinuity
	key      antigravityCacheDigest
	entry    *antigravityCacheEntry
	previous *antigravityCacheBaseline
	current  antigravityCacheSnapshot
	observed bool
	event    antigravityCacheEvent
}

func (o *antigravityCacheContinuity) fingerprint(domain string, values ...string) antigravityCacheDigest {
	h := hmac.New(sha256.New, o.salt[:])
	var size [8]byte
	for _, value := range append([]string{domain}, values...) {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	var result antigravityCacheDigest
	copy(result[:], h.Sum(nil))
	return result
}

func cacheDigestText(d antigravityCacheDigest) string { return hex.EncodeToString(d[:]) }

func (s *AntigravityGatewayService) beginAntigravityCacheObservation(ctx context.Context, c *gin.Context, account *Account, original, wire []byte) *antigravityCacheTicket {
	// This hook is intentionally only in OAuth Messages Forward, not TestConnection,
	// raw Gemini, other protocols/platforms, scheduling or the billing usage path.
	if !strings.HasPrefix(gjson.GetBytes(wire, "model").String(), "gemini-") {
		return nil
	}
	s.cacheContinuityOnce.Do(func() {
		o := &antigravityCacheContinuity{entries: make(map[antigravityCacheDigest]*antigravityCacheEntry), now: time.Now, afterFunc: time.AfterFunc}
		if _, err := rand.Read(o.salt[:]); err != nil {
			return // No unsalted or predictable fallback.
		}
		o.emit = func(event antigravityCacheEvent) {
			encoded, _ := json.Marshal(event)
			logger.LegacyPrintf("service.antigravity_gateway", "cache_continuity %s", encoded)
		}
		s.cacheContinuity = o
	})
	if s.cacheContinuity == nil {
		return nil
	}
	return s.cacheContinuity.begin(c, account, antigravityRequestEffort(ctx), original, wire)
}

func (o *antigravityCacheContinuity) begin(c *gin.Context, account *Account, effort string, original, wire []byte) *antigravityCacheTicket {
	t := &antigravityCacheTicket{tracker: o, event: antigravityCacheEvent{Class: "unknown", Reason: "missing_scope", IntervalMS: -1}}
	if c == nil || account == nil {
		return t
	}
	value, _ := c.Get("api_key") // authenticated middleware contract; no secret key is read
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.UserID <= 0 || key.ID <= 0 {
		return t
	}
	session := getSessionID(c)
	// Only explicit identities may correlate requests. The converter's first-user-
	// text fallback is not a reliable session identity and is deliberately not used.
	if session == "" {
		session = gjson.GetBytes(original, "metadata.user_id").String()
	}
	if session == "" {
		t.event.Reason = "missing_session"
		return t
	}
	if len(session) > 4096 {
		t.event.Reason = "observation_limit"
		return t
	}
	group := int64(0)
	if key.GroupID != nil {
		group = *key.GroupID
	}
	t.key = o.fingerprint("scope", strconv.FormatInt(key.UserID, 10), strconv.FormatInt(key.ID, 10), strconv.FormatInt(group, 10), session)
	t.event.ScopeFP = cacheDigestText(t.key)
	// Register before fingerprinting, so overlapping requests cannot appear serial.
	// Even an unobservable request invalidates this session's old baseline.
	o.attach(t)
	if t.entry == nil || t.event.Reason == "concurrent_requests" {
		return t
	}
	if len(effort) > 256 || len(wire) > antigravityContinuityBodyLimit {
		t.event.Reason = "observation_limit"
		return t
	}
	root := gjson.ParseBytes(wire)
	inner := root.Get("request")
	if !inner.IsObject() || !inner.Get("contents").IsArray() {
		t.event.Reason = "invalid_envelope"
		return t
	}
	fp := func(domain, value string) antigravityCacheDigest {
		return o.fingerprint(domain, t.event.ScopeFP, value)
	}
	snap := antigravityCacheSnapshot{
		system: fp("system", inner.Get("systemInstruction").Raw),
		tools:  fp("tools", inner.Get("tools").Raw),
		config: o.fingerprint("config", t.event.ScopeFP, inner.Get("generationConfig").Raw, inner.Get("toolConfig").Raw, inner.Get("safetySettings").Raw),
		model:  fp("model", root.Get("model").String()), effort: fp("effort", effort),
		account: fp("account", strconv.FormatInt(account.ID, 10)), project: fp("project", root.Get("project").String()),
		session: fp("wire_session", inner.Get("sessionId").String()),
	}
	tooMany := false
	inner.Get("contents").ForEach(func(_, content gjson.Result) bool {
		if len(snap.contents) == antigravityContinuityContents {
			tooMany = true
			return false
		}
		snap.contents = append(snap.contents, antigravityCacheContent{fp("content", content.Raw), len(content.Raw)})
		snap.contentBytes += len(content.Raw)
		return true
	})
	if tooMany {
		t.event.Reason = "observation_limit"
		return t
	}
	last := inner.Get("contents").Array()
	if len(last) > 0 {
		text := last[len(last)-1].Get("parts.0.text").String()
		snap.syntheticTail = strings.HasPrefix(text, "[sub2api:assistant-prefill]\n")
	}
	t.current = snap
	t.event.CurrentContents = len(snap.contents)
	t.event.AccountFP, t.event.ModelFP, t.event.EffortFP = cacheDigestText(snap.account), cacheDigestText(snap.model), cacheDigestText(snap.effort)
	t.observed = true
	return t
}

func (o *antigravityCacheContinuity) attach(t *antigravityCacheTicket) {
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, entry := range o.entries {
		if entry.active == 0 && now.Sub(entry.lastStart) >= antigravityContinuityRetention {
			if entry.timer != nil {
				entry.timer.Stop()
			}
			delete(o.entries, k)
		}
	}
	entry := o.entries[t.key]
	if entry == nil {
		if len(o.entries) >= antigravityContinuityEntries {
			// Evict only the oldest idle entry, never another in-flight observer.
			var oldest antigravityCacheDigest
			var oldestTime time.Time
			for k, candidate := range o.entries {
				if candidate.active == 0 && (oldestTime.IsZero() || candidate.lastStart.Before(oldestTime)) {
					oldest, oldestTime = k, candidate.lastStart
				}
			}
			if oldestTime.IsZero() {
				t.event.Reason = "capacity"
				return
			}
			if timer := o.entries[oldest].timer; timer != nil {
				timer.Stop()
			}
			delete(o.entries, oldest)
		}
		entry = &antigravityCacheEntry{}
		o.entries[t.key] = entry
		t.event.Reason = "first_or_expired"
	} else {
		t.event.IntervalMS = now.Sub(entry.lastStart).Milliseconds()
		t.previous = entry.baseline
		t.event.Reason = "previous_unavailable"
	}
	entry.lastStart = now
	entry.active++
	if entry.active > 1 || entry.overlap {
		entry.overlap = true
		t.event.Reason = "concurrent_requests"
	}
	t.entry = entry
	if entry.timer == nil && o.afterFunc != nil {
		key := t.key
		entry.timer = o.afterFunc(antigravityContinuityRetention, func() { o.expire(key, entry) })
	}
}

func (o *antigravityCacheContinuity) expire(key antigravityCacheDigest, entry *antigravityCacheEntry) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.entries[key] != entry {
		return
	}
	remaining := antigravityContinuityRetention - o.now().Sub(entry.lastStart)
	if remaining <= 0 {
		entry.baseline = nil
		if entry.active == 0 {
			delete(o.entries, key)
			return
		}
		remaining = antigravityContinuityRetention
	}
	if o.afterFunc != nil {
		entry.timer = o.afterFunc(remaining, func() { o.expire(key, entry) })
	}
}

// finish consumes single-generation usage before any billing mutation. Adjusted
// retries and gateway continuations have different request/usage boundaries and
// intentionally invalidate the baseline rather than alleging prefix corruption.
func (t *antigravityCacheTicket) finish(usage *ClaudeUsage, complete, adjusted, continued bool) {
	if t == nil {
		return
	}
	e := &t.event
	if usage != nil {
		e.UsagePresent = true
		e.UncachedTokens, e.CachedTokens, e.CacheWriteTokens = usage.InputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens
		e.PromptTokens = e.UncachedTokens + e.CachedTokens + e.CacheWriteTokens
	}
	o := t.tracker
	if t.entry == nil {
		o.emit(*e)
		return
	}
	o.mu.Lock()
	entry := t.entry
	expired := o.now().Sub(entry.lastStart) >= antigravityContinuityRetention
	valid := t.observed && !expired && complete && usage != nil && e.PromptTokens > 0 && e.UncachedTokens >= 0 && e.CachedTokens >= 0 && e.CacheWriteTokens >= 0
	switch {
	case expired:
		e.Reason = "observation_expired"
	case !t.observed:
		// Preserve the bounded-observation failure reason.
	case entry.overlap:
		e.Reason = "concurrent_requests"
	case adjusted:
		e.Reason = "adjusted_retry"
	case continued:
		e.Reason = "gateway_continuation"
	case !valid:
		e.Reason = "incomplete_or_missing_usage"
	default:
		t.classify()
	}
	entry.active--
	if entry.overlap || adjusted || continued || !valid {
		entry.baseline = nil
	} else {
		entry.baseline = &antigravityCacheBaseline{snapshot: t.current, prompt: e.PromptTokens, cached: e.CachedTokens}
	}
	if entry.active == 0 {
		entry.overlap = false
		if expired {
			if entry.timer != nil {
				entry.timer.Stop()
			}
			delete(o.entries, t.key)
		}
	}
	o.mu.Unlock()
	o.emit(*e)
}

func (t *antigravityCacheTicket) classify() {
	e, prev, cur := &t.event, t.previous, t.current
	if prev == nil {
		if e.Reason == "first_or_expired" {
			e.Class = "first_observation"
			if e.CachedTokens == 0 {
				e.Class = "cold_start_candidate"
			}
		}
		return
	}
	e.HasPrevious = true
	old := prev.snapshot
	e.AccountChanged, e.ModelChanged, e.EffortChanged = old.account != cur.account, old.model != cur.model, old.effort != cur.effort
	e.WireSessionChanged = old.session != cur.session
	e.SystemSame, e.ToolsSame, e.ConfigSame = old.system == cur.system, old.tools == cur.tools, old.config == cur.config
	e.PreviousContents = len(old.contents)
	for i := 0; i < len(old.contents) && i < len(cur.contents) && old.contents[i].fingerprint == cur.contents[i].fingerprint; i++ {
		e.MatchedContents++
	}
	e.PrefixSame = e.MatchedContents == len(old.contents)
	e.RepeatRequest = e.PrefixSame && len(old.contents) == len(cur.contents) && e.SystemSame && e.ToolsSame && e.ConfigSame
	e.PromptGrowthTokens = e.PromptTokens - prev.prompt
	e.ReadDroppedToZero = prev.cached > 0 && e.CachedTokens == 0
	if e.PrefixSame {
		e.AppendedContents = len(cur.contents) - len(old.contents)
		e.AppendedWireBytes = cur.contentBytes - old.contentBytes
		e.AppendedInputDominant = e.AppendedContents > 0 && e.PromptGrowthTokens >= prev.prompt
	}
	switch {
	case e.IntervalMS < 0:
		e.Reason = "clock_order"
	case time.Duration(e.IntervalMS)*time.Millisecond > antigravityContinuityShortGap:
		e.Class, e.Reason = "long_gap", "long_observation_gap"
		if e.CachedTokens == 0 {
			e.Class = "cold_start_candidate"
		}
	case e.AccountChanged || e.ModelChanged || e.EffortChanged || e.WireSessionChanged || old.project != cur.project || !e.ConfigSame:
		e.Class, e.Reason = "context_changed", "execution_context_changed"
	case old.syntheticTail || cur.syntheticTail:
		e.Reason = "compatibility_prefill"
	default:
		e.Comparable = true
		switch {
		case !e.SystemSame || !e.ToolsSame || !e.PrefixSame:
			e.Class, e.Reason = "short_prefix_changed", "observed_request_difference"
		case e.AppendedInputDominant:
			e.Class, e.Reason = "appended_input_dominant", "prompt_growth_at_least_previous_prompt"
		case e.ReadDroppedToZero:
			e.Class, e.Reason = "upstream_miss_candidate", "stable_prefix_read_dropped_to_zero"
		default:
			e.Class, e.Reason = "stable_prefix", "no_zero_read_drop"
		}
	}
}
