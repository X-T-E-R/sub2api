package service

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	defaultOpenAIProxyStreamFailureThreshold  = 2
	defaultOpenAIProxyStreamFailureWindow     = time.Minute
	defaultOpenAIProxyStreamQuarantineTTL     = 10 * time.Minute
	defaultOpenAIProxyStreamCircuitMaxEntries = 4096
	// defaultOpenAIProxyStreamFailureCollapse merges disconnects that land
	// within this interval into a single failure event. One proxy or HTTP/2
	// connection loss kills every stream multiplexed on it at the same
	// moment; counting each stream separately would trip the breaker from a
	// single upstream event (#5056).
	defaultOpenAIProxyStreamFailureCollapse = 3 * time.Second
	// openAIProxyStreamFailOpenLogInterval rate-limits the fail-open warning
	// so an outage-mode burst does not flood the log.
	openAIProxyStreamFailOpenLogInterval = 5 * time.Second
)

type openAIProxyStreamCircuitSettings struct {
	disabled         bool
	failureThreshold int
	failureWindow    time.Duration
	quarantineTTL    time.Duration
	collapseInterval time.Duration
	maxEntries       int
}

type openAIProxyStreamCircuitEntry struct {
	failureCount  int
	windowStart   time.Time
	lastFailureAt time.Time
	blockedUntil  time.Time
	lastTouched   time.Time
}

// openAIProxyStreamCircuit is shared by all platforms and keyed only by proxy ID. It is
// intentionally bounded and ephemeral: a restart clears observations, while a
// tripped entry expires automatically after its TTL.
type openAIProxyStreamCircuit struct {
	mu       sync.Mutex
	settings openAIProxyStreamCircuitSettings
	entries  map[int64]openAIProxyStreamCircuitEntry
}

func resolveProxyStreamCircuitSettings(cfg *config.Config) openAIProxyStreamCircuitSettings {
	settings := openAIProxyStreamCircuitSettings{
		failureThreshold: defaultOpenAIProxyStreamFailureThreshold,
		failureWindow:    defaultOpenAIProxyStreamFailureWindow,
		quarantineTTL:    defaultOpenAIProxyStreamQuarantineTTL,
		collapseInterval: defaultOpenAIProxyStreamFailureCollapse,
		maxEntries:       defaultOpenAIProxyStreamCircuitMaxEntries,
	}
	if cfg == nil {
		return settings
	}
	c := cfg.Gateway.OpenAIProxyStreamCircuit
	settings.disabled = c.Disabled
	if c.FailureThreshold > 0 {
		settings.failureThreshold = c.FailureThreshold
	}
	if c.WindowSeconds > 0 {
		settings.failureWindow = time.Duration(c.WindowSeconds) * time.Second
	}
	if c.TTLSeconds > 0 {
		settings.quarantineTTL = time.Duration(c.TTLSeconds) * time.Second
	}
	return settings
}

func newOpenAIProxyStreamCircuit(settings openAIProxyStreamCircuitSettings) *openAIProxyStreamCircuit {
	if settings.failureThreshold <= 0 {
		settings.failureThreshold = defaultOpenAIProxyStreamFailureThreshold
	}
	if settings.failureWindow <= 0 {
		settings.failureWindow = defaultOpenAIProxyStreamFailureWindow
	}
	if settings.quarantineTTL <= 0 {
		settings.quarantineTTL = defaultOpenAIProxyStreamQuarantineTTL
	}
	if settings.maxEntries <= 0 {
		settings.maxEntries = defaultOpenAIProxyStreamCircuitMaxEntries
	}
	if settings.collapseInterval < 0 {
		settings.collapseInterval = 0
	}
	return &openAIProxyStreamCircuit{
		settings: settings,
		entries:  make(map[int64]openAIProxyStreamCircuitEntry),
	}
}

func (s *OpenAIGatewayService) getOpenAIProxyStreamCircuit() *openAIProxyStreamCircuit {
	if s == nil {
		return nil
	}
	s.openaiProxyStreamCircuitOnce.Do(func() {
		if s.openaiProxyStreamCircuit == nil {
			s.openaiProxyStreamCircuit = newOpenAIProxyStreamCircuit(resolveProxyStreamCircuitSettings(s.cfg))
		}
	})
	return s.openaiProxyStreamCircuit
}

func (c *openAIProxyStreamCircuit) recordFailure(proxyID int64, now time.Time) (bool, time.Time) {
	if c == nil || c.settings.disabled || proxyID <= 0 {
		return false, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[proxyID]
	if exists && now.Before(entry.blockedUntil) {
		entry.lastTouched = now
		c.entries[proxyID] = entry
		return false, entry.blockedUntil
	}
	if !exists {
		c.ensureCapacityLocked(now)
	}
	if entry.windowStart.IsZero() || now.Before(entry.windowStart) || now.Sub(entry.windowStart) > c.settings.failureWindow {
		entry.failureCount = 0
		entry.windowStart = now
		entry.blockedUntil = time.Time{}
	}
	// Collapse a burst of disconnects into one failure event: when a proxy or
	// a multiplexed HTTP/2 connection dies, every in-flight stream reports the
	// same underlying incident within moments of each other.
	if c.settings.collapseInterval > 0 && !entry.lastFailureAt.IsZero() &&
		now.Sub(entry.lastFailureAt) >= 0 && now.Sub(entry.lastFailureAt) < c.settings.collapseInterval {
		entry.lastTouched = now
		c.entries[proxyID] = entry
		return false, time.Time{}
	}
	entry.failureCount++
	entry.lastFailureAt = now
	entry.lastTouched = now
	tripped := entry.failureCount >= c.settings.failureThreshold
	if tripped {
		entry.blockedUntil = now.Add(c.settings.quarantineTTL)
	}
	c.entries[proxyID] = entry
	return tripped, entry.blockedUntil
}

func (c *openAIProxyStreamCircuit) recordSuccess(proxyID int64) bool {
	if c == nil || proxyID <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[proxyID]; !ok {
		return false
	}
	delete(c.entries, proxyID)
	return true
}

func (c *openAIProxyStreamCircuit) isBlocked(proxyID int64, now time.Time) bool {
	if c == nil || c.settings.disabled || proxyID <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[proxyID]
	if !ok || entry.blockedUntil.IsZero() {
		return false
	}
	if !now.Before(entry.blockedUntil) {
		delete(c.entries, proxyID)
		return false
	}
	return true
}

// activeBlockCount reports how many proxies are currently quarantined. It
// gates the fail-open retry: a "no available accounts" selection result only
// warrants a second, quarantine-blind pass when the circuit is actually
// withholding capacity.
func (c *openAIProxyStreamCircuit) activeBlockCount(now time.Time) int {
	if c == nil || c.settings.disabled {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, entry := range c.entries {
		if !entry.blockedUntil.IsZero() && now.Before(entry.blockedUntil) {
			count++
		}
	}
	return count
}

func (c *openAIProxyStreamCircuit) ensureCapacityLocked(now time.Time) {
	if len(c.entries) < c.settings.maxEntries {
		return
	}
	for proxyID, entry := range c.entries {
		staleObservation := entry.blockedUntil.IsZero() && now.Sub(entry.lastTouched) > c.settings.failureWindow
		expiredQuarantine := !entry.blockedUntil.IsZero() && !now.Before(entry.blockedUntil)
		if staleObservation || expiredQuarantine {
			delete(c.entries, proxyID)
		}
	}
	if len(c.entries) < c.settings.maxEntries {
		return
	}
	var oldestProxyID int64
	var oldest time.Time
	for proxyID, entry := range c.entries {
		if oldestProxyID == 0 || entry.lastTouched.Before(oldest) {
			oldestProxyID = proxyID
			oldest = entry.lastTouched
		}
	}
	if oldestProxyID > 0 {
		delete(c.entries, oldestProxyID)
	}
}

func openAIProxyStreamCircuitProxyID(account *Account) (int64, bool) {
	if account == nil || account.ProxyID == nil || *account.ProxyID <= 0 {
		return 0, false
	}
	return *account.ProxyID, true
}

func (s *OpenAIGatewayService) recordOpenAIProxyStreamDisconnect(account *Account, streamErr error, upstreamRequestID string) {
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok || streamErr == nil || errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) || errors.Is(streamErr, bufio.ErrTooLong) {
		return
	}
	circuit := s.getOpenAIProxyStreamCircuit()
	tripped, until := circuit.recordFailure(proxyID, time.Now())
	if !tripped {
		return
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.proxy_quarantined_stream_disconnect",
		zap.Int64("proxy_id", proxyID),
		zap.Int64("account_id", account.ID),
		zap.Time("until", until),
		zap.String("upstream_request_id", upstreamRequestID),
		zap.String("error", sanitizeUpstreamErrorMessage(streamErr.Error())),
	)
}

func (s *OpenAIGatewayService) clearOpenAIProxyStreamDisconnect(account *Account) {
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok {
		return
	}
	if circuit := s.getOpenAIProxyStreamCircuit(); circuit != nil {
		circuit.recordSuccess(proxyID)
	}
}

// openAIProxyStreamQuarantineBypassKey marks a selection pass that must ignore
// proxy quarantine. It is set for the second, fail-open selection attempt when
// the first pass found no available account while the circuit was withholding
// proxies: a degraded proxy is strictly better than answering 502 (#5056).
type openAIProxyStreamQuarantineBypassKey struct{}

func withOpenAIProxyStreamQuarantineBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIProxyStreamQuarantineBypassKey{}, true)
}

func openAIProxyStreamQuarantineBypassed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	bypassed, _ := ctx.Value(openAIProxyStreamQuarantineBypassKey{}).(bool)
	return bypassed
}

func (s *OpenAIGatewayService) isOpenAIProxyStreamQuarantined(ctx context.Context, account *Account) bool {
	proxyID, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok {
		return false
	}
	if openAIProxyStreamQuarantineBypassed(ctx) {
		return false
	}
	circuit := s.getOpenAIProxyStreamCircuit()
	return circuit != nil && circuit.isBlocked(proxyID, time.Now())
}

// logOpenAIProxyStreamQuarantineFailOpen emits a rate-limited warning when a
// selection pass had to re-admit quarantined proxies to serve at all.
func (s *OpenAIGatewayService) logOpenAIProxyStreamQuarantineFailOpen(requestedModel string, blockedProxies int) {
	now := time.Now().UnixNano()
	last := s.openaiProxyStreamFailOpenLogAt.Load()
	if now-last < int64(openAIProxyStreamFailOpenLogInterval) ||
		!s.openaiProxyStreamFailOpenLogAt.CompareAndSwap(last, now) {
		return
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.proxy_stream_quarantine_fail_open",
		zap.Int("blocked_proxies", blockedProxies),
		zap.String("model", requestedModel),
	)
}

// NewProxyStreamCircuit owns one runtime circuit for all gateway instances.
// The existing gateway.openai_proxy_stream_circuit settings apply to all platforms.
func NewProxyStreamCircuit(cfg *config.Config) *openAIProxyStreamCircuit {
	return newOpenAIProxyStreamCircuit(resolveProxyStreamCircuitSettings(cfg))
}

func (c *openAIProxyStreamCircuit) recordDisconnect(ctx context.Context, account *Account, err error) {
	id, ok := openAIProxyStreamCircuitProxyID(account)
	if !ok || err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	tripped, until := c.recordFailure(id, time.Now())
	if tripped {
		logger.L().Warn("gateway.proxy_quarantined_transport_disconnect", zap.Int64("proxy_id", id), zap.Int64("account_id", account.ID), zap.String("platform", account.Platform), zap.Time("until", until))
	}
}

func (c *openAIProxyStreamCircuit) recordCompleted(ctx context.Context, account *Account) {
	if ctx.Err() != nil {
		return
	}
	if id, ok := openAIProxyStreamCircuitProxyID(account); ok {
		c.recordSuccess(id)
	}
}

func (c *openAIProxyStreamCircuit) quarantined(ctx context.Context, account *Account) bool {
	id, ok := openAIProxyStreamCircuitProxyID(account)
	return ok && !openAIProxyStreamQuarantineBypassed(ctx) && c.isBlocked(id, time.Now())
}

// observeTransport is called at the actual Do boundary, never for HTTP errors
// or caller cancellation. Body failures retain their original error identity.
func (c *openAIProxyStreamCircuit) observeTransport(ctx context.Context, account *Account, resp *http.Response, err error) {
	if err != nil {
		if proxyTransportFailure(err) {
			c.recordDisconnect(ctx, account, err)
		}
		return
	}
	if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Body != nil {
		observation := &proxyStreamObservation{circuit: c, ctx: ctx, account: account}
		if resp.Request == nil {
			resp.Request = &http.Request{}
		}
		resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), proxyStreamObservationKey{}, observation))
		resp.Body = &proxyObservedBody{ReadCloser: resp.Body, observation: observation, stream: strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")}
	}
}

type proxyStreamObservationKey struct{}
type proxyStreamObservation struct {
	circuit   *openAIProxyStreamCircuit
	ctx       context.Context
	account   *Account
	failed    atomic.Bool
	closed    atomic.Bool
	completed atomic.Bool
}

type proxyObservedBody struct {
	io.ReadCloser
	observation *proxyStreamObservation
	stream      bool
}

func (b *proxyObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && !b.observation.closed.Load() && !b.observation.completed.Load() && !b.observation.failed.Swap(true) {
		b.observation.circuit.recordDisconnect(b.observation.ctx, b.observation.account, err)
	}
	if err == io.EOF && !b.stream && !b.observation.failed.Load() {
		b.observation.circuit.recordCompleted(b.observation.ctx, b.observation.account)
	}
	return n, err
}

func (b *proxyObservedBody) Close() error {
	b.observation.closed.Store(true)
	return b.ReadCloser.Close()
}

// Call only at protocol EOF: a semantic provider error is not a disconnect.
func observeProxyStreamCompletion(resp *http.Response, terminal bool) {
	if resp == nil || resp.Request == nil {
		return
	}
	observation, _ := resp.Request.Context().Value(proxyStreamObservationKey{}).(*proxyStreamObservation)
	if observation == nil {
		return
	}
	if terminal {
		observation.completed.Store(true)
		observation.circuit.recordCompleted(observation.ctx, observation.account)
	} else if !observation.completed.Load() && !observation.failed.Swap(true) {
		observation.circuit.recordDisconnect(observation.ctx, observation.account, io.ErrUnexpectedEOF)
	}
}

// proxyGeminiGeneration observes only wire termination, including v1internal
// envelopes. A provider error frame is a protocol outcome, not a disconnect.
type proxyGeminiGeneration struct {
	terminal, providerError bool
}

func (g *proxyGeminiGeneration) observeLine(line string) {
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
	if !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "[DONE]" {
		g.terminal = true
		return
	}
	frame := gjson.Parse(payload)
	g.providerError = g.providerError || frame.Get("error").Exists()
	if inner := frame.Get("response"); inner.IsObject() {
		frame = inner
	}
	g.providerError = g.providerError || frame.Get("error").Exists()
	switch reason := frame.Get("candidates.0.finishReason").String(); reason {
	case "", "FINISH_REASON_UNSPECIFIED":
	default:
		g.terminal = true
	}
	if reason := frame.Get("promptFeedback.blockReason").String(); reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
		g.terminal = true
	}
	for _, rating := range frame.Get("candidates.0.safetyRatings").Array() {
		g.terminal = g.terminal || rating.Get("blocked").Bool()
	}
}

func (g *proxyGeminiGeneration) complete(resp *http.Response) {
	if !g.providerError {
		observeProxyStreamCompletion(resp, g.terminal)
	}
}

func proxyTransportFailure(err error) bool {
	if wrapped, ok := err.(*url.Error); ok {
		return proxyTransportFailure(wrapped.Err)
	}
	var network net.Error
	var cert *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var record tls.RecordHeaderError
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.As(err, &network) || errors.As(err, &cert) || errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) || errors.As(err, &record)
}
