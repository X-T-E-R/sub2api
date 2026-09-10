package service

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const openAIPoolResetPersistenceProcessMemory = "process_memory"

// OpenAIPoolResetEvent records one eligible capacity-shed reset decision.
// Triggered is true only when the upstream pool actually accepted the reset;
// cooldown, stale-token, and unavailable-resetter decisions are suppressed.
type OpenAIPoolResetEvent struct {
	AccountID int64
	Protocol  string
	Triggered bool
	At        time.Time
}

// OpenAIPoolResetAccountSummary contains reset decisions for one account.
type OpenAIPoolResetAccountSummary struct {
	AccountID   int64      `json:"account_id"`
	Triggered   uint64     `json:"triggered"`
	Suppressed  uint64     `json:"suppressed"`
	LastResetAt *time.Time `json:"last_reset_at,omitempty"`
}

// OpenAIPoolResetProtocolSummary contains reset decisions for one transport
// protocol (for example openai_h1 or openai_h2).
type OpenAIPoolResetProtocolSummary struct {
	Protocol    string     `json:"protocol"`
	Triggered   uint64     `json:"triggered"`
	Suppressed  uint64     `json:"suppressed"`
	LastResetAt *time.Time `json:"last_reset_at,omitempty"`
}

// OpenAIPoolResetStats is the process-local snapshot exposed to the Ops UI.
// The counters intentionally reset with the process because reset events do
// not have a durable source and are not written to the request/usage path.
type OpenAIPoolResetStats struct {
	Triggered      uint64                           `json:"triggered"`
	Suppressed     uint64                           `json:"suppressed"`
	LastResetAt    *time.Time                       `json:"last_reset_at,omitempty"`
	ByAccount      []OpenAIPoolResetAccountSummary  `json:"by_account"`
	ByProtocol     []OpenAIPoolResetProtocolSummary `json:"by_protocol"`
	Persistence    string                           `json:"persistence"`
	ResetOnRestart bool                             `json:"reset_on_restart"`
}

// OpenAIPoolResetStatsStore is a concurrency-safe in-memory accumulator.
// Keeping the store separate makes the update/snapshot contract testable while
// the package-level store provides one process-wide view for production.
type OpenAIPoolResetStatsStore struct {
	mu         sync.RWMutex
	triggered  uint64
	suppressed uint64
	lastReset  time.Time
	accounts   map[int64]*OpenAIPoolResetAccountSummary
	protocols  map[string]*OpenAIPoolResetProtocolSummary
}

// NewOpenAIPoolResetStatsStore creates an empty process-local accumulator.
func NewOpenAIPoolResetStatsStore() *OpenAIPoolResetStatsStore {
	return &OpenAIPoolResetStatsStore{
		accounts:  make(map[int64]*OpenAIPoolResetAccountSummary),
		protocols: make(map[string]*OpenAIPoolResetProtocolSummary),
	}
}

// Record adds one reset decision to the accumulator.
func (s *OpenAIPoolResetStatsStore) Record(event OpenAIPoolResetEvent) {
	if s == nil {
		return
	}
	protocol := normalizeOpenAIPoolResetProtocol(event.Protocol)
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts == nil {
		s.accounts = make(map[int64]*OpenAIPoolResetAccountSummary)
	}
	if s.protocols == nil {
		s.protocols = make(map[string]*OpenAIPoolResetProtocolSummary)
	}

	account := s.accounts[event.AccountID]
	if account == nil {
		account = &OpenAIPoolResetAccountSummary{AccountID: event.AccountID}
		s.accounts[event.AccountID] = account
	}
	protocolSummary := s.protocols[protocol]
	if protocolSummary == nil {
		protocolSummary = &OpenAIPoolResetProtocolSummary{Protocol: protocol}
		s.protocols[protocol] = protocolSummary
	}

	if event.Triggered {
		s.triggered++
		account.Triggered++
		protocolSummary.Triggered++
		if account.LastResetAt == nil || at.After(*account.LastResetAt) {
			account.LastResetAt = timePointer(at)
		}
		if protocolSummary.LastResetAt == nil || at.After(*protocolSummary.LastResetAt) {
			protocolSummary.LastResetAt = timePointer(at)
		}
		if s.lastReset.IsZero() || at.After(s.lastReset) {
			s.lastReset = at
		}
		return
	}

	s.suppressed++
	account.Suppressed++
	protocolSummary.Suppressed++
}

// Snapshot returns a stable, sorted copy suitable for JSON encoding.
func (s *OpenAIPoolResetStatsStore) Snapshot() OpenAIPoolResetStats {
	result := OpenAIPoolResetStats{
		ByAccount:      []OpenAIPoolResetAccountSummary{},
		ByProtocol:     []OpenAIPoolResetProtocolSummary{},
		Persistence:    openAIPoolResetPersistenceProcessMemory,
		ResetOnRestart: true,
	}
	if s == nil {
		return result
	}

	s.mu.RLock()
	result.Triggered = s.triggered
	result.Suppressed = s.suppressed
	if !s.lastReset.IsZero() {
		result.LastResetAt = timePointer(s.lastReset)
	}
	for _, value := range s.accounts {
		if value == nil {
			continue
		}
		copyValue := *value
		copyValue.LastResetAt = cloneTimePointer(value.LastResetAt)
		result.ByAccount = append(result.ByAccount, copyValue)
	}
	for _, value := range s.protocols {
		if value == nil {
			continue
		}
		copyValue := *value
		copyValue.LastResetAt = cloneTimePointer(value.LastResetAt)
		result.ByProtocol = append(result.ByProtocol, copyValue)
	}
	s.mu.RUnlock()

	sort.Slice(result.ByAccount, func(i, j int) bool {
		left, right := result.ByAccount[i], result.ByAccount[j]
		leftTotal := left.Triggered + left.Suppressed
		rightTotal := right.Triggered + right.Suppressed
		if leftTotal != rightTotal {
			return leftTotal > rightTotal
		}
		return left.AccountID < right.AccountID
	})
	sort.Slice(result.ByProtocol, func(i, j int) bool {
		left, right := result.ByProtocol[i], result.ByProtocol[j]
		leftTotal := left.Triggered + left.Suppressed
		rightTotal := right.Triggered + right.Suppressed
		if leftTotal != rightTotal {
			return leftTotal > rightTotal
		}
		return left.Protocol < right.Protocol
	})
	return result
}

var processOpenAIPoolResetStats = NewOpenAIPoolResetStatsStore()

// RecordOpenAIPoolResetEvent records an eligible reset decision in the
// process-level snapshot used by the Ops monitoring endpoint.
func RecordOpenAIPoolResetEvent(event OpenAIPoolResetEvent) {
	processOpenAIPoolResetStats.Record(event)
}

// SnapshotOpenAIPoolResetStats returns the current process-level snapshot.
func SnapshotOpenAIPoolResetStats() OpenAIPoolResetStats {
	return processOpenAIPoolResetStats.Snapshot()
}

// GetOpenAIPoolResetStats makes the process snapshot available through the
// existing OpsService query surface. The caller still applies the usual Ops
// monitoring authorization/enablement guard.
func (s *OpsService) GetOpenAIPoolResetStats() OpenAIPoolResetStats {
	return SnapshotOpenAIPoolResetStats()
}

func normalizeOpenAIPoolResetProtocol(protocol string) string {
	protocol = strings.TrimSpace(protocol)
	if protocol == "" {
		return "unknown"
	}
	return protocol
}

func openAIPoolResetProtocol(token HTTPUpstreamPoolEntryToken, resp *http.Response) string {
	if token.CacheKey != "" {
		const marker = "|proto:"
		if index := strings.LastIndex(token.CacheKey, marker); index >= 0 {
			if protocol := strings.TrimSpace(token.CacheKey[index+len(marker):]); protocol != "" {
				return normalizeOpenAIPoolResetProtocol(protocol)
			}
		}
	}
	if resp != nil && resp.Request != nil {
		return normalizeOpenAIPoolResetProtocol(string(HTTPUpstreamProfileFromContext(resp.Request.Context())))
	}
	return "unknown"
}

func timePointer(value time.Time) *time.Time {
	copyValue := value
	return &copyValue
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}
