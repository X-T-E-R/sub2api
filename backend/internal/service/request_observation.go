package service

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	requestObservationContextKey = "request_observation"
	maxRequestAttemptLedgerItems = 32
	requestAttemptLedgerEdgeSize = maxRequestAttemptLedgerItems / 2
)

// RequestAttemptEvidence is a bounded, content-free record of one Forward call.
// It intentionally contains no request/response body, headers, URL, credentials,
// or raw error text.
type RequestAttemptEvidence struct {
	Sequence           int                       `json:"sequence"`
	AccountID          int64                     `json:"account_id"`
	Platform           string                    `json:"platform,omitempty"`
	StartedOffsetMs    int                       `json:"started_offset_ms"`
	SelectionMs        int                       `json:"selection_ms"`
	SlotWaitMs         int                       `json:"slot_wait_ms"`
	ForwardMs          int                       `json:"forward_ms"`
	StatusCode         *int                      `json:"status_code,omitempty"`
	Outcome            string                    `json:"outcome"`
	Stage              string                    `json:"stage,omitempty"`
	Scope              string                    `json:"scope,omitempty"`
	Reason             string                    `json:"reason,omitempty"`
	NextAction         string                    `json:"next_action,omitempty"`
	WaitAfterMs        int                       `json:"wait_after_ms,omitempty"`
	UpstreamRequestID  string                    `json:"upstream_request_id,omitempty"`
	TerminalKind       string                    `json:"terminal_kind,omitempty"`
	SemanticOutputSeen bool                      `json:"semantic_output_seen"`
	CyberSession       *CyberSessionBlockReceipt `json:"cyber_session,omitempty"`
}

type RequestAttemptFoldedEvidence struct {
	Count       int            `json:"count"`
	ForwardMs   int            `json:"forward_ms"`
	ReasonCount map[string]int `json:"reason_count,omitempty"`
}

type RequestAttemptLedger struct {
	Version       int                           `json:"version"`
	TotalAttempts int                           `json:"total_attempts"`
	Truncated     bool                          `json:"truncated"`
	Folded        *RequestAttemptFoldedEvidence `json:"folded,omitempty"`
	Attempts      []RequestAttemptEvidence      `json:"attempts"`
}

// RequestObservationSnapshot is immutable request-level evidence captured
// before the usage task is detached from the Gin request.
type RequestObservationSnapshot struct {
	HandlerDurationMs       int
	FirstVisibleOutputMs    *int
	SemanticOutputSeen      bool
	TerminalKind            string
	AttemptCount            int
	AccountSwitchCount      int
	FailedAttemptDurationMs int
	RetryWaitMs             int
	AccountSwitchMs         int
	GatewayRequestID        string
	ClientRequestID         string
	AttemptLedger           *RequestAttemptLedger
}

type requestObservation struct {
	mu    sync.Mutex
	now   func() time.Time
	start time.Time

	gatewayRequestID string
	clientRequestID  string

	selectionStartedAt time.Time
	pendingSelectionMs int
	pendingSlotWaitMs  int
	switchStartedAt    time.Time

	attempts                []RequestAttemptEvidence
	attemptStartedAt        map[int]time.Time
	attemptSemanticAt       map[int]time.Time
	currentAttempt          int
	accountSwitchCount      int
	failedAttemptDurationMs int
	retryWaitMs             int
	accountSwitchMs         int
	firstVisibleAt          time.Time
	terminalKind            string
	protocolObserverSeen    bool
}

func BeginOpenAIRequestObservation(c *gin.Context, startedAt time.Time) {
	if c == nil {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	observer := &requestObservation{
		now:               time.Now,
		start:             startedAt,
		attemptStartedAt:  make(map[int]time.Time),
		attemptSemanticAt: make(map[int]time.Time),
	}
	if c.Request != nil {
		observer.gatewayRequestID = requestObservationContextString(c.Request.Context().Value(ctxkey.RequestID))
		observer.clientRequestID = requestObservationContextString(c.Request.Context().Value(ctxkey.ClientRequestID))
	}
	c.Set(requestObservationContextKey, observer)
}

func requestObservationContextString(value any) string {
	text, _ := value.(string)
	return boundedObservationString(text, 64)
}

func requestObservationFromGin(c *gin.Context) *requestObservation {
	if c == nil {
		return nil
	}
	value, ok := c.Get(requestObservationContextKey)
	if !ok {
		return nil
	}
	observer, _ := value.(*requestObservation)
	return observer
}

func BeginRequestObservationSelection(c *gin.Context) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.beginSelection()
	}
}

func EndRequestObservationSelection(c *gin.Context) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.endSelection()
	}
}

func RecordRequestObservationSlotWait(c *gin.Context, duration time.Duration) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.recordSlotWait(duration)
	}
}

func BeginRequestObservationAttempt(c *gin.Context, accountID int64, platform string) int {
	if observer := requestObservationFromGin(c); observer != nil {
		return observer.beginAttempt(accountID, platform)
	}
	return 0
}

func FinishRequestObservationAttempt(c *gin.Context, sequence int, result *OpenAIForwardResult, observedErr error, duration time.Duration) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.finishAttempt(sequence, result, observedErr, duration)
	}
}

func RecordRequestObservationRetryWait(c *gin.Context, duration time.Duration) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.recordRetryWait(duration)
	}
}

func RecordRequestObservationAccountSwitch(c *gin.Context) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.recordAccountSwitch()
	}
}

// RecordRequestObservationCyberSessionReceipt attaches only the opaque,
// content-free matcher receipt to the current Forward attempt.
func RecordRequestObservationCyberSessionReceipt(c *gin.Context, receipt CyberSessionBlockReceipt) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.recordCyberSessionReceipt(receipt)
	}
}

// ObserveOpenAIRequestEvent is the protocol-adapter seam for paths that do not
// use upstreamResponseModelObserver. It records classification only.
func ObserveOpenAIRequestEvent(c *gin.Context, payload []byte, eventType string) {
	if observer := requestObservationFromGin(c); observer != nil {
		observer.mu.Lock()
		sequence := observer.currentAttempt
		observer.mu.Unlock()
		observer.observeOpenAI(sequence, payload, eventType)
	}
}

func SnapshotRequestObservation(c *gin.Context) *RequestObservationSnapshot {
	if observer := requestObservationFromGin(c); observer != nil {
		return observer.snapshot()
	}
	return nil
}

func (o *requestObservation) beginSelection() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.selectionStartedAt = o.now()
	o.pendingSelectionMs = 0
	o.pendingSlotWaitMs = 0
}

func (o *requestObservation) endSelection() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.selectionStartedAt.IsZero() {
		o.pendingSelectionMs = durationMilliseconds(o.now().Sub(o.selectionStartedAt))
		o.selectionStartedAt = time.Time{}
	}
}

func (o *requestObservation) recordSlotWait(duration time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pendingSlotWaitMs = durationMilliseconds(duration)
}

func (o *requestObservation) beginAttempt(accountID int64, platform string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	sequence := len(o.attempts) + 1
	if !o.switchStartedAt.IsZero() {
		switchMs := durationMilliseconds(now.Sub(o.switchStartedAt))
		o.accountSwitchMs += switchMs
		o.switchStartedAt = time.Time{}
	}
	o.attempts = append(o.attempts, RequestAttemptEvidence{
		Sequence:        sequence,
		AccountID:       accountID,
		Platform:        normalizeObservationEnum(platform, 32),
		StartedOffsetMs: durationMilliseconds(now.Sub(o.start)),
		SelectionMs:     o.pendingSelectionMs,
		SlotWaitMs:      o.pendingSlotWaitMs,
		Outcome:         "in_progress",
	})
	o.pendingSelectionMs = 0
	o.pendingSlotWaitMs = 0
	o.attemptStartedAt[sequence] = now
	o.currentAttempt = sequence
	return sequence
}

func (o *requestObservation) finishAttempt(sequence int, result *OpenAIForwardResult, observedErr error, duration time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry := o.attempt(sequence)
	if entry == nil {
		return
	}
	entry.ForwardMs = durationMilliseconds(duration)
	if entry.ForwardMs == 0 {
		if started := o.attemptStartedAt[sequence]; !started.IsZero() {
			entry.ForwardMs = durationMilliseconds(o.now().Sub(started))
		}
	}
	entry.Outcome = "success"
	if result != nil {
		entry.UpstreamRequestID = boundedObservationString(result.RequestID, 128)
	}
	var failoverErr *UpstreamFailoverError
	switch {
	case errors.As(observedErr, &failoverErr) && failoverErr != nil:
		entry.Outcome = "failover"
		if failoverErr.StatusCode > 0 {
			status := failoverErr.StatusCode
			entry.StatusCode = &status
		}
		entry.Stage = normalizeObservationEnum(string(failoverErr.Stage), 32)
		entry.Scope = normalizeObservationEnum(string(failoverErr.Scope), 32)
		entry.Reason = normalizeObservationReason(string(failoverErr.Reason), failoverErr.StatusCode)
		if requestID := failoverErr.ResponseHeaders.Get("x-request-id"); requestID != "" {
			entry.UpstreamRequestID = boundedObservationString(requestID, 128)
		}
		o.failedAttemptDurationMs += entry.ForwardMs
	case observedErr != nil && result != nil && result.ClientDisconnect:
		entry.Outcome = "client_disconnect"
	case observedErr != nil:
		entry.Outcome = "stream_error"
	}
	if entry.TerminalKind == "" && result != nil {
		if !result.Stream {
			entry.TerminalKind = "json"
		} else if observedErr == nil {
			entry.TerminalKind = "[done]"
		}
	}
	if entry.TerminalKind == "" {
		switch entry.Outcome {
		case "stream_error":
			entry.TerminalKind = "stream_error"
		case "client_disconnect":
			entry.TerminalKind = "client_disconnect"
		}
	}
	if entry.TerminalKind != "" {
		o.terminalKind = entry.TerminalKind
	}
	if semanticAt := o.attemptSemanticAt[sequence]; !semanticAt.IsZero() && result != nil && !result.ClientDisconnect {
		entry.SemanticOutputSeen = true
		if o.firstVisibleAt.IsZero() {
			o.firstVisibleAt = semanticAt
		}
	}
	delete(o.attemptStartedAt, sequence)
}

func (o *requestObservation) recordRetryWait(duration time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	waitMs := durationMilliseconds(duration)
	o.retryWaitMs += waitMs
	if entry := o.lastAttempt(); entry != nil {
		entry.NextAction = "retry_same_account"
		entry.WaitAfterMs += waitMs
	}
}

func (o *requestObservation) recordAccountSwitch() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.accountSwitchCount++
	o.switchStartedAt = o.now()
	if entry := o.lastAttempt(); entry != nil {
		entry.NextAction = "switch_account"
	}
}

func (o *requestObservation) recordCyberSessionReceipt(receipt CyberSessionBlockReceipt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if entry := o.lastAttempt(); entry != nil {
		copy := receipt
		entry.CyberSession = &copy
	}
}

func (o *requestObservation) observeOpenAI(sequence int, payload []byte, eventType string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.protocolObserverSeen = true
	entry := o.attempt(sequence)
	if entry == nil {
		return
	}
	if terminal := normalizeObservationTerminal(eventType, payload, false); terminal != "" {
		entry.TerminalKind = terminal
		o.terminalKind = terminal
	}
	if !entry.SemanticOutputSeen && openAIObservationHasSemanticOutput(payload, eventType) {
		entry.SemanticOutputSeen = true
		o.attemptSemanticAt[sequence] = o.now()
	}
}

func (o *requestObservation) observeAnthropic(sequence int, payload []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.protocolObserverSeen = true
	entry := o.attempt(sequence)
	if entry == nil {
		return
	}
	if terminal := normalizeObservationTerminal("", payload, true); terminal != "" {
		entry.TerminalKind = terminal
		o.terminalKind = terminal
	}
	if !entry.SemanticOutputSeen && anthropicObservationHasSemanticOutput(payload) {
		entry.SemanticOutputSeen = true
		o.attemptSemanticAt[sequence] = o.now()
	}
}

func (o *requestObservation) snapshot() *RequestObservationSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.protocolObserverSeen {
		return nil
	}
	semanticSeen := false
	for i := range o.attempts {
		semanticSeen = semanticSeen || o.attempts[i].SemanticOutputSeen
	}
	var firstVisible *int
	if !o.firstVisibleAt.IsZero() {
		value := durationMilliseconds(o.firstVisibleAt.Sub(o.start))
		firstVisible = &value
	}
	snapshot := &RequestObservationSnapshot{
		FirstVisibleOutputMs:    firstVisible,
		SemanticOutputSeen:      semanticSeen,
		TerminalKind:            normalizeObservationTerminalKind(o.terminalKind),
		AttemptCount:            len(o.attempts),
		AccountSwitchCount:      o.accountSwitchCount,
		FailedAttemptDurationMs: o.failedAttemptDurationMs,
		RetryWaitMs:             o.retryWaitMs,
		AccountSwitchMs:         o.accountSwitchMs,
		GatewayRequestID:        o.gatewayRequestID,
		ClientRequestID:         o.clientRequestID,
	}
	if shouldPersistAttemptLedger(o.attempts, semanticSeen, snapshot.TerminalKind) {
		snapshot.AttemptLedger = buildRequestAttemptLedger(o.attempts)
	}
	snapshot.HandlerDurationMs = durationMilliseconds(o.now().Sub(o.start))
	return snapshot
}

func (o *requestObservation) attempt(sequence int) *RequestAttemptEvidence {
	if sequence <= 0 || sequence > len(o.attempts) {
		return nil
	}
	return &o.attempts[sequence-1]
}

func (o *requestObservation) lastAttempt() *RequestAttemptEvidence {
	if len(o.attempts) == 0 {
		return nil
	}
	return &o.attempts[len(o.attempts)-1]
}

func shouldPersistAttemptLedger(attempts []RequestAttemptEvidence, semanticSeen bool, terminalKind string) bool {
	if len(attempts) != 1 || !semanticSeen {
		return len(attempts) > 0
	}
	entry := attempts[0]
	return entry.Outcome != "success" || terminalKind == "stream_error" || terminalKind == "client_disconnect"
}

func buildRequestAttemptLedger(attempts []RequestAttemptEvidence) *RequestAttemptLedger {
	ledger := &RequestAttemptLedger{Version: 1, TotalAttempts: len(attempts)}
	if len(attempts) <= maxRequestAttemptLedgerItems {
		ledger.Attempts = append([]RequestAttemptEvidence(nil), attempts...)
		return ledger
	}
	ledger.Truncated = true
	ledger.Attempts = make([]RequestAttemptEvidence, 0, maxRequestAttemptLedgerItems)
	ledger.Attempts = append(ledger.Attempts, attempts[:requestAttemptLedgerEdgeSize]...)
	ledger.Attempts = append(ledger.Attempts, attempts[len(attempts)-requestAttemptLedgerEdgeSize:]...)
	folded := &RequestAttemptFoldedEvidence{ReasonCount: make(map[string]int)}
	for _, entry := range attempts[requestAttemptLedgerEdgeSize : len(attempts)-requestAttemptLedgerEdgeSize] {
		folded.Count++
		folded.ForwardMs += entry.ForwardMs
		if entry.Reason != "" {
			folded.ReasonCount[entry.Reason]++
		}
	}
	if len(folded.ReasonCount) == 0 {
		folded.ReasonCount = nil
	}
	ledger.Folded = folded
	return ledger
}

func ApplyRequestObservationSnapshot(log *UsageLog, snapshot *RequestObservationSnapshot) {
	if log == nil || snapshot == nil {
		return
	}
	log.HandlerDurationMs = intPtr(snapshot.HandlerDurationMs)
	log.FirstVisibleOutputMs = snapshot.FirstVisibleOutputMs
	log.SemanticOutputSeen = boolPtr(snapshot.SemanticOutputSeen)
	log.TerminalKind = optionalTrimmedStringPtr(snapshot.TerminalKind)
	log.AttemptCount = intPtr(snapshot.AttemptCount)
	log.AccountSwitchCount = intPtr(snapshot.AccountSwitchCount)
	log.FailedAttemptDurationMs = intPtr(snapshot.FailedAttemptDurationMs)
	log.RetryWaitMs = intPtr(snapshot.RetryWaitMs)
	log.AccountSwitchMs = intPtr(snapshot.AccountSwitchMs)
	log.GatewayRequestID = optionalTrimmedStringPtr(snapshot.GatewayRequestID)
	log.ClientRequestID = optionalTrimmedStringPtr(snapshot.ClientRequestID)
	log.AttemptLedger = snapshot.AttemptLedger
	log.AttemptLedgerAvailable = snapshot.AttemptLedger != nil
}

func RequestObservationAccessLogFields(c *gin.Context) *RequestObservationSnapshot {
	return SnapshotRequestObservation(c)
}

func durationMilliseconds(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	ms := duration.Milliseconds()
	maxInt := int64(^uint(0) >> 1)
	if ms > maxInt {
		return int(maxInt)
	}
	return int(ms)
}

func boundedObservationString(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func normalizeObservationEnum(value string, maxBytes int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return "other"
		}
	}
	return boundedObservationString(value, maxBytes)
}

func normalizeObservationReason(value string, statusCode int) string {
	if value = normalizeObservationEnum(value, 64); value != "" && value != "other" {
		return value
	}
	if statusCode >= 100 && statusCode <= 599 {
		return "http_" + strconv.Itoa(statusCode)
	}
	if value == "other" {
		return value
	}
	return "unknown"
}

func normalizeObservationTerminal(eventType string, payload []byte, anthropic bool) string {
	if anthropic {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	if strings.TrimSpace(eventType) == "" {
		eventType = gjson.GetBytes(payload, "type").String()
	}
	if strings.TrimSpace(eventType) == "" && len(payload) > 0 && gjson.ValidBytes(payload) {
		object := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "object").String()))
		if strings.HasSuffix(object, ".chunk") || gjson.GetBytes(payload, "choices.0.delta").Exists() {
			return ""
		}
		return "json"
	}
	return normalizeObservationTerminalKind(eventType)
}

func normalizeObservationTerminalKind(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "response.completed", "response.done", "response.failed", "response.incomplete",
		"response.cancelled", "response.canceled", "message_stop", "json", "stream_error",
		"transport_error", "client_disconnect", "error", "[done]":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func openAIObservationHasSemanticOutput(payload []byte, eventType string) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	if openAIStreamDataStartsVisibleOutput(string(payload), eventType) {
		return true
	}
	root := gjson.ParseBytes(payload)
	if strings.TrimSpace(eventType) == "response.output_item.added" || strings.TrimSpace(eventType) == "response.output_item.done" {
		switch root.Get("item.type").String() {
		case "function_call", "custom_tool_call", "computer_call", "image_generation_call":
			return true
		}
	}
	for _, path := range []string{
		"output_text", "text", "delta.content", "delta.reasoning", "delta.reasoning_content",
		"delta.refusal", "message.content", "message.reasoning", "message.reasoning_content",
		"response.output_text",
	} {
		if strings.TrimSpace(root.Get(path).String()) != "" {
			return true
		}
	}
	for _, choice := range root.Get("choices").Array() {
		for _, path := range []string{"delta.content", "delta.reasoning", "delta.reasoning_content", "delta.refusal", "message.content", "message.reasoning", "message.reasoning_content", "message.refusal"} {
			if strings.TrimSpace(choice.Get(path).String()) != "" {
				return true
			}
		}
		if len(choice.Get("delta.tool_calls").Array()) > 0 || len(choice.Get("message.tool_calls").Array()) > 0 ||
			choice.Get("delta.function_call").Exists() || choice.Get("message.function_call").Exists() {
			return true
		}
	}
	for _, path := range []string{"output", "response.output"} {
		for _, item := range root.Get(path).Array() {
			if openAIStreamItemHasVisibleOutput(item) {
				return true
			}
			switch item.Get("type").String() {
			case "function_call", "custom_tool_call", "computer_call", "image_generation_call":
				return true
			}
		}
	}
	return false
}

func anthropicObservationHasSemanticOutput(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"delta.text", "delta.partial_json", "delta.thinking", "content_block.text", "content_block.input", "content_block.thinking"} {
		value := root.Get(path)
		if value.Exists() && strings.TrimSpace(value.String()) != "" && value.String() != "{}" {
			return true
		}
	}
	blockType := strings.TrimSpace(root.Get("content_block.type").String())
	if blockType == "tool_use" && strings.TrimSpace(root.Get("content_block.name").String()) != "" {
		return true
	}
	for _, item := range root.Get("content").Array() {
		switch item.Get("type").String() {
		case "text":
			if strings.TrimSpace(item.Get("text").String()) != "" {
				return true
			}
		case "thinking":
			if strings.TrimSpace(item.Get("thinking").String()) != "" {
				return true
			}
		case "tool_use":
			return true
		}
	}
	for _, item := range root.Get("message.content").Array() {
		if anthropicObservationHasSemanticOutput([]byte(item.Raw)) {
			return true
		}
	}
	return false
}
