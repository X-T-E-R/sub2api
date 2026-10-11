package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Task24 gateway runtime for the two Antigravity "thought-only terminal" switches.
//
// The settings pair is resolved once per request (see antigravity_thought_policy.go) and
// carried through the response path as one immutable value:
//
//   - SuppressThoughtOnlyStop (A): a response that only ever produced thinking and that
//     the upstream completed normally keeps its thinking, signature and usage, but the
//     protocol's successful completion signal is withheld, so the downstream client
//     applies its own continuation policy instead of accepting an empty answer.
//   - ContinueThoughtOnly (B): the gateway itself issues exactly one extra upstream
//     request. That request reuses the exact original upstream request envelope and
//     appends the thinking and signature the upstream actually returned as the final
//     model turn, with no added system/user text and no blank turn.
//
// Nothing here reacts to errors, cancellations, refusals, output-limit stops, empty
// responses or truncated streams: those keep their own semantics.

// antigravitySuccessfulFinishReason is the only upstream finish reason that counts as a
// normal completion. MAX_TOKENS, SAFETY, RECITATION, MALFORMED_FUNCTION_CALL, an absent
// reason (truncated stream) and every other value are never suppressed or continued.
const antigravitySuccessfulFinishReason = "STOP"

// errAntigravityThoughtContinuationUnavailable reports that the append-once budget is
// exhausted or the response is not a replayable thought-only completion.
var errAntigravityThoughtContinuationUnavailable = errors.New("antigravity thought continuation unavailable")

// antigravityThoughtContinuationStatusError reports a non-2xx response for the appended
// generation. Only the appended request fails; the first generation stays billable.
type antigravityThoughtContinuationStatusError struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

func (e *antigravityThoughtContinuationStatusError) Error() string {
	return fmt.Sprintf("thought continuation upstream status=%d", e.StatusCode)
}

// AntigravityThoughtContinuationError is the Task24 terminal error of a failed appended
// generation (B). The client already received everything the first generation produced, so:
//
//   - it is deliberately not an UpstreamFailoverError and carries no context cancellation
//     wrapper: exactly one upstream request was appended and a retry on another account or a
//     second attempt would duplicate content that is already downstream;
//   - the first generation is already billed upstream, so the partial ForwardResult it is
//     returned with stays billable and callers must record it before returning the error;
//   - the protocol-level failure was already written downstream (an SSE error frame or a
//     non-success terminal body), so callers must not write a second error response.
//
// The underlying cause is kept for logs and diagnostics only; Unwrap is deliberately not
// provided, so errors.Is(err, context.Canceled) and similar retry/cancel probes stay false and a
// consumer can only read this error as the terminal Task24 outcome.
type AntigravityThoughtContinuationError struct {
	Stage      string
	StatusCode int
	Err        error
}

func (e *AntigravityThoughtContinuationError) Error() string {
	if e == nil {
		return "antigravity thought continuation failed"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("antigravity thought continuation failed (stage=%s status=%d): %v", e.Stage, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("antigravity thought continuation failed (stage=%s): %v", e.Stage, e.Err)
}

// IsAntigravityThoughtContinuationError reports whether err is the Task24 terminal error of a
// failed appended generation. Callers bill the partial ForwardResult it comes with, return it
// without failover and without writing another error response.
func IsAntigravityThoughtContinuationError(err error) bool {
	var target *AntigravityThoughtContinuationError
	return errors.As(err, &target)
}

// antigravityThoughtTailPart is one received thinking part or signature carrier, kept
// exactly as the upstream sent it. raw is the part as it arrived, so the replayed tail keeps
// the carrier's original shape (an absent text field is not turned into an empty one);
// hasText and the decoded fields describe the same part for the eligibility checks.
type antigravityThoughtTailPart struct {
	text      string
	hasText   bool
	thought   bool
	signature string
	raw       json.RawMessage
}

// rebuildWithoutRaw returns the JSON of a part that has no recorded raw form, keeping the
// received shape: a part that arrived without a text field is not given an empty one.
func (p antigravityThoughtTailPart) rebuildWithoutRaw() []byte {
	entry := map[string]any{}
	if p.hasText || p.text != "" {
		entry["text"] = p.text
	}
	if p.thought {
		entry["thought"] = true
	}
	if p.signature != "" {
		entry["thoughtSignature"] = p.signature
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// antigravityThoughtGeneration observes one upstream generation: its terminal signal, the
// thinking it produced, whether it produced visible content or tool calls, and its usage.
type antigravityThoughtGeneration struct {
	sawResponse  bool
	finishReason string

	// tail holds every received thinking part and signature carrier in the original order,
	// verbatim: a signature is never moved onto another part, a carrier is never merged into
	// the thinking before it, and the gateway never invents a thought or a signature.
	tail             []antigravityThoughtTailPart
	hasVisibleText   bool
	hasToolCall      bool
	hasGrounding     bool
	claudeUsage      ClaudeUsage
	rawUsageMetadata map[string]int64
}

func newAntigravityThoughtGeneration() *antigravityThoughtGeneration {
	return &antigravityThoughtGeneration{}
}

// observeLine reads one upstream SSE line ("data: {...}").
func (g *antigravityThoughtGeneration) observeLine(line string) {
	if g == nil {
		return
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	g.observePayload([]byte(payload))
}

// observePayload reads one upstream response object. It accepts both the v1internal
// wrapper and a direct Gemini response.
func (g *antigravityThoughtGeneration) observePayload(payload []byte) {
	if g == nil || len(payload) == 0 {
		return
	}
	parsed := gjson.ParseBytes(payload)
	if inner := parsed.Get("response"); inner.Exists() && inner.IsObject() {
		parsed = inner
	}
	candidates := parsed.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		g.observeUsageMetadata(parsed)
		return
	}

	candidate := candidates.Array()[0]
	g.sawResponse = true
	if reason := candidate.Get("finishReason"); reason.Type == gjson.String && reason.String() != "" {
		g.finishReason = reason.String()
	}
	for _, part := range candidate.Get("content.parts").Array() {
		g.observePart(part)
	}
	g.observeGrounding(candidate.Get("groundingMetadata"))
	g.observeUsageMetadata(parsed)
}

func (g *antigravityThoughtGeneration) observePart(part gjson.Result) {
	if !part.IsObject() {
		return
	}
	if part.Get("functionCall").Exists() {
		g.hasToolCall = true
		return
	}
	if data := part.Get("inlineData.data"); data.Type == gjson.String && data.String() != "" {
		// Rendered downstream as a markdown image: that is visible content.
		g.hasVisibleText = true
		return
	}
	textField := part.Get("text")
	signature := part.Get("thoughtSignature").String()
	thought := part.Get("thought").Bool()
	if thought {
		// Every received thinking part is kept as it came, with its own signature carrier.
		g.tail = append(g.tail, antigravityThoughtTailPart{
			text:      textField.String(),
			hasText:   textField.Exists(),
			thought:   true,
			signature: signature,
			raw:       json.RawMessage(part.Raw),
		})
		return
	}
	if textField.String() != "" {
		g.hasVisibleText = true
		return
	}
	if signature == "" {
		return
	}
	// A non-thought part with empty text and a signature is a signature carrier: keep it
	// verbatim and in place instead of moving the signature onto another part.
	g.tail = append(g.tail, antigravityThoughtTailPart{
		text:      textField.String(),
		hasText:   textField.Exists(),
		signature: signature,
		raw:       json.RawMessage(part.Raw),
	})
}

func (g *antigravityThoughtGeneration) observeGrounding(grounding gjson.Result) {
	if !grounding.IsObject() {
		return
	}
	if len(grounding.Get("webSearchQueries").Array()) > 0 || len(grounding.Get("groundingChunks").Array()) > 0 {
		g.hasGrounding = true
	}
}

func (g *antigravityThoughtGeneration) observeUsageMetadata(parsed gjson.Result) {
	usage := parsed.Get("usageMetadata")
	if !usage.IsObject() {
		return
	}
	// extractGeminiUsage reads the usageMetadata of the response object it is given.
	if extracted := extractGeminiUsage([]byte(parsed.Raw)); extracted != nil {
		g.claudeUsage = *extracted
	}
	raw := map[string]int64{}
	usage.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.Number {
			raw[key.String()] = value.Int()
		}
		return true
	})
	g.rawUsageMetadata = raw
}

// thoughtOnlySuccessful reports the Task24 target shape: the upstream completed the
// response normally and everything it produced is thinking.
func (g *antigravityThoughtGeneration) thoughtOnlySuccessful() bool {
	if g == nil || !g.sawResponse || g.finishReason != antigravitySuccessfulFinishReason {
		return false
	}
	if g.hasVisibleText || g.hasToolCall || g.hasGrounding {
		return false
	}
	return len(g.replayableTail()) > 0
}

// replayableTail returns the model tail for the appended generation, or nil when the
// response carried no actual thinking. The received thinking and signature carriers are
// replayed in their original order; nothing is invented, merged or dropped.
func (g *antigravityThoughtGeneration) replayableTail() []antigravityThoughtTailPart {
	if g == nil || len(g.tail) == 0 {
		return nil
	}
	hasText := false
	for _, entry := range g.tail {
		if strings.TrimSpace(entry.text) != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return nil
	}
	return append([]antigravityThoughtTailPart(nil), g.tail...)
}

// antigravityThoughtContinuation is the per-request Task24 state. It is created once per
// forwarded request and stored on the Gin request context, so every retry inside the
// response path sees one policy pair and one append budget.
type antigravityThoughtContinuation struct {
	service *AntigravityGatewayService
	account *Account
	policy  AntigravityThoughtPolicy
	prefix  string

	proxyURL    string
	accessToken string
	action      string
	baseURL     string
	// body is the exact upstream request envelope of the generation in flight. The
	// appended generation only adds the received model tail to it.
	body []byte

	generation *antigravityThoughtGeneration
	// baseUsage accumulates the usage of every finished generation so the response the
	// client sees stays cumulative with what the upstream actually billed.
	// requestedModel is the client-facing model of this request, used for the normal
	// same-account error bookkeeping when the appended generation fails.
	requestedModel string
	baseUsage      ClaudeUsage
	baseRawUsage   map[string]int64
	extraUsed      bool
	// continuationFailed records that the appended generation did not complete. The
	// response must then end without a successful completion signal, whatever A is set to:
	// a failed second generation may never look like a first-generation success.
	continuationFailed bool
	// terminalErr is the Task24 error the public entry points return together with the
	// billable first-generation result once the appended generation failed.
	terminalErr *AntigravityThoughtContinuationError
	// extraObserved is true once the appended generation's response was obtained, so the
	// response under construction already contains parts of two generations.
	extraObserved bool
	// extraPayload is true once the appended generation produced parseable upstream data. When
	// it stays false, the downstream body is still the first generation's response.
	extraPayload bool
	// finishedGeneration keeps the first generation once the append budget is consumed, so the
	// terminal decision can still describe the body that is actually sent when the appended
	// generation produced nothing.
	finishedGeneration *antigravityThoughtGeneration
}

// hasExtraGeneration reports whether the body being assembled already contains the
// appended generation, which is exactly when the finished generation's usage is added.
func (c *antigravityThoughtContinuation) hasExtraGeneration() bool {
	return c != nil && c.extraObserved
}

// toAntigravityUsage converts the gateway usage record into the pkg response type the
// shared streaming processor reports.
func toAntigravityUsage(usage ClaudeUsage) antigravity.ClaudeUsage {
	return antigravity.ClaudeUsage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		ImageOutputTokens:        usage.ImageOutputTokens,
	}
}

// newAntigravityThoughtContinuation resolves the effective policy for this account and
// returns the runtime state, or nil when both switches are off (the source default), so
// the legacy response path keeps its exact behavior and cost.
func (s *AntigravityGatewayService) newAntigravityThoughtContinuation(
	ctx context.Context,
	account *Account,
	requestedModel, prefix, proxyURL, accessToken, action string,
	body []byte,
) *antigravityThoughtContinuation {
	if s == nil || account == nil || len(body) == 0 {
		return nil
	}
	policy := ResolveAntigravityThoughtPolicy(ctx, account)
	if !policy.SuppressThoughtOnlyStop && !policy.ContinueThoughtOnly {
		return nil
	}
	return &antigravityThoughtContinuation{
		service:        s,
		account:        account,
		policy:         policy,
		requestedModel: requestedModel,
		prefix:         prefix,
		proxyURL:       proxyURL,
		accessToken:    accessToken,
		action:         action,
		body:           body,
		baseURL:        resolveAntigravityForwardBaseURL(account),
		generation:     newAntigravityThoughtGeneration(),
	}
}

type antigravityThoughtContinuationKey struct{}

// WithAntigravityThoughtContinuation publishes the Task24 state to the response path.
func WithAntigravityThoughtContinuation(ctx context.Context, cont *antigravityThoughtContinuation) context.Context {
	if cont == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, antigravityThoughtContinuationKey{}, cont)
}

// antigravityThoughtContinuationFromContext returns the Task24 state of this request, or
// nil when both switches were off when the request started.
func antigravityThoughtContinuationFromContext(ctx context.Context) *antigravityThoughtContinuation {
	if ctx == nil {
		return nil
	}
	cont, _ := ctx.Value(antigravityThoughtContinuationKey{}).(*antigravityThoughtContinuation)
	return cont
}

// antigravityThoughtContinuationFromGin is the same lookup for the response handlers,
// which only carry the Gin context.
func antigravityThoughtContinuationFromGin(c *gin.Context) *antigravityThoughtContinuation {
	if c == nil || c.Request == nil {
		return nil
	}
	return antigravityThoughtContinuationFromContext(c.Request.Context())
}

// publish attaches the Task24 state to the Gin request so the streaming and collecting
// handlers below can observe the response and drive the single appended generation.
func (c *antigravityThoughtContinuation) publish(ginCtx *gin.Context) {
	if c == nil || ginCtx == nil || ginCtx.Request == nil {
		return
	}
	ginCtx.Request = ginCtx.Request.WithContext(WithAntigravityThoughtContinuation(ginCtx.Request.Context(), c))
}

// observeLine feeds one upstream line into the generation currently being read.
func (c *antigravityThoughtContinuation) observeLine(line string) {
	if c == nil || c.generation == nil {
		return
	}
	c.generation.observeLine(line)
	if !c.extraUsed {
		return
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	c.extraPayload = true
}

// observePayload feeds one already unwrapped upstream response object.
func (c *antigravityThoughtContinuation) observePayload(payload []byte) {
	if c == nil || c.generation == nil {
		return
	}
	c.generation.observePayload(payload)
	if c.extraUsed && strings.TrimSpace(string(payload)) != "" {
		c.extraPayload = true
	}
}

// withholdSuccessfulTerminal reports whether the successful terminal of the generation in
// flight must not reach the client yet. It is true when A suppresses this response, or
// when B still intends to append the received thinking and therefore needs the stream to
// stay open.
func (c *antigravityThoughtContinuation) withholdSuccessfulTerminal() bool {
	if c == nil || c.generation == nil || !c.generation.thoughtOnlySuccessful() {
		return false
	}
	if c.policy.SuppressThoughtOnlyStop {
		return true
	}
	return c.policy.ContinueThoughtOnly && !c.extraUsed
}

// suppressFinalTerminal reports whether the response must end without a successful
// completion signal. That is A's effective decision for a thought-only completion, and it is
// also the only legal ending after the appended generation failed: the client must never see
// a failed second generation reported as a successful first one, whatever A is set to.
func (c *antigravityThoughtContinuation) suppressFinalTerminal() bool {
	if c == nil {
		return false
	}
	if c.continuationFailed {
		return true
	}
	if !c.policy.SuppressThoughtOnlyStop {
		return false
	}
	generation := c.effectiveGeneration()
	if generation == nil {
		return false
	}
	return generation.thoughtOnlySuccessful()
}

// effectiveGeneration is the generation the downstream body describes: the appended
// generation once it produced upstream data, otherwise the first generation whose response is
// still the one being sent when the appended generation turned out empty.
func (c *antigravityThoughtContinuation) effectiveGeneration() *antigravityThoughtGeneration {
	if c == nil {
		return nil
	}
	if c.extraPayload || c.finishedGeneration == nil {
		return c.generation
	}
	return c.finishedGeneration
}

// continuationFailure reports whether the appended generation failed.
func (c *antigravityThoughtContinuation) continuationFailure() bool {
	return c != nil && c.continuationFailed
}

// canContinue reports whether the single appended generation is still available for this
// response.
func (c *antigravityThoughtContinuation) canContinue() bool {
	if c == nil || !c.policy.ContinueThoughtOnly || c.extraUsed || c.generation == nil {
		return false
	}
	if !c.generation.thoughtOnlySuccessful() {
		return false
	}
	return c.generation.replayableTail() != nil
}

// cumulativeUsage folds the finished generations into the usage of the generation in
// flight, so the client and the billing path see what the upstream actually consumed.
func (c *antigravityThoughtContinuation) cumulativeUsage(last ClaudeUsage) ClaudeUsage {
	if c == nil {
		return last
	}
	return addClaudeUsage(c.baseUsage, last)
}

// continueOnce issues the one allowed appended generation and returns its response.
//
// The appended request reuses the exact original upstream request envelope (same project,
// requestId, model, session, system instruction, tools, generation config and headers) and
// only appends the received thinking/signature as the final model turn. It is sent through
// the normal HTTPUpstream with the same account, proxy and concurrency slot; the retry
// loop, scheduler, sticky identity and capture range are deliberately not re-entered.
func (c *antigravityThoughtContinuation) continueOnce(ctx context.Context, previous *http.Response) (*http.Response, ClaudeUsage, error) {
	if c == nil {
		return nil, ClaudeUsage{}, errAntigravityThoughtContinuationUnavailable
	}
	if !c.canContinue() {
		return nil, ClaudeUsage{}, errAntigravityThoughtContinuationUnavailable
	}
	tail := c.generation.replayableTail()
	body, err := appendAntigravityThinkingTail(c.body, tail)
	if err != nil {
		return nil, ClaudeUsage{}, err
	}

	// The append budget is consumed even when the request below fails: a failed appended
	// generation must not turn into an unbounded loop.
	finished := c.generation.claudeUsage
	c.baseUsage = addClaudeUsage(c.baseUsage, finished)
	c.mergeBaseRawUsage(c.generation.rawUsageMetadata)
	c.extraUsed = true
	c.finishedGeneration = c.generation
	c.generation = newAntigravityThoughtGeneration()
	// The appended generation repeats the same envelope, so the request body becomes the
	// envelope of the generation in flight as well.
	c.body = body

	baseURL := c.baseURL
	if previous != nil && previous.Request != nil && previous.Request.URL != nil {
		baseURL = previous.Request.URL.Scheme + "://" + previous.Request.URL.Host
	}
	req, err := antigravity.NewAPIRequestWithURL(ctx, baseURL, c.action, c.accessToken, body)
	if err != nil {
		return nil, finished, err
	}
	if c.service == nil || c.service.httpUpstream == nil {
		return nil, finished, errors.New("antigravity upstream transport unavailable")
	}
	resp, err := c.service.httpUpstream.Do(req, c.proxyURL, c.account.ID, c.account.Concurrency)
	if err != nil {
		return nil, finished, fmt.Errorf("thought continuation request failed: %w", err)
	}
	if resp == nil {
		return nil, finished, errors.New("thought continuation returned nil response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// Only a 2xx response is a usable appended generation. Every other status runs the
		// gateway's normal same-account error bookkeeping (rate limit / account error
		// policy) without re-entering the retry loop, the scheduler, sticky identity or an
		// account switch; the failure surfaces as a protocol error, never as success.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, c.service.upstreamErrorBodyReadLimit()))
		_ = resp.Body.Close()
		c.service.handleUpstreamError(ctx, c.prefix, c.account, resp.StatusCode, resp.Header, body,
			c.requestedModel, 0, "", false)
		return nil, finished, &antigravityThoughtContinuationStatusError{
			StatusCode: resp.StatusCode,
			Body:       body,
			Header:     resp.Header.Clone(),
		}
	}
	c.extraObserved = true
	return resp, finished, nil
}

func (c *antigravityThoughtContinuation) mergeBaseRawUsage(raw map[string]int64) {
	if c == nil || len(raw) == 0 {
		return
	}
	if c.baseRawUsage == nil {
		c.baseRawUsage = make(map[string]int64, len(raw))
	}
	for key, value := range raw {
		c.baseRawUsage[key] += value
	}
}

// recordFailure records a failed or unavailable appended generation. The client already
// holds the received thinking; the gateway reports the failed append in the ops stream and
// in the logs, and the single-append budget stays consumed.
func (c *antigravityThoughtContinuation) recordFailure(ginCtx *gin.Context, stage string, err error) {
	if c == nil || err == nil {
		return
	}
	message := sanitizeUpstreamErrorMessage(err.Error())
	status := 0
	var statusErr *antigravityThoughtContinuationStatusError
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode
	}
	c.continuationFailed = true
	c.terminalErr = &AntigravityThoughtContinuationError{Stage: stage, StatusCode: status, Err: err}
	logger.LegacyPrintf("service.antigravity_gateway", "%s thought_continuation_failed stage=%s status=%d account=%d error=%v",
		c.prefix, stage, status, c.account.ID, err)
	appendOpsUpstreamError(ginCtx, OpsUpstreamErrorEvent{
		Platform:           c.account.Platform,
		AccountID:          c.account.ID,
		AccountName:        c.account.Name,
		UpstreamStatusCode: status,
		Kind:               "thought_continuation_error",
		Message:            message,
	})
}

// failureError is the Task24 terminal error of this response, or nil when no appended
// generation failed. Public entry points return it together with the billable first-generation
// result instead of pretending the response completed.
func (c *antigravityThoughtContinuation) failureError() error {
	if c == nil || c.terminalErr == nil {
		return nil
	}
	return c.terminalErr
}

// appendAntigravityThinkingTail appends the received thinking parts as the final model
// turn of the exact upstream request envelope. Nothing else in the envelope changes: no
// new user/system text, no blank turn and no instruction.
func appendAntigravityThinkingTail(body []byte, tail []antigravityThoughtTailPart) ([]byte, error) {
	if len(body) == 0 {
		return nil, errors.New("missing upstream request envelope")
	}
	if len(tail) == 0 {
		return nil, errAntigravityThoughtContinuationUnavailable
	}
	parts := make([]json.RawMessage, 0, len(tail))
	for _, part := range tail {
		// 逐字回放上游实际发来的 part：thinking 布尔、text 是否存在、签名 carrier 的顺序与
		// 形状都不重写（缺失的 text 不会补成空串），也不把签名挪到别的 part 上。
		if len(part.raw) > 0 {
			parts = append(parts, json.RawMessage(part.raw))
			continue
		}
		parts = append(parts, part.rebuildWithoutRaw())
	}
	content := map[string]any{"role": "model", "parts": parts}

	updated, err := sjson.SetBytes(body, "request.contents.-1", content)
	if err == nil {
		return updated, nil
	}
	// The native Gemini route wraps the body the same way; the fallback keeps the
	// continuation working if a future envelope exposes contents at the top level.
	return sjson.SetBytes(body, "contents.-1", content)
}

// antigravityPayloadHasSuccessfulFinishReason reports whether this upstream payload carries
// the normal completion signal.
func antigravityPayloadHasSuccessfulFinishReason(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	return gjson.GetBytes(payload, "candidates.0.finishReason").String() == antigravitySuccessfulFinishReason
}

// antigravityStripSuccessfulFinishReason removes only the successful finish reason from an
// upstream payload. Every other finish reason, promptFeedback, safety result and usage
// value stays exactly as the upstream sent it.
func antigravityStripSuccessfulFinishReason(payload []byte) ([]byte, bool) {
	if len(payload) == 0 {
		return payload, false
	}
	reason := gjson.GetBytes(payload, "candidates.0.finishReason")
	if reason.Type != gjson.String || reason.String() != antigravitySuccessfulFinishReason {
		return payload, false
	}
	updated, err := sjson.DeleteBytes(payload, "candidates.0.finishReason")
	if err != nil {
		return payload, false
	}
	return updated, true
}

// antigravityAddUsageBaseline adds the finished generations' usage to the usageMetadata of
// an upstream payload, so the client sees the cumulative usage of the whole response. It
// returns the payload unchanged when there is no usageMetadata to extend.
func antigravityAddUsageBaseline(payload []byte, baseline map[string]int64) ([]byte, bool) {
	if len(payload) == 0 || len(baseline) == 0 {
		return payload, false
	}
	if !gjson.GetBytes(payload, "usageMetadata").IsObject() {
		return payload, false
	}
	updated := payload
	changed := false
	for key, value := range baseline {
		if value == 0 {
			continue
		}
		total := value
		if current := gjson.GetBytes(updated, "usageMetadata."+key); current.Type == gjson.Number {
			total += current.Int()
		}
		next, err := sjson.SetBytes(updated, "usageMetadata."+key, total)
		if err != nil {
			continue
		}
		updated = next
		changed = true
	}
	if !changed {
		return payload, false
	}
	return updated, true
}

// addClaudeUsage sums two usage records field by field; a continued response is billed
// and reported as the sum of every upstream generation it took.
func addClaudeUsage(a, b ClaudeUsage) ClaudeUsage {
	return ClaudeUsage{
		InputTokens:              a.InputTokens + b.InputTokens,
		OutputTokens:             a.OutputTokens + b.OutputTokens,
		CacheCreationInputTokens: a.CacheCreationInputTokens + b.CacheCreationInputTokens,
		CacheReadInputTokens:     a.CacheReadInputTokens + b.CacheReadInputTokens,
		ImageOutputTokens:        a.ImageOutputTokens + b.ImageOutputTokens,
		CacheCreation5mTokens:    a.CacheCreation5mTokens + b.CacheCreation5mTokens,
		CacheCreation1hTokens:    a.CacheCreation1hTokens + b.CacheCreation1hTokens,
	}
}

// antigravityStripSuccessfulFinishReasonFromResponse removes the successful finish reason
// from an already decoded response object, keeping every other field including
// usageMetadata exactly as the upstream sent it.
func antigravityStripSuccessfulFinishReasonFromResponse(response map[string]any) {
	candidates, ok := response["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return
	}
	if reason, _ := candidate["finishReason"].(string); reason != antigravitySuccessfulFinishReason {
		return
	}
	delete(candidate, "finishReason")
}

// jsonNumberAsInt64 reads a JSON number field from a decoded response object.
func jsonNumberAsInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// antigravityAddUsageBaselineToResponse does the same for an already decoded response
// object, which is how the non-streaming Gemini route emits its single body.
func antigravityAddUsageBaselineToResponse(response map[string]any, baseline map[string]int64) {
	if len(response) == 0 || len(baseline) == 0 {
		return
	}
	usage, ok := response["usageMetadata"].(map[string]any)
	if !ok || len(usage) == 0 {
		return
	}
	for key, value := range baseline {
		if value == 0 {
			continue
		}
		total := value
		if current, ok := jsonNumberAsInt64(usage[key]); ok {
			total += current
		}
		usage[key] = total
	}
}
