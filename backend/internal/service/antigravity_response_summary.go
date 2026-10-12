package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Counts content-bearing downstream events (not tokens or bytes). No payload
// value or signature is retained. Collect responses count their content items.
type antigravityDeliverySummary struct {
	Protocol       string
	Stream         bool
	ThinkingEvents int
	TextEvents     int
	ToolEvents     int
	ProtocolError  bool
}

// Observe only successfully written complete response frames. Gemini SSE and
// the protocol adapters already supply complete data lines per Write.
func (s *antigravityDeliverySummary) observe(data []byte) {
	if !s.Stream {
		s.observeJSON(gjson.ParseBytes(data))
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "data:") {
			s.observeJSON(gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		}
	}
}
func (s *antigravityDeliverySummary) observeJSON(root gjson.Result) {
	if root.Get("error").Exists() || root.Get("type").String() == "error" {
		s.ProtocolError = true
	}
	switch s.Protocol {
	case "messages":
		if !s.Stream {
			for _, item := range root.Get("content").Array() {
				s.observeAnthropicItem(item)
			}
		} else {
			switch root.Get("type").String() {
			case "content_block_delta":
				switch root.Get("delta.type").String() {
				case "text_delta":
					if root.Get("delta.text").String() != "" {
						s.TextEvents++
					}
				}
			case "content_block_start":
				switch root.Get("content_block.type").String() {
				case "tool_use":
					s.ToolEvents++
				case "thinking":
					s.ThinkingEvents++
				}
			}
		}
	case "chat_completions":
		for _, choice := range root.Get("choices").Array() {
			item := choice.Get("message")
			if s.Stream {
				item = choice.Get("delta")
			}
			if item.Get("reasoning_content").String() != "" {
				s.ThinkingEvents++
			}
			if item.Get("content").String() != "" {
				s.TextEvents++
			}
			s.ToolEvents += len(item.Get("tool_calls").Array())
		}
	case "responses":
		if !s.Stream {
			for _, item := range root.Get("output").Array() {
				switch item.Get("type").String() {
				case "reasoning":
					s.ThinkingEvents++
				case "function_call":
					s.ToolEvents++
				case "message":
					for _, part := range item.Get("content").Array() {
						if part.Get("text").String() != "" {
							s.TextEvents++
						}
					}
				}
			}
		} else {
			switch root.Get("type").String() {
			case "response.output_text.delta":
				if root.Get("delta").String() != "" {
					s.TextEvents++
				}
			case "response.output_item.added":
				switch root.Get("item.type").String() {
				case "function_call":
					s.ToolEvents++
				case "reasoning":
					s.ThinkingEvents++
				}
			case "response.failed":
				s.ProtocolError = true
			}
		}
	case "gemini":
		for _, candidate := range root.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				if part.Get("functionCall").IsObject() {
					s.ToolEvents++
				}
				if part.Get("thought").Bool() {
					if part.Get("text").String() != "" || part.Get("thoughtSignature").String() != "" {
						s.ThinkingEvents++
					}
				}
				if (!part.Get("thought").Bool() && part.Get("text").String() != "") || part.Get("inlineData.data").String() != "" {
					s.TextEvents++
				}
			}
		}
	}
}
func (s *antigravityDeliverySummary) observeAnthropicItem(item gjson.Result) {
	switch item.Get("type").String() {
	case "thinking":
		if item.Get("thinking").String() != "" || item.Get("signature").String() != "" {
			s.ThinkingEvents++
		}
	case "text":
		if item.Get("text").String() != "" {
			s.TextEvents++
		}
	case "tool_use":
		s.ToolEvents++
	}
}

type antigravitySummaryWriter struct {
	gin.ResponseWriter
	summary *antigravityDeliverySummary
}

func (w *antigravitySummaryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil && n == len(p) {
		w.summary.observe(p)
	}
	return n, err
}
func (w *antigravitySummaryWriter) WriteString(p string) (int, error) {
	n, err := w.ResponseWriter.WriteString(p)
	if err == nil && n == len(p) {
		w.summary.observe([]byte(p))
	}
	return n, err
}

// startResponse pins the processed protocol and wraps the actual client writer.
// No instance locks/state: the Task23 shallow-copy transport contract is unchanged.
func (c *antigravityThoughtContinuation) startResponse(ginCtx *gin.Context, protocol string, stream bool) func() {
	if c == nil {
		return func() {}
	}
	c.delivery.Protocol = protocol
	c.delivery.Stream = stream
	c.generation.nativeGemini = protocol == "gemini"
	c.publish(ginCtx)
	original := ginCtx.Writer
	ginCtx.Writer = &antigravitySummaryWriter{ResponseWriter: original, summary: &c.delivery}
	return func() {
		ginCtx.Writer = original
		if !original.Written() {
			return
		} // A failover with no client response is not request completion.
		const key = "antigravity_response_summary_written"
		if _, exists := ginCtx.Get(key); exists {
			return
		}
		ginCtx.Set(key, true)
		fields := c.summaryFields(ginCtx)
		attrs := make([]zap.Field, 0, len(fields)+1)
		attrs = append(attrs, zap.String("component", "service.antigravity_gateway"))
		for name, value := range fields {
			attrs = append(attrs, zap.Any(name, value))
		}
		logger.FromContext(ginCtx.Request.Context()).Info("antigravity.response_summary", attrs...)
	}
}
func (c *antigravityThoughtContinuation) summaryFields(ginCtx *gin.Context) map[string]any {
	g := c.effectiveGeneration()
	reason := "eligible_not_finalized"
	decision := "skipped"
	switch {
	case c.continuationFailed:
		reason = "continuation_failed"
	case c.noUsableAnswerErr != nil:
		decision = "retryable_error"
		reason = "thinking_only"
		if g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED" {
			reason = "missing_terminal"
		} else if !g.thoughtOnlySuccessful() {
			reason = "empty"
		}
	case !c.policy.SuppressThoughtOnlyStop:
		reason = "policy_off"
	case ginCtx.Request.Context().Err() != nil:
		reason = "canceled"
	case c.delivery.ProtocolError:
		reason = "protocol_error"
	case g == nil || !g.sawResponse:
		reason = "no_candidate"
	case g.hasRefusal:
		reason = "refusal"
	case g.hasVisibleText:
		reason = "visible_content"
	case g.hasToolCall:
		reason = "tool"
	case g.hasGrounding:
		reason = "grounding"
	case !g.noUsableAnswerEligible():
		reason = "explicit_terminal"
	}
	fields := map[string]any{
		"account_id": c.account.ID, "protocol": c.delivery.Protocol, "stream": c.delivery.Stream,
		"downstream_thinking_events": c.delivery.ThinkingEvents, "downstream_text_events": c.delivery.TextEvents, "downstream_tool_events": c.delivery.ToolEvents,
		"effective_a": c.policy.SuppressThoughtOnlyStop, "effective_b": c.policy.ContinueThoughtOnly,
		"a_decision": decision, "a_reason": reason,
	}
	if g != nil {
		fields["upstream_finish_reason"] = safeAntigravityFinishReason(g.finishReason)
		fields["detector_visible"] = g.hasVisibleText
		fields["detector_tool"] = g.hasToolCall
		fields["detector_grounding"] = g.hasGrounding
		fields["detector_rendered_grounding"] = g.hasRenderedGrounding
		fields["detector_refusal"] = g.hasRefusal
	}
	for _, key := range []ctxkey.Key{ctxkey.RequestID, ctxkey.ClientRequestID} {
		if value, ok := ginCtx.Request.Context().Value(key).(string); ok && value != "" {
			fields[stringKey(key)] = truncateString(sanitizeUpstreamErrorMessage(strings.Join(strings.Fields(value), " ")), 128)
		}
	}
	return fields
}
func stringKey(key ctxkey.Key) string {
	if key == ctxkey.ClientRequestID {
		return "client_request_id"
	}
	return "request_id"
}
func safeAntigravityFinishReason(value string) string {
	switch value {
	case "", "STOP", "FINISH_REASON_UNSPECIFIED", "MAX_TOKENS", "SAFETY", "RECITATION", "LANGUAGE", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS":
		return value
	default:
		return "UNRECOGNIZED"
	}
}
