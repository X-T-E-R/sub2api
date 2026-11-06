package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type streamPreheaderProtocol uint8

const (
	streamPreheaderNone streamPreheaderProtocol = iota
	streamPreheaderMessages
	streamPreheaderGemini
	streamPreheaderChat
	streamPreheaderResponses
)

const streamPreheaderTerminalCode = "gateway_stream_terminal_error"

var errStreamPreheaderTerminal = errors.New("gateway stream terminated after preheader heartbeat")

type preheaderTransport func(*http.Request) (*http.Response, error)

const streamPreheaderPolicyKey = "gateway_stream_preheader_policy"

type streamPreheaderPolicy struct {
	protocol streamPreheaderProtocol
	interval time.Duration
}

// ConfigureStreamPreheader records the authenticated inbound streaming intent.
// This does not start a timer or write a response. Only an actual inference
// transport call, after request construction and business admission, can do so.
func ConfigureStreamPreheader(c *gin.Context, cfg *config.Config, protocol string, stream bool) {
	interval := streamPreheaderInterval(cfg, stream)
	if interval <= 0 {
		return
	}
	var wire streamPreheaderProtocol
	switch protocol {
	case "messages":
		wire = streamPreheaderMessages
	case "gemini":
		wire = streamPreheaderGemini
	case "chat":
		wire = streamPreheaderChat
	case "responses":
		wire = streamPreheaderResponses
	default:
		return
	}
	c.Set(streamPreheaderPolicyKey, streamPreheaderPolicy{protocol: wire, interval: interval})
}

func StreamPreheaderConfigured(c *gin.Context) bool {
	_, enabled := c.Get(streamPreheaderPolicyKey)
	return enabled
}

func requestStreamPreheader(c *gin.Context, req *http.Request, account *Account, send preheaderTransport, observe func(*http.Response, []byte)) (*http.Response, error, bool) {
	if c != nil {
		if policy, enabled := c.Get(streamPreheaderPolicyKey); enabled {
			p := policy.(streamPreheaderPolicy)
			return roundTripStreamPreheader(c, req, account, p.protocol, p.interval, send, observe)
		}
	}
	resp, err := send(req)
	return resp, err, false
}

func streamPreheaderInterval(cfg *config.Config, stream bool) time.Duration {
	if !stream || cfg == nil || !cfg.Gateway.StreamPreheaderKeepaliveEnabled {
		return 0
	}
	return time.Duration(cfg.Gateway.StreamKeepaliveInterval) * time.Second
}

// waitForStreamHeaders transfers only the upstream transport call to a goroutine.
// The request goroutine retains exclusive ownership of Gin and stops its timer
// before handing the response to the existing body reader. Client cancellation
// is linked only until headers, including for transports that detach the body
// lifetime so it can still collect upstream usage after a downstream disconnect.
func waitForStreamHeaders(c *gin.Context, req *http.Request, protocol streamPreheaderProtocol, interval time.Duration, send preheaderTransport) (*http.Response, error, bool) {
	if protocol == streamPreheaderNone || interval <= 0 || c.Writer.Written() {
		resp, err := send(req)
		return resp, err, false
	}
	client := c.Request.Context()
	if err := client.Err(); err != nil {
		return nil, err, false
	}
	ctx, cancel := context.WithCancel(req.Context())
	type result struct {
		resp *http.Response
		err  error
	}
	results := make(chan result)
	go func() {
		resp, err := send(req.WithContext(ctx))
		select {
		case results <- result{resp, err}:
		case <-ctx.Done():
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
	}()

	started := false
	finish := func(r result) (*http.Response, error, bool) {
		if err := client.Err(); err != nil {
			cancel()
			if r.resp != nil && r.resp.Body != nil {
				_ = r.resp.Body.Close()
			}
			return nil, err, started
		}
		if r.resp != nil && r.resp.Body != nil {
			r.resp.Body = &streamPreheaderBody{ReadCloser: r.resp.Body, cancel: cancel}
		} else {
			cancel()
		}
		return r.resp, r.err, started
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case r := <-results:
			return finish(r)
		case <-client.Done():
			cancel()
			return nil, client.Err(), started
		case <-ctx.Done():
			cancel()
			return nil, ctx.Err(), started
		case <-timer.C:
			// Prefer an already available response over committing SSE 200.
			select {
			case r := <-results:
				return finish(r)
			default:
			}
			if err := client.Err(); err != nil {
				cancel()
				return nil, err, started
			}
			if !started {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("X-Accel-Buffering", "no")
				c.Status(http.StatusOK)
				started = true
				c.Set(streamPreheaderCommittedKey, protocol)
			}
			if _, err := c.Writer.WriteString(streamPreheaderPing(protocol)); err != nil {
				cancel()
				return nil, err, started
			}
			c.Writer.Flush()
			timer.Reset(interval)
		}
	}
}

func streamPreheaderPing(protocol streamPreheaderProtocol) string {
	if protocol == streamPreheaderMessages {
		return "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	}
	return ": keepalive\n\n"
}

type streamPreheaderBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamPreheaderBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

const streamPreheaderCommittedKey = "gateway_stream_preheader_committed"

// StreamPreheaderCommitted distinguishes an opt-in heartbeat from other
// writers' pings. It also bars new account/model attempts after a terminal.
func StreamPreheaderCommitted(c *gin.Context) bool {
	_, committed := c.Get(streamPreheaderCommittedKey)
	return committed
}

// Existing body heartbeat owners adopt the deployment's terminal/no-replay
// policy when they commit SSE before any semantic model output.
func markStreamPreheaderBodyHeartbeat(c *gin.Context) {
	if policy, enabled := c.Get(streamPreheaderPolicyKey); enabled {
		c.Set(streamPreheaderCommittedKey, policy.(streamPreheaderPolicy).protocol)
	}
}

const streamPreheaderFinalizedKey = "gateway_stream_preheader_finalized"

// StreamPreheaderFinalized tells callers the transport already handled the
// terminal response (or downstream cancellation) and must not be retried.
func StreamPreheaderFinalized(c *gin.Context) bool {
	_, finalized := c.Get(streamPreheaderFinalizedKey)
	return finalized
}

// FinishStreamPreheaderError is the fallback for failures after a heartbeat
// when the transport or body handler has not already emitted a terminal event.
func FinishStreamPreheaderError(c *gin.Context, status int, errType, message string, headers http.Header) bool {
	if StreamPreheaderFinalized(c) {
		return true
	}
	if !StreamPreheaderCommitted(c) {
		return false
	}
	_ = writeStreamPreheaderError(c, status, errType, message, headers)
	return true
}

func FinishStreamPreheaderFailover(c *gin.Context, failure *UpstreamFailoverError) bool {
	if !StreamPreheaderCommitted(c) {
		return false
	}
	status := failure.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(failure.ClientMessage))
	if message == "" {
		message = http.StatusText(status)
	}
	return FinishStreamPreheaderError(c, status, streamPreheaderErrorType(status, failure.ResponseBody), message, failure.ResponseHeaders)
}

type streamPreheaderResponseMeta struct{ id, model string }

func configuredStreamFailureFrame(c *gin.Context, status int, errType, message string, response ...streamPreheaderResponseMeta) string {
	policy, _ := c.Get(streamPreheaderPolicyKey)
	return recordStreamPreheaderFailure(c, policy.(streamPreheaderPolicy).protocol, status, errType, message, "", response...)
}

func writeConfiguredStreamFailure(c *gin.Context, status int, errType, message string, response ...streamPreheaderResponseMeta) error {
	if !c.Writer.Written() {
		c.Set(streamPreheaderFinalizedKey, true)
		MarkResponseCommitted(c)
		if err := c.Request.Context().Err(); err != nil {
			return err
		}
		policy, _ := c.Get(streamPreheaderPolicyKey)
		p := policy.(streamPreheaderPolicy)
		payload := gin.H{"error": gin.H{"type": errType, "message": message}}
		switch p.protocol {
		case streamPreheaderMessages:
			payload["type"] = "error"
		case streamPreheaderGemini:
			payload["error"] = gin.H{"code": status, "status": streamPreheaderGoogleStatus(status, errType), "message": message}
		}
		c.JSON(status, payload)
		return errStreamPreheaderTerminal
	}
	return writeStreamPreheaderFailureFrame(c, configuredStreamFailureFrame(c, status, errType, message, response...))
}

func recordStreamPreheaderFailure(c *gin.Context, protocol streamPreheaderProtocol, status int, errType, message, retryAfter string, response ...streamPreheaderResponseMeta) string {
	c.Set(streamPreheaderFinalizedKey, true)
	MarkResponseCommitted(c)
	if c.Request.Context().Err() != nil {
		return ""
	}
	MarkOpsStreamFailure(c, errType, streamPreheaderTerminalCode, message, status)
	return buildStreamPreheaderError(protocol, status, errType, message, retryAfter, time.Now(), response...)
}

func writeStreamPreheaderError(c *gin.Context, status int, errType, message string, headers http.Header) error {
	protocol, _ := c.Get(streamPreheaderCommittedKey)
	return writeStreamPreheaderFailureFrame(c, recordStreamPreheaderFailure(c, protocol.(streamPreheaderProtocol), status, errType, message, headers.Get("Retry-After")))
}

func writeStreamPreheaderFailureFrame(c *gin.Context, frame string) error {
	if err := c.Request.Context().Err(); err != nil {
		return err
	}
	if _, err := c.Writer.WriteString(frame); err != nil {
		return err
	}
	c.Writer.Flush()
	return errStreamPreheaderTerminal
}

// roundTripStreamPreheader never resends the request. A late HTTP failure is
// final: consume the bounded error body, retain account/error observations, and
// return before a caller's original HTTP-error retry or JSON response branches.
// A true third result tells the caller to return err immediately.
func roundTripStreamPreheader(c *gin.Context, req *http.Request, account *Account, protocol streamPreheaderProtocol, interval time.Duration, send preheaderTransport, observe func(*http.Response, []byte)) (*http.Response, error, bool) {
	resp, err, started := waitForStreamHeaders(c, req, protocol, interval, send)
	if protocol != streamPreheaderNone && interval > 0 && c.Request.Context().Err() != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		c.Set(streamPreheaderFinalizedKey, true)
		return nil, c.Request.Context().Err(), true
	}
	if !started {
		return resp, err, false
	}
	if err != nil || resp == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		setOpsUpstreamError(c, 0, "Upstream request failed", "")
		return nil, writeStreamPreheaderError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed", nil), true
	}
	if resp.StatusCode < 400 {
		return resp, nil, false
	}
	var body []byte
	if resp.Body != nil {
		body, err = readStreamPreheaderErrorBody(c, resp.Body, protocol, interval)
		_ = resp.Body.Close()
		if err != nil {
			c.Set(streamPreheaderFinalizedKey, true)
			return nil, err, true
		}
	}
	if observe != nil {
		observe(resp, body)
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	setOpsUpstreamError(c, resp.StatusCode, message, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
			UpstreamURL: safeUpstreamURL(req.URL.String()), Kind: "stream_preheader_http_error", Message: message,
		})
	}
	if message == "" || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		message = http.StatusText(resp.StatusCode)
		if message == "" {
			message = "Upstream request failed"
		}
	}
	errType := streamPreheaderErrorType(resp.StatusCode, body)
	if protocol == streamPreheaderGemini {
		errType = gjson.GetBytes(body, "error.status").String()
	}
	return nil, writeStreamPreheaderError(c, resp.StatusCode, errType, message, resp.Header), true
}

// A late rejection can flush its HTTP headers long before its JSON body. Keep
// the same downstream writer alive while collecting the bounded error detail;
// unlike a successful stream, cancellation must also stop this error-body read.
func readStreamPreheaderErrorBody(c *gin.Context, body io.Reader, protocol streamPreheaderProtocol, interval time.Duration) ([]byte, error) {
	results := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(body, gatewayUpstreamErrorBodyReadLimit))
		results <- data
	}()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case data := <-results:
			if err := c.Request.Context().Err(); err != nil {
				return nil, err
			}
			return data, nil
		case <-c.Request.Context().Done():
			return nil, c.Request.Context().Err()
		case <-timer.C:
			if err := c.Request.Context().Err(); err != nil {
				return nil, err
			}
			if _, err := c.Writer.WriteString(streamPreheaderPing(protocol)); err != nil {
				return nil, err
			}
			c.Writer.Flush()
			timer.Reset(interval)
		}
	}
}

func streamPreheaderErrorType(status int, body []byte) string {
	if value := gjson.GetBytes(body, "error.type"); value.Type == gjson.String && value.String() != "" {
		return value.String()
	}
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "upstream_error"
	}
}

func buildStreamPreheaderError(protocol streamPreheaderProtocol, status int, errType, message, retryAfter string, now time.Time, response ...streamPreheaderResponseMeta) string {
	retryAfter, retryDelay := streamPreheaderRetryAfter(retryAfter, now)
	failure := gin.H{"code": streamPreheaderTerminalCode, "type": errType, "message": message, "status": status}
	if retryAfter != "" {
		failure["retry_after"] = retryAfter
	}
	event := ""
	var payload gin.H
	switch protocol {
	case streamPreheaderMessages:
		event = "error"
		payload = gin.H{"type": event, "error": failure}
	case streamPreheaderGemini:
		info := gin.H{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"domain": "sub2api.gateway", "reason": "GATEWAY_STREAM_TERMINAL_ERROR",
		}
		if retryAfter != "" {
			info["metadata"] = gin.H{"retryAfter": retryAfter}
		}
		details := []gin.H{info}
		if retryDelay != "" {
			details = append(details, gin.H{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": retryDelay})
		}
		payload = gin.H{"error": gin.H{"code": status, "message": message, "status": streamPreheaderGoogleStatus(status, errType), "details": details}}
	case streamPreheaderChat:
		payload = gin.H{"error": failure}
	case streamPreheaderResponses:
		event = "response.failed"
		meta := streamPreheaderResponseMeta{}
		if len(response) > 0 {
			meta = response[0]
		}
		if meta.id == "" {
			meta.id = "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		result := gin.H{"id": meta.id, "object": "response", "created_at": now.Unix(), "status": "failed", "output": []any{}, "error": failure}
		if meta.model != "" {
			result["model"] = meta.model
		}
		payload = gin.H{"type": event, "response": result}
	}
	body, _ := json.Marshal(payload)
	if event != "" {
		return "event: " + event + "\ndata: " + string(body) + "\n\n"
	}
	return "data: " + string(body) + "\n\n"
}

func streamPreheaderRetryAfter(value string, now time.Time) (string, string) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds <= 315576000000 { // google.protobuf.Duration upper bound, in seconds.
			return value, strconv.FormatUint(seconds, 10) + "s"
		}
		return value, ""
	}
	if date, err := http.ParseTime(value); err == nil {
		seconds := date.Unix() - now.Unix()
		if seconds < 0 {
			seconds = 0
		}
		return value, strconv.FormatInt(seconds, 10) + "s"
	}
	return "", ""
}

func streamPreheaderGoogleStatus(status int, original string) string {
	switch original {
	case "INVALID_ARGUMENT", "UNAUTHENTICATED", "PERMISSION_DENIED", "NOT_FOUND", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DEADLINE_EXCEEDED", "CANCELLED", "UNKNOWN", "ALREADY_EXISTS", "DATA_LOSS":
		return original
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusInternalServerError:
		return "INTERNAL"
	case http.StatusBadGateway, http.StatusServiceUnavailable, 529:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		return "UNKNOWN"
	}
}
