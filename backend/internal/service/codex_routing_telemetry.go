package service

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	codexTelemetryPolicyKey  = "codex_telemetry_policy"
	codexTelemetryAttemptKey = "codex_telemetry_attempt"
)

// SetCodexTelemetryCapturePolicy installs a memory-only policy reader. Each new
// forwarding attempt/turn takes its own value, including on long-lived sockets.
func SetCodexTelemetryCapturePolicy(c *gin.Context, policy func() bool) {
	if c != nil {
		c.Set(codexTelemetryPolicyKey, policy)
	}
}

func codexTelemetryCaptureEnabled(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, _ := c.Get(codexTelemetryPolicyKey)
	policy, _ := value.(func() bool)
	return policy != nil && policy()
}

type codexTelemetryAttempt struct {
	mu                sync.Mutex
	collector         *codextelemetry.Collector
	accountID         int64
	platform          string
	upstreamRequestID string
	failed            bool
	consumed          bool
	startedAtUnixMs   int64
}

func beginCodexTelemetryAttempt(c *gin.Context, account *Account, transport string, reused bool) *codexTelemetryAttempt {
	if c == nil {
		return nil
	}
	c.Set(codexTelemetryAttemptKey, (*codexTelemetryAttempt)(nil))
	if account == nil || account.Platform != PlatformOpenAI || !codexTelemetryCaptureEnabled(c) {
		return nil
	}
	attempt := &codexTelemetryAttempt{
		collector:       codextelemetry.New(transport, reused),
		accountID:       account.ID,
		platform:        account.Platform,
		startedAtUnixMs: time.Now().UnixMilli(),
	}
	c.Set(codexTelemetryAttemptKey, attempt)
	return attempt
}

func codexTelemetryAttemptFromContext(c *gin.Context) *codexTelemetryAttempt {
	if c == nil {
		return nil
	}
	value, _ := c.Get(codexTelemetryAttemptKey)
	attempt, _ := value.(*codexTelemetryAttempt)
	return attempt
}

func beginCodexHTTPAttempt(c *gin.Context, account *Account) *codexTelemetryAttempt {
	attempt := beginCodexTelemetryAttempt(c, account, codextelemetry.HTTP, false)
	if observer := upstreamResponseModelObserverFromContext(c); observer != nil {
		observer.codexTelemetry = attempt
	}
	return attempt
}

func (a *codexTelemetryAttempt) observe(payload []byte, eventType string, completedResponseIDs ...string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.collector.Closed() {
		return
	}
	if a.collector.Observe(payload, eventType, completedResponseIDs...) {
		return
	}
	if eventType != "error" && !isUpstreamResponseModelTerminalEvent(eventType) {
		return
	}
	_, eventID, _ := parseOpenAIWSEventEnvelope(payload)
	if eventID != "" && a.collector.ResponseID() != "" && eventID != a.collector.ResponseID() {
		return
	}
	switch eventType {
	case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		a.failed = true
	case "response.completed", "response.done":
		a.failed = false
	}
}

func (a *codexTelemetryAttempt) headers(headers http.Header, source string, status int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.collector.ObserveHeaders(headers, source)
	a.upstreamRequestID = normalizeObservedUpstreamResponseModel(headers.Get("x-request-id"))
	if status >= 400 {
		a.failed = true
	}
}

func (a *codexTelemetryAttempt) snapshot() *codextelemetry.Snapshot {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.collector.Snapshot()
}

func (a *codexTelemetryAttempt) finish(result *OpenAIForwardResult, err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil && !a.collector.Closed() {
		a.failed = true
	}
	a.collector.Close()
	if result != nil {
		if result.CodexTelemetry == nil {
			result.CodexTelemetry = a.collector.Snapshot()
		}
		expectedID := strings.TrimSpace(result.ResponseID)
		if expectedID == "" && result.OpenAIWSMode {
			expectedID = strings.TrimSpace(result.RequestID)
		}
		if snapshot := result.CodexTelemetry; snapshot != nil && expectedID != "" && snapshot.ResponseID != "" && snapshot.ResponseID != expectedID {
			result.CodexTelemetry = nil
		}
	}
}

func observedCodexTelemetry(c *gin.Context) *codextelemetry.Snapshot {
	return codexTelemetryAttemptFromContext(c).snapshot()
}

func cloneCodexTelemetry(snapshot *codextelemetry.Snapshot) *codextelemetry.Snapshot {
	return codextelemetry.Clone(snapshot)
}

// AttachCodexTelemetryToOpsEntry only enriches an already-admitted Ops row.
// Calling it after the logger's eligibility/classification checks preserves row
// counts, skip-monitoring behavior and request-error classification.
func AttachCodexTelemetryToOpsEntry(c *gin.Context, entry *OpsInsertErrorLogInput) {
	a := codexTelemetryAttemptFromContext(c)
	if a == nil || entry == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.failed || (entry.AccountID != nil && *entry.AccountID > 0 && *entry.AccountID != a.accountID) {
		return
	}
	snapshot := a.collector.Snapshot()
	if snapshot == nil {
		return
	}
	for i := len(entry.UpstreamErrors) - 1; i >= 0; i-- {
		event := entry.UpstreamErrors[i]
		if event == nil || event.AccountID != a.accountID || event.codexTelemetryOwner != a {
			continue
		}
		if event.CodexTelemetry != nil {
			return
		}
		if event.UpstreamRequestID != "" && a.upstreamRequestID != "" && event.UpstreamRequestID != a.upstreamRequestID {
			continue
		}
		copy := *event
		copy.CodexTelemetry = snapshot
		entry.UpstreamErrors = append([]*OpsUpstreamErrorEvent(nil), entry.UpstreamErrors...)
		entry.UpstreamErrors[i] = &copy
		return
	}
	status := 0
	if entry.UpstreamStatusCode != nil {
		status = *entry.UpstreamStatusCode
	}
	entry.UpstreamErrors = append(append([]*OpsUpstreamErrorEvent(nil), entry.UpstreamErrors...), &OpsUpstreamErrorEvent{
		AtUnixMs: a.startedAtUnixMs,
		Platform: a.platform, AccountID: a.accountID, UpstreamRequestID: a.upstreamRequestID,
		UpstreamStatusCode: status, Kind: "request_error", CodexTelemetry: snapshot, codexTelemetryOwner: a,
	})
}

// ApplyOpsStreamTelemetrySnapshot runs after stream-error classification. The
// frozen telemetry must not turn a local/client error into a provider error.
func ApplyOpsStreamTelemetrySnapshot(entry *OpsInsertErrorLogInput, streamErr OpsStreamError) {
	if entry != nil && len(streamErr.codexTelemetryEvents) > 0 {
		entry.UpstreamErrors = streamErr.codexTelemetryEvents
	}
}

// codexTelemetryFrameConn observes the native passthrough upstream boundary.
// Reads/writes and their errors are returned unchanged; no relay bookkeeping is
// driven by this collector. In particular, stray metadata cannot open a turn.
type codexTelemetryFrameConn struct {
	inner     openaiwsv2.FrameConn
	c         *gin.Context
	account   *Account
	headers   http.Header
	mu        sync.Mutex
	attempt   *codexTelemetryAttempt
	turns     int
	completed []string
}

func (conn *codexTelemetryFrameConn) WriteFrame(ctx context.Context, messageType coderws.MessageType, payload []byte) error {
	isCreate := (messageType == coderws.MessageText || messageType == coderws.MessageBinary) && gjson.GetBytes(payload, "type").String() == "response.create"
	if isCreate {
		conn.mu.Lock()
		conn.turns++
		conn.attempt = beginCodexTelemetryAttempt(conn.c, conn.account, codextelemetry.WebSocket, conn.turns > 1)
		if gjson.GetBytes(payload, "generate").Type == gjson.False {
			conn.attempt = nil
			conn.c.Set(codexTelemetryAttemptKey, (*codexTelemetryAttempt)(nil))
		}
		if conn.turns == 1 {
			conn.attempt.headers(conn.headers, codextelemetry.WSUpgradeHeaders, http.StatusSwitchingProtocols)
		}
		conn.mu.Unlock()
	}
	err := conn.inner.WriteFrame(ctx, messageType, payload)
	if err != nil && isCreate {
		conn.mu.Lock()
		conn.attempt.finish(nil, err)
		conn.mu.Unlock()
	}
	return err
}

func (conn *codexTelemetryFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	messageType, payload, err := conn.inner.ReadFrame(ctx)
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if err != nil {
		conn.attempt.finish(nil, err)
	} else if messageType == coderws.MessageText && conn.attempt != nil {
		eventType, _, _ := parseOpenAIWSEventEnvelope(payload)
		conn.attempt.observe(payload, eventType, conn.completed...)
	}
	return messageType, payload, err
}

func (conn *codexTelemetryFrameConn) take(responseID string) *codextelemetry.Snapshot {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if responseID != "" && len(responseID) <= codextelemetry.MaxIDBytes {
		conn.completed = append(conn.completed, strings.Clone(responseID))
		if len(conn.completed) > 32 {
			conn.completed = conn.completed[len(conn.completed)-32:]
		}
	}
	a := conn.attempt
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.consumed {
		return nil
	}
	snapshot := a.collector.Snapshot()
	if snapshot == nil {
		return nil
	}
	if snapshot.ResponseID != strings.TrimSpace(responseID) {
		if snapshot.ResponseID != "" || snapshot.StreamID != "" {
			return nil
		}
		// An ambiguous response may still retain the real, connection-scoped
		// handshake. Event-derived evidence continues to require an ID match.
		for _, observation := range snapshot.Observations {
			if observation.Source != codextelemetry.WSUpgradeHeaders || observation.Association != codextelemetry.Connection {
				return nil
			}
		}
	}
	a.consumed = true
	return snapshot
}

func (conn *codexTelemetryFrameConn) Close() error { return conn.inner.Close() }

func (l *openAIWSConnLease) codexTelemetryConnectionReused() bool {
	return l != nil && (l.Reused() || l.IsPrewarmed() || (l.conn != nil && l.conn.codexTelemetryHandshakeTaken.Load()))
}

func (l *openAIWSConnLease) takeCodexTelemetryHandshake(capture bool) http.Header {
	if l == nil || l.conn == nil || l.Reused() || l.IsPrewarmed() || !l.conn.codexTelemetryHandshakeTaken.CompareAndSwap(false, true) {
		return nil
	}
	if !capture {
		return nil
	}
	return l.HandshakeHeaders()
}
