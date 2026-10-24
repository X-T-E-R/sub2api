package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

type antigravityCompatStreamAdapter interface {
	Emit(*apicompat.AnthropicStreamEvent, *antigravityClientWriter)
	// Finalize writes the protocol terminal. suppressSuccessfulTerminal withholds the
	// successful completion frame; the close-out frames and usage are still written.
	Finalize(*antigravityClientWriter, bool)
	WriteError(*antigravityClientWriter, string)
	FinalizeFailure(*antigravityClientWriter, *AntigravityNoUsableAnswerError)
}

// isResponsesTerminalEvent reports the Responses protocol's terminal frames. They carry
// the successful completion signal together with the usage, which Task24 A withholds as
// one unit so the downstream client applies its own continuation policy.
func isResponsesTerminalEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.incomplete"
}

type antigravityChatStreamAdapter struct {
	anthropicState *apicompat.AnthropicEventToResponsesState
	chatState      *apicompat.ResponsesEventToChatState
}

func newAntigravityChatStreamAdapter(model string, includeUsage bool) *antigravityChatStreamAdapter {
	// The Antigravity processor emits full cumulative usage, including an
	// authoritative zero uncached bucket after a fully cached final prompt.
	anthropicState := apicompat.NewAnthropicEventToResponsesStateWithOptions(apicompat.AnthropicToResponsesOptions{
		UsageDeltasAreSnapshots: true,
	})
	anthropicState.Model = model
	chatState := apicompat.NewResponsesEventToChatState()
	chatState.Model = model
	chatState.IncludeUsage = includeUsage
	return &antigravityChatStreamAdapter{
		anthropicState: anthropicState,
		chatState:      chatState,
	}
}

func (a *antigravityChatStreamAdapter) Emit(event *apicompat.AnthropicStreamEvent, writer *antigravityClientWriter) {
	for _, responseEvent := range apicompat.AnthropicEventToResponsesEvents(event, a.anthropicState) {
		a.emitResponseEvent(&responseEvent, writer)
	}
}

func (a *antigravityChatStreamAdapter) Finalize(writer *antigravityClientWriter, suppressSuccessfulTerminal bool) {
	for _, responseEvent := range apicompat.FinalizeAnthropicResponsesStream(a.anthropicState) {
		terminal := isResponsesTerminalEvent(responseEvent.Type)
		for _, chunk := range apicompat.ResponsesEventToChatChunks(&responseEvent, a.chatState) {
			if suppressSuccessfulTerminal && terminal && len(chunk.Choices) > 0 {
				// Chat Completions 没有「不完整」终态：成功 finish 帧不生成，但终态事件里的
				// usage 已经喂给 Chat 状态，下面那条 usage-only 帧（choices 为空）仍然下发，
				// 客户端因此既不会把纯思考响应当成成功完成，也不会丢失真实用量。
				continue
			}
			if data, err := apicompat.ChatChunkToSSE(chunk); err == nil {
				writer.Write([]byte(data))
			}
		}
	}
	for _, chunk := range apicompat.FinalizeResponsesChatStream(a.chatState) {
		if suppressSuccessfulTerminal && len(chunk.Choices) > 0 {
			// finish_reason 帧是成功完成信号；usage-only 帧（choices 为空）保留。
			continue
		}
		if data, err := apicompat.ChatChunkToSSE(chunk); err == nil {
			writer.Write([]byte(data))
		}
	}
	if suppressSuccessfulTerminal {
		// 连同流终止标记一起抑制：只留下内容与 usage，客户端自行决定接续。
		return
	}
	writer.Write([]byte("data: [DONE]\n\n"))
}

func (a *antigravityChatStreamAdapter) FinalizeFailure(writer *antigravityClientWriter, failure *AntigravityNoUsableAnswerError) {
	a.Finalize(writer, true)
	data, _ := json.Marshal(map[string]any{"error": failure.protocolError("chat")})
	writer.Fprintf("data: %s\n\n", data)
}

func (a *antigravityChatStreamAdapter) WriteError(writer *antigravityClientWriter, reason string) {
	writer.Fprintf("data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\"}}\n\n", reason)
}

func (a *antigravityChatStreamAdapter) emitResponseEvent(event *apicompat.ResponsesStreamEvent, writer *antigravityClientWriter) {
	for _, chunk := range apicompat.ResponsesEventToChatChunks(event, a.chatState) {
		if data, err := apicompat.ChatChunkToSSE(chunk); err == nil {
			writer.Write([]byte(data))
		}
	}
}

type antigravityResponsesStreamAdapter struct {
	anthropicState *apicompat.AnthropicEventToResponsesState
}

func newAntigravityResponsesStreamAdapter(model string) *antigravityResponsesStreamAdapter {
	state := apicompat.NewAnthropicEventToResponsesStateWithOptions(apicompat.AnthropicToResponsesOptions{
		PreserveThinkingSignatures: true,
		UsageDeltasAreSnapshots:    true,
	})
	state.Model = model
	return &antigravityResponsesStreamAdapter{anthropicState: state}
}

func (a *antigravityResponsesStreamAdapter) Emit(event *apicompat.AnthropicStreamEvent, writer *antigravityClientWriter) {
	for _, responseEvent := range apicompat.AnthropicEventToResponsesEvents(event, a.anthropicState) {
		a.emitResponseEvent(responseEvent, writer)
	}
}

func (a *antigravityResponsesStreamAdapter) Finalize(writer *antigravityClientWriter, suppressSuccessfulTerminal bool) {
	for _, responseEvent := range apicompat.FinalizeAnthropicResponsesStream(a.anthropicState) {
		if suppressSuccessfulTerminal && isResponsesTerminalEvent(responseEvent.Type) {
			// 成功完成信号换成一个明确不完整的终态：输出项收尾完整、usage 累计保留，
			// 下游据此自行决定是否接续，而不会把纯思考响应当成成功完成。
			responseEvent.Type = "response.incomplete"
			if responseEvent.Response != nil {
				responseEvent.Response.Status = "incomplete"
				responseEvent.Response.IncompleteDetails = nil
			}
		}
		a.emitResponseEvent(responseEvent, writer)
	}
}

func (a *antigravityResponsesStreamAdapter) FinalizeFailure(writer *antigravityClientWriter, failure *AntigravityNoUsableAnswerError) {
	for _, event := range apicompat.FinalizeAnthropicResponsesStream(a.anthropicState) {
		if isResponsesTerminalEvent(event.Type) {
			event.Type = "response.failed"
			if event.Response != nil {
				event.Response.Status = "failed"
				event.Response.IncompleteDetails = nil
				event.Response.Error = &apicompat.ResponsesError{Code: failure.Code, Message: failure.message()}
			}
		}
		a.emitResponseEvent(event, writer)
	}
}

func (a *antigravityResponsesStreamAdapter) WriteError(writer *antigravityClientWriter, reason string) {
	writer.Fprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":%q}}\n\n", reason)
}

func (a *antigravityResponsesStreamAdapter) emitResponseEvent(event apicompat.ResponsesStreamEvent, writer *antigravityClientWriter) {
	if data, err := apicompat.ResponsesEventToSSE(event); err == nil {
		writer.Write([]byte(data))
	}
}

type antigravityCompatScanEvent struct {
	line string
	err  error
}

type antigravityCompatStreamSession struct {
	processor                   *antigravity.StreamingProcessor
	adapter                     antigravityCompatStreamAdapter
	writer                      *antigravityClientWriter
	usage                       *ClaudeUsage
	pendingEvents               []apicompat.AnthropicStreamEvent
	firstTokenMs                *int
	startTime                   time.Time
	meaningfulData              bool
	preContentKeepaliveSent     bool
	preContentKeepaliveInterval time.Duration
	// continuation is the Task24 A/B state of this request, nil while both effective
	// switches are off.
	continuation *antigravityThoughtContinuation
}

const (
	antigravityCompatPreContentKeepaliveInterval = 15 * time.Second
	// Allow slow first tokens, but do not let comment-only streams reset the idle timer forever.
	antigravityCompatPreContentMaxWait = 2 * time.Minute
)

func newAntigravityCompatStreamSession(
	model string,
	startTime time.Time,
	adapter antigravityCompatStreamAdapter,
	writer *antigravityClientWriter,
) *antigravityCompatStreamSession {
	return &antigravityCompatStreamSession{
		processor:                   antigravity.NewStreamingProcessor(model),
		adapter:                     adapter,
		writer:                      writer,
		usage:                       &ClaudeUsage{},
		startTime:                   startTime,
		preContentKeepaliveInterval: antigravityCompatPreContentKeepaliveInterval,
	}
}

func (s *antigravityCompatStreamSession) consume(line string) {
	if s.continuation != nil {
		// 观察这一代上游响应，并决定成功终态是否允许发往下游。
		s.continuation.observeLine(line)
		s.processor.SetWithholdSuccessfulTerminal(s.continuation.withholdSuccessfulTerminal())
	}
	claudeEvents := s.processor.ProcessLine(strings.TrimRight(line, "\r\n"))
	if len(claudeEvents) == 0 {
		return
	}
	s.consumeClaudeEvents(claudeEvents)
}

// suppressSuccessfulTerminal reports A's effective decision for the response that is about
// to be finalized.
func (s *antigravityCompatStreamSession) suppressSuccessfulTerminal() bool {
	return s.continuation.suppressFinalTerminal()
}

func (s *antigravityCompatStreamSession) hasMeaningfulData() bool {
	return s.meaningfulData || s.processor.HasContent()
}

func (s *antigravityCompatStreamSession) writePreContentKeepalive(now time.Time) {
	// Intentional tradeoff: this commits HTTP 200 after 15s, so later upstream
	// failures cannot fail over; report them as SSE errors to the client instead.
	if s.hasMeaningfulData() || s.writer.Disconnected() || now.Sub(s.startTime) < s.preContentKeepaliveInterval {
		return
	}
	if s.writer.Write([]byte(": ping\n\n")) {
		s.preContentKeepaliveSent = true
	}
}

func (s *antigravityCompatStreamSession) finish() (*antigravityStreamResult, error) {
	if s.continuation != nil {
		// A 的有效设置决定最终终态：抑制成功终态时，连 converter 的补发终态也不能出现。
		s.processor.SetWithholdSuccessfulTerminal(s.suppressSuccessfulTerminal())
		// 抑制时仍然补一条只带 usage 的 message_delta（stop_reason 为 null）：Chat
		// Completions/Responses 的终态帧里带着这段真实用量，客户端不会把纯思考响应当成
		// 成功完成，同时也不会丢失用量。
		s.processor.SetUsageOnlyTerminal(s.suppressSuccessfulTerminal())
	}
	finalEvents, usage := s.processor.Finish()
	mergeAntigravityCompatUsage(s.usage, usage)
	s.consumeClaudeEvents(finalEvents)
	noAnswer := (s.continuation.responseFailure() != nil)
	if noAnswer {
		// Empty STOP has no content event to release the buffered message/usage.
		// Publish those real frames before the explicit failed terminal, without
		// inventing content or treating the response as a successful empty stream.
		for i := range s.pendingEvents {
			s.adapter.Emit(&s.pendingEvents[i], s.writer)
		}
		s.pendingEvents = nil
	}
	if !noAnswer && !s.hasMeaningfulData() && !s.writer.Disconnected() {
		if s.preContentKeepaliveSent {
			s.adapter.WriteError(s.writer, "empty_stream")
			return s.result(false), errors.New("empty Antigravity compatibility stream after keepalive")
		}
		return nil, antigravityCompatEmptyStreamError()
	}
	if s.continuation.responseFailure() != nil {
		s.adapter.FinalizeFailure(s.writer, s.continuation.responseFailure())
	} else {
		s.adapter.Finalize(s.writer, s.suppressSuccessfulTerminal())
	}
	return s.result(s.writer.Disconnected()), s.continuation.failureError()
}

func (s *antigravityCompatStreamSession) collectResult(clientDisconnect bool) *antigravityStreamResult {
	_, usage := s.processor.Finish()
	mergeAntigravityCompatUsage(s.usage, usage)
	return s.result(clientDisconnect)
}

func (s *antigravityCompatStreamSession) result(clientDisconnect bool) *antigravityStreamResult {
	return &antigravityStreamResult{
		usage:            s.usage,
		firstTokenMs:     s.firstTokenMs,
		clientDisconnect: clientDisconnect,
	}
}

func (s *antigravityCompatStreamSession) consumeClaudeEvents(data []byte) {
	var eventType string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			s.consumeClaudeData(eventType, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func (s *antigravityCompatStreamSession) consumeClaudeData(eventType, payload string) {
	var event apicompat.AnthropicStreamEvent
	if json.Unmarshal([]byte(payload), &event) != nil {
		return
	}
	if event.Type == "" {
		event.Type = eventType
	}
	if event.Usage != nil {
		mergeAnthropicUsage(s.usage, *event.Usage)
	}
	if event.Message != nil {
		mergeAnthropicUsage(s.usage, event.Message.Usage)
	}
	s.emitOrBuffer(event)
}

func (s *antigravityCompatStreamSession) emitOrBuffer(event apicompat.AnthropicStreamEvent) {
	if s.meaningfulData {
		s.adapter.Emit(&event, s.writer)
		return
	}

	s.pendingEvents = append(s.pendingEvents, event)
	if !isMeaningfulAntigravityCompatEvent(&event) {
		return
	}

	s.meaningfulData = true
	ms := int(time.Since(s.startTime).Milliseconds())
	s.firstTokenMs = &ms
	for i := range s.pendingEvents {
		s.adapter.Emit(&s.pendingEvents[i], s.writer)
	}
	s.pendingEvents = nil
}

func isMeaningfulAntigravityCompatEvent(event *apicompat.AnthropicStreamEvent) bool {
	if event == nil {
		return false
	}
	if event.ContentBlock != nil {
		block := event.ContentBlock
		return block.Type == "tool_use" ||
			block.Text != "" ||
			block.Thinking != "" ||
			block.Source != nil
	}
	if event.Delta != nil {
		delta := event.Delta
		return delta.Text != "" ||
			delta.PartialJSON != "" ||
			delta.Thinking != ""
	}
	return false
}

func mergeAntigravityCompatUsage(dst *ClaudeUsage, src *antigravity.ClaudeUsage) {
	if dst == nil || src == nil {
		return
	}
	dst.InputTokens = src.InputTokens
	dst.OutputTokens = src.OutputTokens
	dst.CacheCreationInputTokens = src.CacheCreationInputTokens
	dst.CacheReadInputTokens = src.CacheReadInputTokens
	dst.ImageOutputTokens = src.ImageOutputTokens
}

func (s *AntigravityGatewayService) handleAntigravityCompatStream(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	adapter antigravityCompatStreamAdapter,
	prefix string,
) (*antigravityStreamResult, error) {
	return s.handleAntigravityCompatStreamWithKeepaliveInterval(c, resp, startTime, originalModel, adapter, prefix, antigravityCompatPreContentKeepaliveInterval, antigravityCompatPreContentMaxWait)
}

func (s *AntigravityGatewayService) handleAntigravityCompatStreamWithKeepaliveInterval(
	c *gin.Context, resp *http.Response, startTime time.Time, originalModel string,
	adapter antigravityCompatStreamAdapter, prefix string, interval, maxPreContentWait time.Duration,
) (*antigravityStreamResult, error) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	writer := newAntigravityClientWriter(c.Writer, flusher, prefix)
	writer.beforeFirstWrite = func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}
	session := newAntigravityCompatStreamSession(originalModel, startTime, adapter, writer)
	session.preContentKeepaliveInterval = interval
	// Task24: A/B state for this request, nil while both effective switches are off.
	session.continuation = antigravityThoughtContinuationFromGin(c)
	events, stopScanner, maxLineSize := s.startAntigravityCompatScanner(resp.Body)
	var scannerStops []func()
	var extraResp *http.Response
	defer func() {
		stopScanner()
		for _, stop := range scannerStops {
			stop()
		}
		if extraResp != nil && extraResp.Body != nil {
			_ = extraResp.Body.Close()
		}
	}()

	timeout := s.antigravityCompatStreamTimeout()
	timeoutTimer, timeoutCh := newAntigravityCompatTimer(timeout)
	if timeoutTimer != nil {
		defer timeoutTimer.Stop()
	}
	keepaliveTicker, keepaliveCh := s.newAntigravityCompatKeepaliveTicker()
	if keepaliveTicker != nil {
		defer keepaliveTicker.Stop()
	}
	preContentDelay := time.Until(startTime.Add(interval))
	if preContentDelay < 0 {
		preContentDelay = 0
	}
	preContentTimer := time.NewTimer(preContentDelay)
	defer preContentTimer.Stop()
	preContentDeadline := startTime.Add(maxPreContentWait)
	deadlineDelay := time.Until(preContentDeadline)
	if deadlineDelay < 0 {
		deadlineDelay = 0
	}
	deadlineTimer := time.NewTimer(deadlineDelay)
	defer deadlineTimer.Stop()
	preContentTimeout := func() (*antigravityStreamResult, error) {
		if session.preContentKeepaliveSent {
			writeAntigravityCompatStreamError(c, adapter, writer, "stream_timeout")
			return session.collectResult(false), fmt.Errorf("pre-content stream timeout")
		}
		return nil, antigravityCompatEmptyStreamError()
	}

	for {
		select {
		case event, open := <-events:
			if !session.hasMeaningfulData() && !writer.Disconnected() && !time.Now().Before(preContentDeadline) {
				return preContentTimeout()
			}
			if !open {
				cont := session.continuation
				eligibleNoAnswer := cont != nil && cont.effectiveGeneration().failureCode(cont.policy) != ""
				if !eligibleNoAnswer && !session.hasMeaningfulData() && !writer.Disconnected() {
					return handleAntigravityCompatEmptyStream(c, session)
				}
				// Task24 B：只思考且正常完成时追加一次生成；失败不形成硬错误，终端由 A
				// 的有效设置决定，第一代 usage 照常入账。
				if cont := session.continuation; cont != nil && cont.canContinue() && !writer.Disconnected() {
					nextResp, finishedUsage, continueErr := cont.continueOnce(c.Request.Context(), resp)
					if continueErr != nil {
						// 追加生成失败：以协议不成功终态 + 错误帧结束，保留内容与累计 usage。
						// 可计费结果与 Task24 终态错误一起返回，调用方先入账再返回错误。
						cont.recordFailure(c, "compat_stream", continueErr)
						finishResult, finishErr := session.finish()
						writeAntigravityCompatStreamError(c, session.adapter, session.writer, "thought_continuation_error")
						if finishErr == nil {
							finishErr = cont.failureError()
						}
						return finishResult, finishErr
					}
					if nextResp != nil {
						session.processor.AddUsageBaseline(toAntigravityUsage(finishedUsage))
						session.processor.ResetGenerationUsage()
						extraResp = nextResp
						scannerStops = append(scannerStops, stopScanner)
						events, stopScanner, maxLineSize = s.startAntigravityCompatScanner(nextResp.Body)
						continue
					}
				}
				if !writer.Disconnected() {
					session.continuation.completeNoUsableAnswer(c, "compat_stream")
				}
				return session.finish()
			}
			if event.err != nil {
				if session.continuation != nil && session.continuation.hasExtraGeneration() {
					// 追加生成的读取失败：协议错误 + 非成功终态，保留已收到的内容与用量，
					// 避免错误路径让调用方丢掉上游已经计量的 usage。
					session.continuation.recordFailure(c, "compat_stream_read", event.err)
					finishResult, finishErr := session.finish()
					writeAntigravityCompatStreamError(c, session.adapter, session.writer, "thought_continuation_error")
					if finishErr == nil {
						finishErr = session.continuation.failureError()
					}
					return finishResult, finishErr
				}
				return s.handleAntigravityCompatReadError(c, session, event.err, maxLineSize, prefix)
			}
			resetAntigravityCompatTimer(timeoutTimer, timeout)
			s.observeAntigravityGeminiSSELine(c, event.line)
			session.consume(event.line)

		case <-timeoutCh:
			if writer.Disconnected() {
				return session.collectResult(true), nil
			}
			if !session.hasMeaningfulData() {
				return preContentTimeout()
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (%s)", prefix)
			writeAntigravityCompatStreamError(c, adapter, writer, "stream_timeout")
			return session.collectResult(false), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if session.hasMeaningfulData() && !writer.Disconnected() {
				writer.Write([]byte(": ping\n\n"))
			}
		case now := <-preContentTimer.C:
			if !session.hasMeaningfulData() && !writer.Disconnected() && !now.Before(preContentDeadline) {
				return preContentTimeout()
			}
			session.writePreContentKeepalive(now)
			if !session.hasMeaningfulData() && !writer.Disconnected() {
				preContentTimer.Reset(interval)
			}
		case <-deadlineTimer.C:
			if !session.hasMeaningfulData() && !writer.Disconnected() {
				return preContentTimeout()
			}
		}
	}
}

func handleAntigravityCompatEmptyStream(c *gin.Context, session *antigravityCompatStreamSession) (*antigravityStreamResult, error) {
	if session.preContentKeepaliveSent {
		writeAntigravityCompatStreamError(c, session.adapter, session.writer, "empty_stream")
		return session.collectResult(false), errors.New("empty Antigravity compatibility stream after keepalive")
	}
	return nil, antigravityCompatEmptyStreamError()
}

func (s *AntigravityGatewayService) startAntigravityCompatScanner(
	body io.Reader,
) (<-chan antigravityCompatScanEvent, func(), int) {
	maxLineSize := defaultMaxLineSize
	if s.settingService != nil && s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}
	scanner := bufio.NewScanner(body)
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

	events := make(chan antigravityCompatScanEvent, 16)
	done := make(chan struct{})
	go func() {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		send := func(event antigravityCompatScanEvent) bool {
			select {
			case events <- event:
				return true
			case <-done:
				return false
			}
		}
		for scanner.Scan() {
			if !send(antigravityCompatScanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			send(antigravityCompatScanEvent{err: err})
		}
	}()
	return events, func() { close(done) }, maxLineSize
}

func (s *AntigravityGatewayService) antigravityCompatStreamTimeout() time.Duration {
	if s.settingService == nil || s.settingService.cfg == nil {
		return 0
	}
	return time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
}

func (s *AntigravityGatewayService) newAntigravityCompatKeepaliveTicker() (*time.Ticker, <-chan time.Time) {
	if s.settingService == nil || s.settingService.cfg == nil {
		return nil, nil
	}
	interval := time.Duration(s.settingService.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	if interval <= 0 {
		return nil, nil
	}
	ticker := time.NewTicker(interval)
	return ticker, ticker.C
}

func newAntigravityCompatTimer(timeout time.Duration) (*time.Timer, <-chan time.Time) {
	if timeout <= 0 {
		return nil, nil
	}
	timer := time.NewTimer(timeout)
	return timer, timer.C
}

func resetAntigravityCompatTimer(timer *time.Timer, timeout time.Duration) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func (s *AntigravityGatewayService) handleAntigravityCompatReadError(
	c *gin.Context,
	session *antigravityCompatStreamSession,
	err error,
	maxLineSize int,
	prefix string,
) (*antigravityStreamResult, error) {
	if !session.hasMeaningfulData() && !session.writer.Disconnected() {
		if session.preContentKeepaliveSent {
			writeAntigravityCompatStreamError(c, session.adapter, session.writer, "stream_read_error")
			return session.collectResult(false), fmt.Errorf("stream read error: %w", err)
		}
		return nil, antigravityCompatEmptyStreamError()
	}
	if disconnect, handled := handleStreamReadError(err, session.writer.Disconnected(), prefix); handled {
		return session.collectResult(disconnect), nil
	}
	session.continuation.markReadFailure()
	if errors.Is(err, bufio.ErrTooLong) {
		logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (%s): max_size=%d error=%v", prefix, maxLineSize, err)
		writeAntigravityCompatStreamError(c, session.adapter, session.writer, "response_too_large")
		return session.result(false), err
	}
	writeAntigravityCompatStreamError(c, session.adapter, session.writer, "stream_read_error")
	return nil, fmt.Errorf("stream read error: %w", err)
}

func writeAntigravityCompatStreamError(
	c *gin.Context,
	adapter antigravityCompatStreamAdapter,
	writer *antigravityClientWriter,
	reason string,
) {
	adapter.WriteError(writer, reason)
	MarkResponseCommitted(c)
}

func antigravityCompatEmptyStreamError() error {
	logger.LegacyPrintf("service.antigravity_gateway", "Empty Antigravity compatibility stream, triggering failover")
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
		RetryableOnSameAccount: true,
	}
}

func (s *AntigravityGatewayService) handleChatCompletionsStreamingFromAntigravity(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	includeUsage bool,
) (*antigravityStreamResult, error) {
	return s.handleAntigravityCompatStream(
		c,
		resp,
		startTime,
		originalModel,
		newAntigravityChatStreamAdapter(originalModel, includeUsage),
		"antigravity chat completions stream",
	)
}

func (s *AntigravityGatewayService) handleResponsesStreamingFromAntigravity(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
) (*antigravityStreamResult, error) {
	return s.handleAntigravityCompatStream(
		c,
		resp,
		startTime,
		originalModel,
		newAntigravityResponsesStreamAdapter(originalModel),
		"antigravity responses stream",
	)
}
