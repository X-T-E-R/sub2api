package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This adapter translates the real Forward stream to admin events. It does not
// buffer the generation or replace forwarding's retry/signature/usage handling.
// The raw transport tap verifies the upstream terminal instead of trusting a
// protocol transformer's synthesized message_stop after EOF.
type accountTestForwardWriter struct {
	gin.ResponseWriter
	service     *AccountTestService
	admin       *gin.Context
	pending     string
	usable      bool
	failure     string
	failureInfo accountTestErrorDetail
	statusCode  int
	errorBody   bytes.Buffer
	headers     http.Header
}

func (w *accountTestForwardWriter) Header() http.Header { return w.headers }
func (w *accountTestForwardWriter) WriteHeader(code int) {
	w.statusCode = code
	if code >= 400 {
		w.failure = fmt.Sprintf("Forward returned HTTP %d", code)
	}
}
func (w *accountTestForwardWriter) WriteHeaderNow() {}

// Admin event writes already flush atomically; Forward's keepalive flush must
// not race the raw-reader metadata writer on Gin's response state.
func (w *accountTestForwardWriter) Flush() {}
func (w *accountTestForwardWriter) Status() int {
	if w.statusCode != 0 {
		return w.statusCode
	}
	return http.StatusOK
}
func (w *accountTestForwardWriter) Size() int                         { return 0 }
func (w *accountTestForwardWriter) Written() bool                     { return true }
func (w *accountTestForwardWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *accountTestForwardWriter) errorDetail() accountTestErrorDetail {
	if w.statusCode >= 400 && w.errorBody.Len() > 0 {
		if detail := buildAccountTestUpstreamError("Forward", w.statusCode, w.errorBody.Bytes()); detail.message != "" {
			return detail
		}
	}
	if w.failureInfo.message != "" {
		return w.failureInfo
	}
	return accountTestErrorDetail{message: w.failure}
}
func (w *accountTestForwardWriter) Write(p []byte) (int, error) {
	if err := w.admin.Request.Context().Err(); err != nil {
		return 0, err
	}
	if w.statusCode >= 400 {
		if remaining := int(gatewayUpstreamErrorBodyReadLimit) - w.errorBody.Len(); remaining > 0 {
			if len(p) > remaining {
				_, _ = w.errorBody.Write(p[:remaining])
			} else {
				_, _ = w.errorBody.Write(p)
			}
		}
		return len(p), nil
	}
	w.pending += strings.ReplaceAll(string(p), "\r\n", "\n")
	for {
		n := strings.Index(w.pending, "\n\n")
		if n < 0 {
			break
		}
		frame := w.pending[:n]
		w.pending = w.pending[n+2:]
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			payload := gjson.Parse(data)
			switch payload.Get("type").String() {
			case "content_block_delta":
				if payload.Get("delta.type").String() == "text_delta" {
					text := payload.Get("delta.text").String()
					if strings.TrimSpace(text) != "" {
						w.usable = true
					}
					if text != "" {
						w.service.sendEvent(w.admin, TestEvent{Type: "content", Text: text})
					}
				}
			case "content_block_start":
				if payload.Get("content_block.type").String() == "tool_use" {
					w.usable = true
					w.service.sendEvent(w.admin, TestEvent{Type: "content", Text: payload.Get("content_block").Raw})
				}
			case "error":
				detail := buildAccountTestUpstreamError("", 0, []byte(data))
				if detail.message == "" {
					detail.message = "Upstream stream error"
				}
				w.failure = detail.message
				w.failureInfo = detail
			}
		}
	}
	return len(p), nil
}

type accountTestRawStreamState struct {
	mu                   sync.Mutex
	service              *AccountTestService
	admin                *gin.Context
	observer             *upstreamResponseModelObserver
	terminal             bool
	failure              error
	failureDetail        accountTestErrorDetail
	pending              string
	upstreamErrorStatus  int
	upstreamErrorBody    []byte
	upstreamTransportErr error
}

func (s *accountTestRawStreamState) lastUpstreamError() (int, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upstreamErrorStatus == 0 || len(s.upstreamErrorBody) == 0 {
		return s.upstreamErrorStatus, nil, s.upstreamTransportErr
	}
	body := make([]byte, len(s.upstreamErrorBody))
	copy(body, s.upstreamErrorBody)
	return s.upstreamErrorStatus, body, s.upstreamTransportErr
}

func (s *accountTestRawStreamState) observeLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	if !gjson.ValidBytes(data) {
		return
	}
	s.observer.ObserveGemini(data)
	s.service.sendEvent(s.admin, TestEvent{Type: "test_metadata", UpstreamResponseModel: s.observer.Model(), UpstreamResponseModelConflict: s.observer.Conflict()})
	if e := gjson.GetBytes(data, "error"); e.Exists() {
		detail := buildAccountTestUpstreamError("upstream stream error", 0, data)
		if detail.message == "" {
			detail.message = "upstream stream error"
		}
		s.failure = errors.New(detail.message)
		s.failureDetail = detail
	}
	candidates := gjson.GetBytes(data, "response.candidates")
	if !candidates.Exists() {
		candidates = gjson.GetBytes(data, "candidates")
	}
	for _, candidate := range candidates.Array() {
		reason := candidate.Get("finishReason").String()
		if reason == "STOP" || reason == "MAX_TOKENS" {
			s.terminal = true
		} else if reason != "" {
			s.failure = fmt.Errorf("upstream generation ended with %s", reason)
			s.failureDetail = accountTestErrorDetail{}
		}
	}
}
func (s *accountTestRawStreamState) observe(p []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending += string(p)
	for {
		n := strings.IndexByte(s.pending, '\n')
		if n < 0 {
			break
		}
		s.observeLine(strings.TrimSuffix(s.pending[:n], "\r"))
		s.pending = s.pending[n+1:]
	}
	if len(s.pending) > 8<<20 {
		s.failure = fmt.Errorf("upstream stream frame exceeds 8 MiB")
		s.failureDetail = accountTestErrorDetail{}
		s.pending = ""
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if s.pending != "" {
				s.observeLine(s.pending)
				s.pending = ""
			}
		} else {
			s.failure = err
			s.failureDetail = accountTestErrorDetail{}
		}
	}
}

type accountTestObservedBody struct {
	io.ReadCloser
	state *accountTestRawStreamState
}

func (b *accountTestObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.state.observe(p[:n], err)
	return n, err
}

type accountTestErrorBodyTap struct {
	io.ReadCloser
	state *accountTestRawStreamState
}

func (b *accountTestErrorBodyTap) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.state.mu.Lock()
		if remaining := int(gatewayUpstreamErrorBodyReadLimit) - len(b.state.upstreamErrorBody); remaining > 0 {
			chunk := p[:n]
			if len(chunk) > remaining {
				chunk = chunk[:remaining]
			}
			b.state.upstreamErrorBody = append(b.state.upstreamErrorBody, chunk...)
		}
		b.state.mu.Unlock()
	}
	return n, err
}

type accountTestObservedUpstream struct {
	HTTPUpstream
	state *accountTestRawStreamState
}

func (u *accountTestObservedUpstream) wrap(resp *http.Response, err error) (*http.Response, error) {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	// Every upstream attempt owns its own terminal outcome so a retry's transport
	// error never inherits a stale HTTP error body from an earlier attempt.
	u.state.upstreamErrorStatus = 0
	u.state.upstreamErrorBody = nil
	u.state.upstreamTransportErr = nil
	if err != nil {
		u.state.upstreamTransportErr = err
		return resp, err
	}
	if resp != nil {
		if resp.StatusCode == http.StatusOK {
			u.state.terminal = false
			u.state.failure = nil
			u.state.failureDetail = accountTestErrorDetail{}
			u.state.pending = ""
			u.state.observer = &upstreamResponseModelObserver{}
			resp.Body = &accountTestObservedBody{ReadCloser: resp.Body, state: u.state}
		} else if resp.StatusCode >= 400 && resp.Body != nil {
			u.state.upstreamErrorStatus = resp.StatusCode
			resp.Body = &accountTestErrorBodyTap{ReadCloser: resp.Body, state: u.state}
		}
	}
	return resp, err
}
func (u *accountTestObservedUpstream) observeRequest(req *http.Request) {
	if req.GetBody == nil {
		return
	}
	body, err := req.GetBody()
	if err != nil {
		return
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		return
	}
	if model := firstValidTrimmedGJSONString(data, "model"); model != "" {
		u.state.service.sendEvent(u.state.admin, TestEvent{Type: "test_metadata", UpstreamSentModel: model})
	}
}
func (u *accountTestObservedUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.observeRequest(req)
	resp, err := u.HTTPUpstream.Do(req, proxy, id, concurrency)
	return u.wrap(resp, err)
}
func (u *accountTestObservedUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.observeRequest(req)
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
	return u.wrap(resp, err)
}

func (s *AccountTestService) streamAntigravityAccountTest(c *gin.Context, account *Account, modelID string) error {
	gateway := s.antigravityGatewayService
	if gateway == nil {
		return s.sendErrorAndEnd(c, "Antigravity gateway service not configured")
	}
	modelID = antigravityConnectionTestModel(modelID)
	payload := map[string]any{"model": modelID, "stream": true, "max_tokens": 1024, "messages": []map[string]any{{"role": "user", "content": accountTestEffectivePrompt(c)}}}
	effort := accountTestReasoningEffort(c)
	if effort != "" {
		payload["output_config"] = map[string]any{"effort": effort}
	}
	body, _ := json.Marshal(payload)
	ctx := WithAntigravityRequestEffort(c.Request.Context(), body)
	if message := antigravityEffortRequestError(ctx, account, modelID); message != "" {
		return s.sendErrorAndEnd(c, message)
	}
	sent := resolveFinalAntigravityModelKey(ctx, account, modelID)
	if sent == "" {
		return s.sendErrorAndEnd(c, "Model/effort is not available for this account")
	}
	// Report the resolver's actual named level, including template defaults and
	// complete-suffix precedence. Literal IDs are never split into fake levels.
	effective, err := s.applyAntigravityAccountTestEffort(c, account, payload, sent, antigravityEffortPolicyFromContext(ctx))
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	body, _ = json.Marshal(payload)
	c.Set(accountTestEffectiveEffortKey, effective)
	c.Set("account_test_event_write_mutex", &sync.Mutex{})
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID, UpstreamSentModel: sent, EffectiveEffort: effective, StreamMode: "stream"})
	state := &accountTestRawStreamState{service: s, admin: c, observer: &upstreamResponseModelObserver{}}
	// Reuse dependencies, not the service value: it owns synchronization and
	// continuity state that must not be copied while production Forward is active.
	isolated := NewAntigravityGatewayService(
		gateway.accountRepo, gateway.cache, gateway.schedulerSnapshot,
		gateway.tokenProvider, gateway.rateLimitService,
		&accountTestObservedUpstream{HTTPUpstream: gateway.httpUpstream, state: state},
		gateway.settingService, gateway.internal500Cache,
		nil, gateway.tlsFPProfileService)
	forward := c.Copy()
	writer := &accountTestForwardWriter{ResponseWriter: c.Writer, service: s, admin: c, headers: make(http.Header)}
	forward.Writer = writer
	forward.Request = c.Request.Clone(ctx)
	forward.Request.Body = io.NopCloser(bytes.NewReader(body))
	result, err := isolated.Forward(ctx, forward, account, body, false)
	if ctx.Err() != nil {
		return s.sendErrorAndEnd(c, "Test canceled: "+ctx.Err().Error())
	}
	if err != nil {
		return s.sendFormattedErrorAndEnd(c, buildAntigravityAccountTestError(err, state, writer))
	}
	state.mu.Lock()
	failure, failureDetail, terminal, responseModel, conflict := state.failure, state.failureDetail, state.terminal, state.observer.Model(), state.observer.Conflict()
	upstreamErrorStatus := state.upstreamErrorStatus
	upstreamErrorBody := append([]byte(nil), state.upstreamErrorBody...)
	state.mu.Unlock()
	if failure != nil {
		failureDetail.message = "Stream failed: " + failure.Error()
		return s.sendFormattedErrorAndEnd(c, failureDetail)
	}
	if writerDetail := writer.errorDetail(); writerDetail.message != "" {
		return s.sendFormattedErrorAndEnd(c, writerDetail)
	}
	if !terminal {
		if upstreamErrorStatus >= 400 && len(bytes.TrimSpace(upstreamErrorBody)) > 0 {
			return s.sendFormattedErrorAndEnd(c, buildAccountTestUpstreamError("Upstream", upstreamErrorStatus, upstreamErrorBody))
		}
		return s.sendErrorAndEnd(c, "Upstream stream ended without a generation terminal")
	}
	if !writer.usable {
		return s.sendErrorAndEnd(c, "Upstream returned no usable text, tool or media output")
	}
	if result != nil && result.UpstreamModel != "" {
		s.sendEvent(c, TestEvent{Type: "test_metadata", UpstreamSentModel: result.UpstreamModel, EffectiveEffort: effective, UpstreamResponseModel: responseModel, UpstreamResponseModelConflict: conflict})
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// Match the resolver's explicit opt-in template contract, not ID typography.
func accountTestAntigravityNamedEffort(account *Account, requested, effort, sent string, policy *AntigravityModelEffortSettings) string {
	mapping := account.GetModelMapping()
	if target, matched := resolveRequestedModelInMapping(mapping, requested); matched {
		template, ok := parseAntigravityEffortTemplate(target)
		if !ok {
			return ""
		}
		if _, exact := mapping[requested]; !exact {
			return ""
		}
		if effort != "" {
			return effort
		}
		if template.defaultEffort != "" {
			return template.defaultEffort
		}
		return policy.DefaultEffort
	}
	for _, level := range policy.Levels {
		suffix := "-" + level
		if !strings.HasSuffix(requested, suffix) {
			continue
		}
		base := strings.TrimSuffix(requested, suffix)
		// The formal resolver already chose the winning template (including
		// longest suffix, aliases and wildcards). Verify
		// against that target, not a second ranking of the user level list.
		if template, valid := parseAntigravityEffortTemplate(mapping[base]); valid && template.model(level, policy) == sent {
			return level
		}
	}
	return ""
}

// Shared by request construction and its caller regression: native request
// effort and displayed metadata must follow the model selected by the resolver.
func (s *AccountTestService) applyAntigravityAccountTestEffort(c *gin.Context, account *Account, payload map[string]any, sent string, policy *AntigravityModelEffortSettings) (string, error) {
	requested, _ := payload["model"].(string)
	effort := accountTestReasoningEffort(c)
	effective := accountTestAntigravityNamedEffort(account, requested, effort, sent, policy)
	if effective == "" {
		if err := s.applyAccountTestEffort(c, account, payload, "anthropic"); err != nil {
			return "", err
		}
		value, _ := c.Get(accountTestEffectiveEffortKey)
		effective, _ = value.(string)
	} else if effort != "" {
		payload["output_config"] = map[string]any{"effort": effective}
	}
	return effective, nil
}
