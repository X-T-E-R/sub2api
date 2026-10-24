package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type antigravityStreamResult struct {
	usage            *ClaudeUsage
	firstTokenMs     *int
	clientDisconnect bool // 客户端是否在流式传输过程中断开
}

func (s *AntigravityGatewayService) observeAntigravityGeminiSSELine(c *gin.Context, line string) {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	// Observe the original payload: ObserveGemini supports both the v1internal
	// wrapper and direct Gemini response shapes. The main stream handler will
	// unwrap the same line for business processing, so unwrapping here would be
	// duplicate work on every SSE event.
	observer.ObserveGemini([]byte(payload))
}

// antigravityClientWriter 封装流式响应的客户端写入，自动检测断开并标记。
// 断开后所有写入操作变为 no-op，调用方通过 Disconnected() 判断是否继续 drain 上游。
type antigravityClientWriter struct {
	w                gin.ResponseWriter
	flusher          http.Flusher
	disconnected     bool
	prefix           string // 日志前缀，标识来源方法
	beforeFirstWrite func()
	onWrite          func([]byte, int, error)
	onDisconnect     func()
}

func newAntigravityClientWriter(w gin.ResponseWriter, flusher http.Flusher, prefix string) *antigravityClientWriter {
	return &antigravityClientWriter{w: w, flusher: flusher, prefix: prefix}
}

// Write 写入数据到客户端，写入失败时标记断开并返回 false
func (cw *antigravityClientWriter) Write(p []byte) bool {
	if cw.disconnected {
		return false
	}
	cw.prepareFirstWrite()
	n, err := cw.w.Write(p)
	if cw.onWrite != nil {
		cw.onWrite(p, n, err)
	}
	if err != nil {
		cw.markDisconnected()
		return false
	}
	cw.flusher.Flush()
	return true
}

// Fprintf 格式化写入数据到客户端，写入失败时标记断开并返回 false
func (cw *antigravityClientWriter) Fprintf(format string, args ...any) bool {
	return cw.Write([]byte(fmt.Sprintf(format, args...)))
}

func (cw *antigravityClientWriter) Disconnected() bool { return cw.disconnected }

func (cw *antigravityClientWriter) prepareFirstWrite() {
	if cw.beforeFirstWrite == nil {
		return
	}
	prepare := cw.beforeFirstWrite
	cw.beforeFirstWrite = nil
	prepare()
}

func (cw *antigravityClientWriter) markDisconnected() {
	if cw.disconnected {
		return
	}
	cw.disconnected = true
	if cw.onDisconnect != nil {
		cw.onDisconnect()
	}
	logger.LegacyPrintf("service.antigravity_gateway", "Client disconnected during streaming (%s), continuing to drain upstream for billing", cw.prefix)
}

// geminiCaptureResponseWriter taps the bytes that Gin's normal Data renderer
// actually hands to the underlying response writer. It delegates every other
// ResponseWriter method unchanged, so status/content-type/header semantics stay
// exactly on Gin's existing path.
type geminiCaptureResponseWriter struct {
	gin.ResponseWriter
	capture *GeminiCaptureRequest
}

func (w *geminiCaptureResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.record(p, n, err)
	return n, err
}

func (w *geminiCaptureResponseWriter) WriteString(value string) (int, error) {
	n, err := w.ResponseWriter.WriteString(value)
	w.record([]byte(value), n, err)
	return n, err
}

func (w *geminiCaptureResponseWriter) record(p []byte, n int, err error) {
	if w.capture == nil {
		return
	}
	w.capture.RecordConvertedWrite(p, n, err)
	if err != nil || n != len(p) {
		w.capture.MarkClientDisconnect()
	}
}

func writeClaudeNonStreamingResponse(c *gin.Context, body []byte) {
	writeClaudeResponse(c, body, http.StatusOK)
}

func writeClaudeResponse(c *gin.Context, body []byte, status int) {
	capture := GeminiCaptureFromContext(c.Request.Context())
	if capture == nil {
		c.Data(status, "application/json", body)
		return
	}
	originalWriter := c.Writer
	c.Writer = &geminiCaptureResponseWriter{ResponseWriter: originalWriter, capture: capture}
	defer func() { c.Writer = originalWriter }()
	c.Data(status, "application/json", body)
}

// handleStreamReadError 处理上游读取错误的通用逻辑。
// 返回 (clientDisconnect, handled)：handled=true 表示错误已处理，调用方应返回已收集的 usage。
func handleStreamReadError(err error, clientDisconnected bool, prefix string) (disconnect bool, handled bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		logger.LegacyPrintf("service.antigravity_gateway", "Context canceled during streaming (%s), returning collected usage", prefix)
		return true, true
	}
	if clientDisconnected {
		logger.LegacyPrintf("service.antigravity_gateway", "Upstream read error after client disconnect (%s): %v, returning collected usage", prefix, err)
		return true, true
	}
	return false, false
}

func (s *AntigravityGatewayService) handleGeminiStreamingResponse(c *gin.Context, resp *http.Response, startTime time.Time) (*antigravityStreamResult, error) {
	if upstreamResponseModelObserverFromContext(c) == nil {
		beginUpstreamResponseModelObservation(c)
	}
	// Native Gemini writes directly through the client writer. Keep the capture
	// tap on that final converted wire as well; when disabled, leave Gin's
	// writer untouched.
	originalWriter := c.Writer
	if capture := GeminiCaptureFromContext(c.Request.Context()); capture != nil {
		c.Writer = &geminiCaptureResponseWriter{ResponseWriter: originalWriter, capture: capture}
		defer func() { c.Writer = originalWriter }()
	}
	c.Status(resp.StatusCode)
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/event-stream; charset=utf-8"
	}
	c.Header("Content-Type", contentType)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	// 使用 Scanner 并限制单行大小，避免 ReadString 无上限导致 OOM
	maxLineSize := defaultMaxLineSize
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}
	// Task24: A/B state for this request, nil while both effective switches are off.
	cont := antigravityThoughtContinuationFromGin(c)
	usage := &ClaudeUsage{}
	var firstTokenMs *int
	sawUpstreamObject := false

	type scanEvent struct {
		line string
		err  error
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	// 独立 goroutine 读取上游，避免读取阻塞影响超时处理。每个 source 持有自己的 done，
	// Task24 的追加生成为第二个上游响应再起一个 source。
	startScanner := func(body io.Reader) (<-chan scanEvent, func()) {
		scanner := bufio.NewScanner(body)
		scanBuf := getSSEScannerBuf64K()
		scanner.Buffer(scanBuf[:0], maxLineSize)
		events := make(chan scanEvent, 16)
		done := make(chan struct{})
		sendEvent := func(ev scanEvent) bool {
			select {
			case events <- ev:
				return true
			case <-done:
				return false
			}
		}
		go func() {
			defer putSSEScannerBuf64K(scanBuf)
			defer close(events)
			for scanner.Scan() {
				atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
				if !sendEvent(scanEvent{line: scanner.Text()}) {
					return
				}
			}
			if err := scanner.Err(); err != nil {
				_ = sendEvent(scanEvent{err: err})
			}
		}()
		return events, func() { close(done) }
	}
	events, stopScanner := startScanner(resp.Body)
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

	// 上游数据间隔超时保护（防止上游挂起长期占用连接）
	streamInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	// 下游 keepalive：防止代理/Cloudflare Tunnel 因连接空闲而断开
	keepaliveInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.settingService.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	lastDataAt := time.Now()

	cw := newAntigravityClientWriter(c.Writer, flusher, "antigravity gemini")

	// 仅发送一次错误事件，避免多次写入导致协议混乱
	errorEventSent := false
	sendErrorEvent := func(reason string) {
		if errorEventSent || cw.Disconnected() {
			return
		}
		errorEventSent = true
		cw.Fprintf("event: error\ndata: {\"error\":\"%s\"}\n\n", reason)
	}

	// Task24 A/B: a successful terminal (finishReason=STOP) is held back until the
	// downstream decision is known, so that A can suppress only that completion signal and
	// B can append its single generation before the client sees one terminal. Nothing is
	// rewritten while the response keeps producing content: as soon as this generation
	// stops being a thought-only completion the held frames are released verbatim.
	type heldFrame struct {
		frame   string
		payload string
	}
	var heldFrames []heldFrame
	holdTerminal := false
	flushHeld := func(suppressSuccessfulTerminal bool) {
		if len(heldFrames) == 0 {
			holdTerminal = false
			return
		}
		for _, held := range heldFrames {
			frame := held.frame
			if suppressSuccessfulTerminal && held.payload != "" {
				if stripped, changed := antigravityStripSuccessfulFinishReason([]byte(held.payload)); changed {
					frame = "data: " + string(stripped) + "\n\n"
				}
			}
			cw.Write([]byte(frame))
		}
		heldFrames = nil
		holdTerminal = false
	}
	emitFrame := func(frame, payload string) {
		if holdTerminal {
			heldFrames = append(heldFrames, heldFrame{frame: frame, payload: payload})
			return
		}
		cw.Write([]byte(frame))
	}
	result := func(clientDisconnect bool) *antigravityStreamResult {
		final := *usage
		if cont != nil && cont.hasExtraGeneration() {
			// 两次生成对下游是一条响应，usage 必须是两代累计值。
			final = cont.cumulativeUsage(final)
		}
		return &antigravityStreamResult{usage: &final, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnect}
	}
	defer func() {
		if holdTerminal {
			// 任何提前退出的路径都要把暂存的终态按 A 的有效设置放行，绝不悬挂。
			flushHeld(cont != nil && cont.suppressFinalTerminal())
		}
	}()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !sawUpstreamObject && c.Request.Context().Err() != nil {
					return result(true), nil
				}
				if !sawUpstreamObject && !cw.Disconnected() {
					if !c.Writer.Written() {
						return nil, &UpstreamFailoverError{
							StatusCode:             http.StatusBadGateway,
							ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
							RetryableOnSameAccount: true,
						}
					}
					// A keepalive may have committed HTTP 200; report failure without replay.
					sendErrorEvent("empty_stream_response")
					return result(false), errors.New("empty stream response after client output")
				}
				// Task24 B：只思考且正常完成时追加一次生成；失败不形成硬错误，终端由 A
				// 的有效设置决定，第一代 usage 照常入账。
				if cont != nil && cont.canContinue() && !cw.Disconnected() {
					nextResp, _, continueErr := cont.continueOnce(c.Request.Context(), resp)
					if continueErr != nil {
						// 追加生成失败：第一代的成功完成信号一律不发，改为协议错误；已收到
						// 的内容与累计 usage 照常下发。
						cont.recordFailure(c, "gemini_stream", continueErr)
						flushHeld(true)
						if !errors.Is(continueErr, context.Canceled) && !errors.Is(continueErr, context.DeadlineExceeded) {
							sendErrorEvent("thought_continuation_error")
						}
						return result(cw.Disconnected()), cont.failureError()
					}
					if nextResp != nil {
						// 整条响应只出现一次终态：第一代的成功完成信号让位给追加生成。
						flushHeld(true)
						extraResp = nextResp
						scannerStops = append(scannerStops, stopScanner)
						events, stopScanner = startScanner(nextResp.Body)
						continue
					}
				}
				noAnswer := !cw.Disconnected() && cont.completeNoUsableAnswer(c, "gemini_stream")
				flushHeld(cont != nil && cont.suppressFinalTerminal())
				if noAnswer {
					data, _ := json.Marshal(map[string]any{"error": antigravityNoUsableAnswerGoogleError(cont.responseFailure())})
					cw.Fprintf("event: error\ndata: %s\n\n", data)
				}
				return result(cw.Disconnected()), cont.failureError()
			}
			if ev.err != nil {
				if cont != nil && cont.hasExtraGeneration() {
					// 追加生成的读取失败：不发第一代的成功终态，改为协议错误；已计量的
					// usage 与已收到的内容照常下发。
					cont.recordFailure(c, "gemini_stream_read", ev.err)
					flushHeld(true)
					if !errors.Is(ev.err, context.Canceled) && !errors.Is(ev.err, context.DeadlineExceeded) {
						sendErrorEvent("thought_continuation_error")
					}
					return result(cw.Disconnected()), cont.failureError()
				}
				if disconnect, handled := handleStreamReadError(ev.err, cw.Disconnected(), "antigravity gemini"); handled {
					flushHeld(cont != nil && cont.suppressFinalTerminal())
					return result(disconnect), nil
				}
				cont.markReadFailure()
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (antigravity): max_size=%d error=%v", maxLineSize, ev.err)
					sendErrorEvent("response_too_large")
					return result(false), ev.err
				}
				sendErrorEvent("stream_read_error")
				return nil, ev.err
			}

			lastDataAt = time.Now()

			line := ev.line
			s.observeAntigravityGeminiSSELine(c, line)
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "" || payload == "[DONE]" {
					if sawUpstreamObject {
						emitFrame(line+"\n", "")
					}
					continue
				}

				// 解包 v1internal 响应
				inner, parseErr := s.unwrapV1InternalResponse([]byte(payload))
				if parseErr == nil && inner != nil {
					payload = string(inner)
				}

				if !gjson.ParseBytes(inner).IsObject() {
					continue
				}
				sawUpstreamObject = true

				// 解析 usage
				if u := extractGeminiUsage(inner); u != nil {
					usage = u
				}
				var parsed map[string]any
				if json.Unmarshal(inner, &parsed) == nil {
					// Check for MALFORMED_FUNCTION_CALL
					if candidates, ok := parsed["candidates"].([]any); ok && len(candidates) > 0 {
						if cand, ok := candidates[0].(map[string]any); ok {
							if fr, ok := cand["finishReason"].(string); ok && fr == "MALFORMED_FUNCTION_CALL" {
								logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] MALFORMED_FUNCTION_CALL detected in forward stream")
								if content, ok := cand["content"]; ok {
									if b, err := json.Marshal(content); err == nil {
										logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] Malformed content: %s", string(b))
									}
								}
							}
						}
					}
				}

				if firstTokenMs == nil {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}

				if cont != nil {
					cont.observePayload(inner)
					if holdTerminal && !cont.withholdSuccessfulTerminal() {
						// 这一代不再是只有思考的正常完成：暂存帧原样放行，不误伤正文/工具。
						flushHeld(false)
					}
					if !holdTerminal && cont.withholdSuccessfulTerminal() &&
						antigravityPayloadHasSuccessfulFinishReason(inner) {
						holdTerminal = true
					}
					if cont.hasExtraGeneration() {
						if withBaseline, changed := antigravityAddUsageBaseline([]byte(payload), cont.baseRawUsage); changed {
							payload = string(withBaseline)
						}
					}
				}

				emitFrame("data: "+payload+"\n\n", payload)
				continue
			}

			if sawUpstreamObject {
				emitFrame(line+"\n", "")
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if cw.Disconnected() {
				logger.LegacyPrintf("service.antigravity_gateway", "Upstream timeout after client disconnect (antigravity gemini), returning collected usage")
				flushHeld(cont != nil && cont.suppressFinalTerminal())
				return result(true), nil
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (antigravity)")
			if capture := GeminiCaptureFromContext(c.Request.Context()); capture != nil {
				capture.MarkTimeout()
			}
			sendErrorEvent("stream_timeout")
			return result(false), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if cw.Disconnected() {
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// SSE ping/keepalive：保持连接活跃防止 Cloudflare Tunnel 等代理断开
			if !cw.Fprintf(":\n\n") {
				logger.LegacyPrintf("service.antigravity_gateway", "Client disconnected during keepalive ping (antigravity gemini), continuing to drain upstream for billing")
				continue
			}
		}
	}
}

// handleGeminiStreamToNonStreaming 读取上游流式响应，合并为非流式响应返回给客户端
// Gemini 流式响应是增量的，需要累积所有 chunk 的内容
func (s *AntigravityGatewayService) handleGeminiStreamToNonStreaming(c *gin.Context, resp *http.Response, startTime time.Time) (*antigravityStreamResult, error) {
	if upstreamResponseModelObserverFromContext(c) == nil {
		beginUpstreamResponseModelObservation(c)
	}
	// The non-streaming native route also writes its converted JSON directly via
	// Gin, so tap the actual client wire without changing the disabled path.
	originalWriter := c.Writer
	if capture := GeminiCaptureFromContext(c.Request.Context()); capture != nil {
		c.Writer = &geminiCaptureResponseWriter{ResponseWriter: originalWriter, capture: capture}
		defer func() { c.Writer = originalWriter }()
	}
	// Task24: A/B state for this request, nil while both effective switches are off.
	cont := antigravityThoughtContinuationFromGin(c)
	maxLineSize := defaultMaxLineSize
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}

	usage := &ClaudeUsage{}
	var firstTokenMs *int
	var last map[string]any
	var lastWithParts map[string]any
	var collectedParts []map[string]any // 收集所有 parts，保留 thinking/tool/signature 顺序
	// Task24 B：追加生成开始时第一代的响应体被移到快照里；只有追加生成真的产生了响应体时，
	// 下游身体才属于第二代，第一代的用量只能累计一次。
	var firstLast, firstLastWithParts map[string]any
	lastFromAppended, lastWithPartsFromAppended := false, false

	type scanEvent struct {
		line string
		err  error
	}

	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	// 独立 goroutine 读取上游，避免读取阻塞影响超时处理；每个 source 持有自己的 done，
	// Task24 的追加生成为第二个上游响应再起一个 source。
	startScanner := func(body io.Reader) (<-chan scanEvent, func()) {
		scanner := bufio.NewScanner(body)
		scanBuf := getSSEScannerBuf64K()
		scanner.Buffer(scanBuf[:0], maxLineSize)
		events := make(chan scanEvent, 16)
		done := make(chan struct{})
		sendEvent := func(ev scanEvent) bool {
			select {
			case events <- ev:
				return true
			case <-done:
				return false
			}
		}
		go func() {
			defer putSSEScannerBuf64K(scanBuf)
			defer close(events)
			for scanner.Scan() {
				atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
				if !sendEvent(scanEvent{line: scanner.Text()}) {
					return
				}
			}
			if err := scanner.Err(); err != nil {
				_ = sendEvent(scanEvent{err: err})
			}
		}()
		return events, func() { close(done) }
	}
	events, stopScanner := startScanner(resp.Body)
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

	// 上游数据间隔超时保护（防止上游挂起长期占用连接）
	streamInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Task24 B：第一代只有思考且正常完成时，追加一次生成（原样信封 + 实际
				// 收到的思考/签名）。追加失败不变成硬错误：下游拿到第一代的合法响应，
				// 终态由 A 的有效设置决定，第一代 usage 照常入账。
				if cont != nil && cont.canContinue() {
					nextResp, _, continueErr := cont.continueOnce(c.Request.Context(), resp)
					if continueErr != nil {
						// 追加生成失败：非流式响应以非成功终态结束（finishReason 被抑制），
						// 内容与第一代 usage 照常返回，调用方正常计费。
						cont.recordFailure(c, "gemini_collect", continueErr)
					} else if nextResp != nil {
						// 新的一代开始：第一代的响应体放入快照并清空当前代的收集状态，
						// 避免只有第一代有内容时它的响应体与用量被累计两次。
						firstLast, firstLastWithParts = last, lastWithParts
						last, lastWithParts, usage = nil, nil, nil
						extraResp = nextResp
						scannerStops = append(scannerStops, stopScanner)
						events, stopScanner = startScanner(nextResp.Body)
						continue
					}
				}
				// 流结束，返回收集的响应
				goto returnResponse
			}
			if ev.err != nil {
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (antigravity non-stream): max_size=%d error=%v", maxLineSize, ev.err)
				}
				if cont != nil && cont.hasExtraGeneration() {
					// 追加生成的读取失败照常返回第一代的合法响应，usage 累计入账。
					cont.recordFailure(c, "gemini_collect_read", ev.err)
					goto returnResponse
				}
				cont.markReadFailure()
				return nil, ev.err
			}

			line := ev.line
			s.observeAntigravityGeminiSSELine(c, line)
			trimmed := strings.TrimRight(line, "\r\n")

			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}

			// 解包 v1internal 响应
			inner, parseErr := s.unwrapV1InternalResponse([]byte(payload))
			if parseErr != nil {
				continue
			}

			var parsed map[string]any
			if err := json.Unmarshal(inner, &parsed); err != nil {
				continue
			}

			// 记录首 token 时间
			if firstTokenMs == nil {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}

			last = parsed
			lastFromAppended = cont != nil && cont.hasExtraGeneration()

			// 提取 usage
			if u := extractGeminiUsage(inner); u != nil {
				usage = u
			}

			// Check for MALFORMED_FUNCTION_CALL
			if candidates, ok := parsed["candidates"].([]any); ok && len(candidates) > 0 {
				if cand, ok := candidates[0].(map[string]any); ok {
					if fr, ok := cand["finishReason"].(string); ok && fr == "MALFORMED_FUNCTION_CALL" {
						logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] MALFORMED_FUNCTION_CALL detected in forward non-stream collect")
						if content, ok := cand["content"]; ok {
							if b, err := json.Marshal(content); err == nil {
								logger.LegacyPrintf("service.antigravity_gateway", "[Antigravity] Malformed content: %s", string(b))
							}
						}
					}
				}
			}

			// 保留最后一个有 parts 的响应
			if parts := extractGeminiParts(parsed); len(parts) > 0 {
				lastWithParts = parsed
				lastWithPartsFromAppended = cont != nil && cont.hasExtraGeneration()
				// 收集所有 parts，保留 thinking/tool/signature 的原始顺序
				collectedParts = append(collectedParts, parts...)
			}

			if cont != nil {
				cont.observePayload(inner)
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (antigravity non-stream)")
			return nil, fmt.Errorf("stream data interval timeout")
		}
	}

returnResponse:
	if cont != nil && cont.hasExtraGeneration() && last == nil && lastWithParts == nil {
		// 追加生成没有产生任何可解析数据：下游看到的仍是第一代的合法响应，用量取第一代
		// （它已经记入累计基数，这里不能再叠加一次）。
		last, lastWithParts = firstLast, firstLastWithParts
		usage = nil
	}

	// 选择最后一个有效响应
	finalResponse := pickGeminiCollectResult(last, lastWithParts)
	cont.reconcileCollectedTerminal(finalResponse)

	// 处理空响应情况 — 触发同账号重试 + failover 切换账号
	if last == nil && lastWithParts == nil && !cont.completeNoUsableAnswer(c, "gemini_collect") {
		logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-Forward] warning: empty stream response (gemini non-stream), triggering failover")
		return nil, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
			RetryableOnSameAccount: true,
		}
	}

	// 将所有收集到的 parts 合并回最终响应，保留 thinking、tool
	// 和 thoughtSignature carrier 的顺序与内容。
	if len(collectedParts) > 0 {
		finalResponse = mergeCollectedPartsToResponse(finalResponse, collectedParts)
	}

	if cont != nil {
		// 只有最终响应体真的来自追加生成时，此前各代的原始用量才能叠加到它上面；否则身体
		// 还是第一代的，叠加会把第一代算两次。
		bodyFromAppendedGeneration := false
		if lastWithParts != nil {
			bodyFromAppendedGeneration = lastWithPartsFromAppended
		} else if last != nil {
			bodyFromAppendedGeneration = lastFromAppended
		}
		if cont.hasExtraGeneration() {
			if bodyFromAppendedGeneration {
				// 下游与计费必须看到两代上游用量的累计值。
				antigravityAddUsageBaselineToResponse(finalResponse, cont.baseRawUsage)
			}
			finalUsage := ClaudeUsage{}
			if usage != nil {
				finalUsage = *usage
			}
			finalUsage = cont.cumulativeUsage(finalUsage)
			usage = &finalUsage
		}
		if cont.suppressFinalTerminal() {
			// Task24 A：只抑制成功终态，content/usageMetadata 原样保留。
			antigravityStripSuccessfulFinishReasonFromResponse(finalResponse)
		}
	}

	status := http.StatusOK
	if cont.completeNoUsableAnswer(c, "gemini_collect") {
		status = cont.responseFailure().status()
		finalResponse["error"] = antigravityNoUsableAnswerGoogleError(cont.responseFailure())
	}
	respBody, err := json.Marshal(finalResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}
	// Final counters and retained modality details must describe the same billable response.
	if finalUsage := extractGeminiUsage(respBody); finalUsage != nil {
		usage = finalUsage
	}
	c.Data(status, "application/json", respBody)

	// 追加生成失败时，第一代的合法响应体已经写出（非成功终态），结果与终态错误一起返回。
	return &antigravityStreamResult{usage: usage, firstTokenMs: firstTokenMs}, cont.failureError()
}

// getOrCreateGeminiParts 获取 Gemini 响应的 parts 结构，返回深拷贝和更新回调
func getOrCreateGeminiParts(response map[string]any) (result map[string]any, existingParts []any, setParts func([]any)) {
	// 深拷贝 response
	result = make(map[string]any)
	for k, v := range response {
		result[k] = v
	}

	// 获取或创建 candidates
	candidates, ok := result["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		candidates = []any{map[string]any{}}
	}

	// 获取第一个 candidate
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		candidate = make(map[string]any)
		candidates[0] = candidate
	}

	// 获取或创建 content
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		content = map[string]any{"role": "model"}
		candidate["content"] = content
	}

	// 获取现有 parts
	existingParts, ok = content["parts"].([]any)
	if !ok {
		existingParts = []any{}
	}

	// 返回更新回调
	setParts = func(newParts []any) {
		content["parts"] = newParts
		result["candidates"] = candidates
	}

	return result, existingParts, setParts
}

// mergeCollectedPartsToResponse 将收集的所有 parts 合并到 Gemini 响应中
// 这个函数会合并所有类型的 parts：text、thinking、functionCall、inlineData 等
// 保持原始顺序，只合并连续的普通 text parts
func mergeCollectedPartsToResponse(response map[string]any, collectedParts []map[string]any) map[string]any {
	if len(collectedParts) == 0 {
		return response
	}

	result, _, setParts := getOrCreateGeminiParts(response)

	// 合并策略：
	// 1. 保持原始顺序
	// 2. 连续的普通 text parts 合并为一个
	// 3. thinking、functionCall、inlineData 等保持原样
	var mergedParts []any
	var textBuffer strings.Builder

	flushTextBuffer := func() {
		if textBuffer.Len() > 0 {
			mergedParts = append(mergedParts, map[string]any{
				"text": textBuffer.String(),
			})
			textBuffer.Reset()
		}
	}

	for _, part := range collectedParts {
		text, hasText := part["text"].(string)
		thought, _ := part["thought"].(bool)
		_, hasSignature := part["thoughtSignature"]

		switch {
		case thought:
			// Thinking parts (including empty carriers) must never be mixed into
			// ordinary text and retain their thoughtSignature field.
			flushTextBuffer()
			mergedParts = append(mergedParts, part)
		case hasSignature:
			// A non-thought signature carrier belongs at this exact boundary. Do
			// not turn it into ordinary text or drop the signature key.
			flushTextBuffer()
			mergedParts = append(mergedParts, part)
		case hasText:
			// Only unsigned ordinary text may be coalesced.
			_, _ = textBuffer.WriteString(text)
		default:
			// Non-text parts (functionCall, inlineData, etc.) retain order.
			flushTextBuffer()
			mergedParts = append(mergedParts, part)
		}
	}

	// 刷新剩余的 text
	flushTextBuffer()

	setParts(mergedParts)
	return result
}

func (s *AntigravityGatewayService) writeClaudeError(c *gin.Context, status int, errType, message string) error {
	MarkResponseCommitted(c)
	c.JSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": errType, "message": message},
	})
	return fmt.Errorf("%s", message)
}

// WriteMappedClaudeError 导出版本，供 handler 层使用（如 fallback 错误处理）
func (s *AntigravityGatewayService) WriteMappedClaudeError(c *gin.Context, account *Account, upstreamStatus int, upstreamRequestID string, body []byte) error {
	return s.writeMappedClaudeError(c, account, upstreamStatus, upstreamRequestID, body)
}

func (s *AntigravityGatewayService) writeMappedClaudeError(c *gin.Context, account *Account, upstreamStatus int, upstreamRequestID string, body []byte) error {
	MarkResponseCommitted(c)
	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
	logBody, maxBytes := s.getLogConfig()
	upstreamDetail := s.getUpstreamErrorDetail(body)
	setOpsUpstreamError(c, upstreamStatus, upstreamMsg, upstreamDetail)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: upstreamStatus,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "http_error",
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})

	// 记录上游错误详情便于排障（可选：由配置控制；不回显到客户端）
	if logBody {
		logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-Forward] upstream_error status=%d body=%s", upstreamStatus, truncateForLog(body, maxBytes))
	}

	// 检查错误透传规则
	if ptStatus, ptErrType, ptErrMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, upstreamStatus, body,
		0, "", "",
	); matched {
		c.JSON(ptStatus, gin.H{
			"type":  "error",
			"error": gin.H{"type": ptErrType, "message": ptErrMsg},
		})
		if upstreamMsg == "" {
			return fmt.Errorf("upstream error: %d", upstreamStatus)
		}
		return fmt.Errorf("upstream error: %d message=%s", upstreamStatus, upstreamMsg)
	}

	var statusCode int
	var errType, errMsg string

	switch upstreamStatus {
	case 400:
		statusCode = http.StatusBadRequest
		errType = "invalid_request_error"
		errMsg = getPassthroughOrDefault(upstreamMsg, "Invalid request")
	case 401:
		statusCode = http.StatusBadGateway
		errType = "authentication_error"
		errMsg = "Upstream authentication failed"
	case 403:
		statusCode = http.StatusBadGateway
		errType = "permission_error"
		errMsg = "Upstream access forbidden"
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = "Upstream rate limit exceeded"
	case 529:
		statusCode = http.StatusServiceUnavailable
		errType = "overloaded_error"
		errMsg = "Upstream service overloaded"
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream request failed"
	}

	c.JSON(statusCode, gin.H{
		"type":  "error",
		"error": gin.H{"type": errType, "message": errMsg},
	})
	if upstreamMsg == "" {
		return fmt.Errorf("upstream error: %d", upstreamStatus)
	}
	return fmt.Errorf("upstream error: %d message=%s", upstreamStatus, upstreamMsg)
}

func (s *AntigravityGatewayService) writeGoogleError(c *gin.Context, status int, message string) error {
	MarkResponseCommitted(c)
	statusStr := "UNKNOWN"
	switch status {
	case 400:
		statusStr = "INVALID_ARGUMENT"
	case 404:
		statusStr = "NOT_FOUND"
	case 429:
		statusStr = "RESOURCE_EXHAUSTED"
	case 500:
		statusStr = "INTERNAL"
	case 502, 503:
		statusStr = "UNAVAILABLE"
	}

	c.JSON(status, gin.H{
		"error": gin.H{
			"code":    status,
			"message": message,
			"status":  statusStr,
		},
	})
	return fmt.Errorf("%s", message)
}

// collectClaudeStreamResponse 收集上游流式响应，转换为 Claude 非流式格式返回
// 用于处理客户端非流式请求但上游只支持流式的情况
func (s *AntigravityGatewayService) collectClaudeStreamResponse(c *gin.Context, resp *http.Response, startTime time.Time, originalModel string) ([]byte, *antigravityStreamResult, error) {
	if upstreamResponseModelObserverFromContext(c) == nil {
		beginUpstreamResponseModelObservation(c)
	}
	// Task24: A/B state for this request, nil while both effective switches are off.
	cont := antigravityThoughtContinuationFromGin(c)
	maxLineSize := defaultMaxLineSize
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}

	var firstTokenMs *int
	var last map[string]any
	var lastWithParts map[string]any
	var collectedParts []map[string]any // 收集所有 parts（包括 text、thinking、functionCall、inlineData 等）
	var meaningfulResponse bool
	// Task24 B：追加生成开始时第一代的响应体被移到快照里；只有追加生成真的产生了响应体时，
	// 最终身体才属于第二代，第一代的原始用量只能累计一次。
	var firstLast, firstLastWithParts map[string]any
	lastFromAppended, lastWithPartsFromAppended := false, false

	type scanEvent struct {
		line string
		err  error
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	// 独立 goroutine 读取上游，避免读取阻塞影响超时处理；每个 source 持有自己的 done，
	// 因此 Task24 的追加生成可以在读完第一代之后换到第二代。
	startScanner := func(body io.Reader) (<-chan scanEvent, func()) {
		scanner := bufio.NewScanner(body)
		scanBuf := getSSEScannerBuf64K()
		scanner.Buffer(scanBuf[:0], maxLineSize)
		events := make(chan scanEvent, 16)
		done := make(chan struct{})
		sendEvent := func(ev scanEvent) bool {
			select {
			case events <- ev:
				return true
			case <-done:
				return false
			}
		}
		go func() {
			defer putSSEScannerBuf64K(scanBuf)
			defer close(events)
			for scanner.Scan() {
				atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
				if !sendEvent(scanEvent{line: scanner.Text()}) {
					return
				}
			}
			if err := scanner.Err(); err != nil {
				_ = sendEvent(scanEvent{err: err})
			}
		}()
		return events, func() { close(done) }
	}
	events, stopScanner := startScanner(resp.Body)
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

	// 上游数据间隔超时保护（防止上游挂起长期占用连接）
	streamInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Task24 B：第一代只有思考且正常完成时，追加一次生成（复用原样上游信封
				// 加实际收到的思考/签名）。追加失败不再变成硬错误：下游拿到第一代的
				// 合法响应，终态由 A 的有效设置决定，usage 累计入账。
				if cont != nil && cont.canContinue() {
					nextResp, _, continueErr := cont.continueOnce(c.Request.Context(), resp)
					if continueErr != nil {
						cont.recordFailure(c, "claude_collect", continueErr)
					} else if nextResp != nil {
						// 新的一代开始：第一代的响应体放入快照并清空当前代的收集状态，
						// 避免只有第一代有内容时它的响应体被累计两次。
						firstLast, firstLastWithParts = last, lastWithParts
						last, lastWithParts = nil, nil
						extraResp = nextResp
						scannerStops = append(scannerStops, stopScanner)
						events, stopScanner = startScanner(nextResp.Body)
						continue
					}
				}
				// 流结束，转换并返回响应
				goto returnResponse
			}
			if ev.err != nil {
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (antigravity claude non-stream): max_size=%d error=%v", maxLineSize, ev.err)
				}
				if cont != nil && cont.hasExtraGeneration() {
					// 追加生成的读取失败照常返回第一代的合法响应，usage 累计入账。
					cont.recordFailure(c, "claude_collect_read", ev.err)
					goto returnResponse
				}
				cont.markReadFailure()
				return nil, nil, ev.err
			}

			line := ev.line
			trimmed := strings.TrimRight(line, "\r\n")

			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}

			// 解包 v1internal 响应
			inner, parseErr := s.unwrapV1InternalResponse([]byte(payload))
			if parseErr != nil {
				continue
			}
			upstreamResponseModelObserverFromContext(c).ObserveGemini(inner)
			if cont != nil {
				cont.observePayload(inner)
			}

			var parsed map[string]any
			if err := json.Unmarshal(inner, &parsed); err != nil {
				continue
			}

			last = parsed
			lastFromAppended = cont != nil && cont.hasExtraGeneration()

			// 保留最后一个有 parts 的响应，并收集所有 parts
			parts := extractGeminiParts(parsed)
			if len(parts) > 0 {
				lastWithParts = parsed
				lastWithPartsFromAppended = cont != nil && cont.hasExtraGeneration()

				// 收集所有 parts（text、thinking、functionCall、inlineData 等）
				collectedParts = append(collectedParts, parts...)
			}
			if len(parts) > 0 || strings.TrimSpace(extractGeminiFinishReason(parsed)) != "" {
				meaningfulResponse = true
				if firstTokenMs == nil {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (antigravity claude non-stream)")
			return nil, nil, fmt.Errorf("stream data interval timeout")
		}
	}

returnResponse:
	// 追加生成没有产生任何可解析数据时，下游身体仍是第一代的合法响应；它已经在累计基数
	// 里，所以不能再把第一代的原始用量叠加一次。
	if cont != nil && cont.hasExtraGeneration() && last == nil && lastWithParts == nil {
		last, lastWithParts = firstLast, firstLastWithParts
	}

	// 处理空响应情况 — 触发同账号重试 + failover 切换账号
	if !meaningfulResponse && !cont.completeNoUsableAnswer(c, "claude_collect") {
		logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-Forward] warning: empty stream response (claude non-stream), triggering failover")
		return nil, nil, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
			RetryableOnSameAccount: true,
		}
	}

	// 选择最后一个有效响应
	finalResponse := pickGeminiCollectResult(last, lastWithParts)
	cont.reconcileCollectedTerminal(finalResponse)

	// 将收集的所有 parts 合并到最终响应中
	if len(collectedParts) > 0 {
		finalResponse = mergeCollectedPartsToResponse(finalResponse, collectedParts)
	}

	// Task24: 只有最终响应体真的来自追加生成时，下游与计费才能看到两代上游用量的累计值；
	// 身体还是第一代时叠加会把第一代算两次。
	if cont != nil && cont.hasExtraGeneration() {
		bodyFromAppendedGeneration := false
		if lastWithParts != nil {
			bodyFromAppendedGeneration = lastWithPartsFromAppended
		} else if last != nil {
			bodyFromAppendedGeneration = lastFromAppended
		}
		if bodyFromAppendedGeneration {
			antigravityAddUsageBaselineToResponse(finalResponse, cont.baseRawUsage)
		}
	}

	// 序列化为 JSON（Gemini 格式）
	geminiBody, err := json.Marshal(finalResponse)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal gemini response: %w", err)
	}

	// 转换 Gemini 响应为 Claude 格式
	claudeResp, agUsage, err := antigravity.TransformGeminiToClaude(geminiBody, originalModel)
	if err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-Forward] transform_error error=%v body=%s", err, string(geminiBody))
		return nil, nil, fmt.Errorf("failed to parse upstream response: %w", err)
	}

	// Task24 A：只思考且上游正常完成时，抑制成功终态；思考、签名与 usage 保留。
	if cont != nil && cont.suppressFinalTerminal() {
		claudeResp, err = withClaudeSuccessfulTerminalSuppressed(claudeResp)
		if err != nil {
			return nil, nil, err
		}
	}

	if cont.completeNoUsableAnswer(c, "claude_collect") {
		claudeResp, err = sjson.SetBytes(claudeResp, "type", "error")
		if err == nil {
			claudeResp, err = sjson.SetBytes(claudeResp, "error", cont.responseFailure().protocolError("messages"))
		}
		if err != nil {
			return nil, nil, err
		}
	}

	// 转换为 service.ClaudeUsage
	usage := &ClaudeUsage{
		InputTokens:              agUsage.InputTokens,
		OutputTokens:             agUsage.OutputTokens,
		CacheCreationInputTokens: agUsage.CacheCreationInputTokens,
		CacheReadInputTokens:     agUsage.CacheReadInputTokens,
		ImageOutputTokens:        agUsage.ImageOutputTokens,
	}

	return claudeResp, &antigravityStreamResult{usage: usage, firstTokenMs: firstTokenMs}, cont.failureError()
}

// withClaudeSuccessfulTerminalSuppressed replaces the successful stop reason of a
// non-streaming Anthropic body with null, keeping the content, signatures and usage. The
// caller keeps the request's effective A/B policy, so the terminal is decided in one place.
func withClaudeSuccessfulTerminalSuppressed(claudeBody []byte) ([]byte, error) {
	updated, err := sjson.SetBytes(claudeBody, "stop_reason", nil)
	if err != nil {
		return claudeBody, err
	}
	return updated, nil
}

// handleClaudeStreamToNonStreaming 收集上游流式响应，转换为 Claude 非流式格式返回
// 用于处理客户端非流式请求但上游只支持流式的情况
func (s *AntigravityGatewayService) handleClaudeStreamToNonStreaming(c *gin.Context, resp *http.Response, startTime time.Time, originalModel string) (*antigravityStreamResult, error) {
	claudeResp, streamRes, err := s.collectClaudeStreamResponse(c, resp, startTime, originalModel)
	if err != nil {
		if IsAntigravityPartialResponseError(err) {
			// The protocol failure is already encoded; preserve partial content and billable usage.
			if claudeResp != nil {
				status := http.StatusOK
				if antigravityThoughtContinuationFromGin(c).responseFailure() != nil {
					status = antigravityThoughtContinuationFromGin(c).responseFailure().status()
				}
				writeClaudeResponse(c, claudeResp, status)
			}
			return streamRes, err
		}
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			return nil, err
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}

		errMsg := "Failed to parse upstream response"
		errType := "upstream_error"
		if strings.Contains(err.Error(), "stream data interval timeout") {
			errMsg = "Upstream stream data interval timeout"
			errType = "upstream_timeout"
		} else if errors.Is(err, bufio.ErrTooLong) {
			errMsg = "Upstream response line too long"
			errType = "response_too_large"
		}

		return nil, s.writeClaudeError(c, http.StatusBadGateway, errType, errMsg)
	}
	writeClaudeNonStreamingResponse(c, claudeResp)
	return streamRes, nil
}

// handleClaudeStreamingResponse 处理 Claude 流式响应（Gemini SSE → Claude SSE 转换）
func (s *AntigravityGatewayService) handleClaudeStreamingResponse(c *gin.Context, resp *http.Response, startTime time.Time, originalModel string) (*antigravityStreamResult, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	processor := antigravity.NewStreamingProcessor(originalModel)
	// Task24: A/B state for this request. It is nil while both effective switches are
	// off, in which case this loop keeps its exact pre-Task24 behavior and cost.
	cont := antigravityThoughtContinuationFromGin(c)
	var firstTokenMs *int
	// 使用 Scanner 并限制单行大小，避免 ReadString 无上限导致 OOM
	maxLineSize := defaultMaxLineSize
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.settingService.cfg.Gateway.MaxLineSize
	}

	// 辅助函数：转换 antigravity.ClaudeUsage 到 service.ClaudeUsage
	convertUsage := func(agUsage *antigravity.ClaudeUsage) *ClaudeUsage {
		if agUsage == nil {
			return &ClaudeUsage{}
		}
		return &ClaudeUsage{
			InputTokens:              agUsage.InputTokens,
			OutputTokens:             agUsage.OutputTokens,
			CacheCreationInputTokens: agUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     agUsage.CacheReadInputTokens,
		}
	}

	type scanEvent struct {
		line string
		err  error
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	// 独立 goroutine 读取上游，避免读取阻塞影响超时处理。
	// 每个 source 持有自己的 done，所以 stop 只能调用一次；Task24 的追加生成会为
	// 第二个上游响应再起一个 source，并在函数退出时统一收尾。
	startScanner := func(body io.Reader) (<-chan scanEvent, func()) {
		scanner := bufio.NewScanner(body)
		scanBuf := getSSEScannerBuf64K()
		scanner.Buffer(scanBuf[:0], maxLineSize)
		events := make(chan scanEvent, 16)
		done := make(chan struct{})
		sendEvent := func(ev scanEvent) bool {
			select {
			case events <- ev:
				return true
			case <-done:
				return false
			}
		}
		go func() {
			defer putSSEScannerBuf64K(scanBuf)
			defer close(events)
			for scanner.Scan() {
				atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
				if !sendEvent(scanEvent{line: scanner.Text()}) {
					return
				}
			}
			if err := scanner.Err(); err != nil {
				_ = sendEvent(scanEvent{err: err})
			}
		}()
		return events, func() { close(done) }
	}
	events, stopScanner := startScanner(resp.Body)
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

	streamInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.settingService.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	// 下游 keepalive：防止代理/Cloudflare Tunnel 因连接空闲而断开
	keepaliveInterval := time.Duration(0)
	if s.settingService.cfg != nil && s.settingService.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.settingService.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	lastDataAt := time.Now()

	cw := newAntigravityClientWriter(c.Writer, flusher, "antigravity claude")
	if capture := GeminiCaptureFromContext(c.Request.Context()); capture != nil {
		cw.onWrite = capture.RecordConvertedWrite
		cw.onDisconnect = capture.MarkClientDisconnect
	}

	// 仅发送一次错误事件，避免多次写入导致协议混乱
	errorEventSent := false
	sendErrorEvent := func(reason string) {
		if errorEventSent || cw.Disconnected() {
			return
		}
		errorEventSent = true
		cw.Fprintf("event: error\ndata: {\"error\":\"%s\"}\n\n", reason)
	}

	// finalizeFailure 结束一条「已收到的思考 + 累计 usage + 协议错误」的响应：追加生成
	// 失败绝不能伪装成第一代的成功，但已产生的计费结果必须保留（返回 nil Go error，
	// 调用方照常入账，也不会触发 failover/新请求）。
	finalizeFailure := func(contErr error) (*antigravityStreamResult, error) {
		processor.SetWithholdSuccessfulTerminal(true)
		processor.SetUsageOnlyTerminal(true)
		finalEvents, agUsage := processor.Finish()
		if len(finalEvents) > 0 {
			cw.Write(finalEvents)
		}
		if !errors.Is(contErr, context.Canceled) && !errors.Is(contErr, context.DeadlineExceeded) {
			sendErrorEvent("thought_continuation_error")
		}
		// 终态错误与第一代的可计费结果一起返回：调用方先入账再返回错误，且不做 failover、
		// 不再写第二个错误响应（协议错误已经发出）。
		return &antigravityStreamResult{usage: convertUsage(agUsage), firstTokenMs: firstTokenMs, clientDisconnect: cw.Disconnected()}, cont.failureError()
	}

	// finishUsage 是获取 processor 最终 usage 的辅助函数
	finishUsage := func() *ClaudeUsage {
		_, agUsage := processor.Finish()
		return convertUsage(agUsage)
	}

	// finalize 发送结束事件并返回结果。
	finalize := func() (*antigravityStreamResult, error) {
		noAnswer := !cw.Disconnected() && cont.completeNoUsableAnswer(c, "claude_stream")
		if cont != nil {
			// A 的有效设置（或追加生成失败）决定最终终态：抑制成功终态，但仍下发累计
			// usage，让下游拿到真实用量而不是被当成成功完成。
			processor.SetWithholdSuccessfulTerminal(cont.suppressFinalTerminal())
			processor.SetUsageOnlyTerminal(cont.suppressFinalTerminal())
		}
		finalEvents, agUsage := processor.Finish()
		if len(finalEvents) > 0 {
			cw.Write(finalEvents)
		} else if !noAnswer && !processor.MessageStartSent() && !cw.Disconnected() {
			// 整个流未收到任何可解析的上游数据（全部 SSE 行均无法被 JSON 解析），
			// 触发 failover 在同账号重试，避免向客户端发出缺少 message_start 的残缺流
			logger.LegacyPrintf("service.antigravity_gateway", "[antigravity-Claude-Stream] empty stream response (no valid events parsed), triggering failover")
			return nil, &UpstreamFailoverError{
				StatusCode:             http.StatusBadGateway,
				ResponseBody:           []byte(`{"error":"empty stream response from upstream"}`),
				RetryableOnSameAccount: true,
			}
		}
		if noAnswer {
			writeAntigravityNoUsableAnswerAnthropic(cw, cont.responseFailure())
		}
		return &antigravityStreamResult{usage: convertUsage(agUsage), firstTokenMs: firstTokenMs, clientDisconnect: cw.Disconnected()}, cont.failureError()
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Task24 B：上游正常完成、且本轮只有思考时，追加一次生成。请求复用原样
				// 上游信封并接上实际收到的思考/签名；只追加一次，不重入重试循环，也不
				// 重开调度/粘性身份/并发槽位。
				if cont != nil && cont.canContinue() && !cw.Disconnected() {
					nextResp, finishedUsage, continueErr := cont.continueOnce(c.Request.Context(), resp)
					if continueErr != nil {
						cont.recordFailure(c, "claude_stream", continueErr)
						return finalizeFailure(continueErr)
					}
					if nextResp != nil {
						processor.AddUsageBaseline(toAntigravityUsage(finishedUsage))
						processor.ResetGenerationUsage()
						extraResp = nextResp
						scannerStops = append(scannerStops, stopScanner)
						events, stopScanner = startScanner(nextResp.Body)
						continue
					}
				}
				// 上游完成，发送结束事件
				return finalize()
			}
			if ev.err != nil {
				if cont != nil && cont.hasExtraGeneration() {
					// 追加生成的读取失败：按缴协议错误结束（不发成功终态），保留已计量的
					// usage 与已收到的思考。
					cont.recordFailure(c, "claude_stream_read", ev.err)
					return finalizeFailure(ev.err)
				}
				if disconnect, handled := handleStreamReadError(ev.err, cw.Disconnected(), "antigravity claude"); handled {
					return &antigravityStreamResult{usage: finishUsage(), firstTokenMs: firstTokenMs, clientDisconnect: disconnect}, nil
				}
				cont.markReadFailure()
				if errors.Is(ev.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (antigravity): max_size=%d error=%v", maxLineSize, ev.err)
					sendErrorEvent("response_too_large")
					return &antigravityStreamResult{usage: convertUsage(nil), firstTokenMs: firstTokenMs}, ev.err
				}
				sendErrorEvent("stream_read_error")
				return nil, fmt.Errorf("stream read error: %w", ev.err)
			}

			lastDataAt = time.Now()
			s.observeAntigravityGeminiSSELine(c, ev.line)
			if cont != nil {
				// 观察这一代上游响应，并决定成功终态是否允许发往下游。追加生成之前的
				// 暂扣不发 usage delta，那条 delta 只在响应真正结束时发出。
				cont.observeLine(ev.line)
				processor.SetWithholdSuccessfulTerminal(cont.withholdSuccessfulTerminal())
				processor.SetUsageOnlyTerminal(false)
			}

			// 处理 SSE 行，转换为 Claude 格式
			claudeEvents := processor.ProcessLine(strings.TrimRight(ev.line, "\r\n"))
			if len(claudeEvents) > 0 {
				if firstTokenMs == nil {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
				cw.Write(claudeEvents)
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if cw.Disconnected() {
				logger.LegacyPrintf("service.antigravity_gateway", "Upstream timeout after client disconnect (antigravity claude), returning collected usage")
				return &antigravityStreamResult{usage: finishUsage(), firstTokenMs: firstTokenMs, clientDisconnect: true}, nil
			}
			logger.LegacyPrintf("service.antigravity_gateway", "Stream data interval timeout (antigravity)")
			if capture := GeminiCaptureFromContext(c.Request.Context()); capture != nil {
				capture.MarkTimeout()
			}
			sendErrorEvent("stream_timeout")
			return &antigravityStreamResult{usage: convertUsage(nil), firstTokenMs: firstTokenMs}, fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if cw.Disconnected() {
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// SSE ping 事件：Anthropic 原生格式，客户端会正确处理，
			// 同时保持连接活跃防止 Cloudflare Tunnel 等代理断开
			if !cw.Fprintf("event: ping\ndata: {\"type\": \"ping\"}\n\n") {
				logger.LegacyPrintf("service.antigravity_gateway", "Client disconnected during keepalive ping (antigravity claude), continuing to drain upstream for billing")
				continue
			}
		}
	}
}

func (s *AntigravityGatewayService) extractImageInputSize(body []byte) string {
	var req antigravity.GeminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}

	if req.GenerationConfig != nil && req.GenerationConfig.ImageConfig != nil {
		return strings.TrimSpace(req.GenerationConfig.ImageConfig.ImageSize)
	}

	return ""
}

// isImageGenerationModel 判断模型是否为图片生成模型
// 支持的模型：gemini-3.1-flash-image, gemini-3-pro-image, gemini-2.5-flash-image 等
func isImageGenerationModel(model string) bool {
	modelLower := strings.ToLower(model)
	// 移除 models/ 前缀
	modelLower = strings.TrimPrefix(modelLower, "models/")

	// 精确匹配或前缀匹配
	return modelLower == "gemini-3.1-flash-image" ||
		modelLower == "gemini-3.1-flash-image-preview" ||
		strings.HasPrefix(modelLower, "gemini-3.1-flash-image-") ||
		modelLower == "gemini-3-pro-image" ||
		modelLower == "gemini-3-pro-image-preview" ||
		strings.HasPrefix(modelLower, "gemini-3-pro-image-") ||
		modelLower == "gemini-2.5-flash-image" ||
		modelLower == "gemini-2.5-flash-image-preview" ||
		strings.HasPrefix(modelLower, "gemini-2.5-flash-image-")
}
