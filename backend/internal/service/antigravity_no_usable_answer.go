package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
)

const antigravityNoUsableAnswerMessage = "Upstream completed without a usable answer."

// antigravityThinkingOnlyCode is the only code that authorises a consumer to
// consider one explicit continuation: authoritative clean EOF, STOP, actual
// non-empty thinking delivered, and no text, tool call, grounding or refusal.
const antigravityThinkingOnlyCode = "antigravity_thinking_only"

// AntigravityNoUsableAnswerError is an already-reported, billable partial
// outcome, never a failover signal. Code, not HTTP status or api_error, decides
// whether a harness may consider one explicit continuation.
type AntigravityNoUsableAnswerError struct {
	Stage string
	Code  string
}

func (e *AntigravityNoUsableAnswerError) Error() string { return e.message() }
func (e *AntigravityNoUsableAnswerError) message() string {
	if e == nil || e.Code == antigravityThinkingOnlyCode || e.Code == "antigravity_empty_response" || e.Code == "antigravity_missing_terminal" {
		return antigravityNoUsableAnswerMessage
	}
	return antigravity.TerminalFailureMessage(e.Code)
}
func (e *AntigravityNoUsableAnswerError) status() int {
	if e != nil && e.Code == "antigravity_safety" {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}
func (e *AntigravityNoUsableAnswerError) protocolError(protocol string) map[string]any {
	typ := "api_error"
	if protocol == "chat" {
		typ = "server_error"
	}
	if e.Code == "antigravity_safety" {
		typ = "invalid_request_error"
	}
	return map[string]any{"type": typ, "code": e.Code, "message": e.message(), "retryable": false}
}
func IsAntigravityNoUsableAnswerError(err error) bool {
	var target *AntigravityNoUsableAnswerError
	return errors.As(err, &target)
}

// Partial outcomes retain actual usage and never replay tools or write a second
// generic handler error, including when the provider failed after visible text.
func IsAntigravityPartialResponseError(err error) bool {
	return IsAntigravityNoUsableAnswerError(err) || IsAntigravityThoughtContinuationError(err)
}

func (g *antigravityThoughtGeneration) failureCode(policy AntigravityThoughtPolicy) string {
	if g == nil || g.readFailed {
		return ""
	}
	if g.hasRefusal {
		return "antigravity_safety"
	}
	if code := antigravity.TerminalFailureCode(g.finishReason); code != "" {
		return code
	}
	if !policy.SuppressThoughtOnlyStop || !g.noUsableAnswerEligible() {
		return ""
	}
	if g.finishReason == "" || g.finishReason == "FINISH_REASON_UNSPECIFIED" {
		return "antigravity_missing_terminal"
	}
	if g.hasThinking {
		return "antigravity_thinking_only"
	}
	return "antigravity_empty_response"
}

// Finalize only after draining clean EOF, including trailing usage frames.
// Explicit provider failures are not gated by A. A still controls empty/normal
// thinking-only/missing-terminal suppression with its original account override.
func (c *antigravityThoughtContinuation) completeNoUsableAnswer(ginCtx *gin.Context, stage string) bool {
	if c == nil || c.continuationFailed || ginCtx.Request.Context().Err() != nil {
		return false
	}
	if c.noUsableAnswerErr != nil {
		return true
	}
	code := c.effectiveGeneration().failureCode(c.policy)
	if code == "" {
		return false
	}
	c.recordTerminalFailure(ginCtx, stage, code)
	return true
}
func (c *antigravityThoughtContinuation) recordTerminalFailure(ginCtx *gin.Context, stage, code string) {
	if c.noUsableAnswerErr != nil {
		return
	}
	c.noUsableAnswerErr = &AntigravityNoUsableAnswerError{Stage: stage, Code: code}
	reason := strings.TrimPrefix(code, "antigravity_")
	if reason == "empty_response" {
		reason = "empty"
	}
	appendOpsUpstreamError(ginCtx, OpsUpstreamErrorEvent{
		Platform: c.account.Platform, AccountID: c.account.ID, AccountName: c.account.Name,
		UpstreamStatusCode: http.StatusOK, Kind: "no_usable_answer", Stage: stage,
		Reason: reason, Message: c.noUsableAnswerErr.message(),
	})
}
func (c *antigravityThoughtContinuation) responseFailure() *AntigravityNoUsableAnswerError {
	if c == nil {
		return nil
	}
	return c.noUsableAnswerErr
}
func writeAntigravityNoUsableAnswerAnthropic(writer *antigravityClientWriter, failure *AntigravityNoUsableAnswerError) {
	data, _ := json.Marshal(map[string]any{"type": "error", "error": failure.protocolError("messages")})
	writer.Fprintf("event: error\ndata: %s\n\n", data)
}
func antigravityNoUsableAnswerGoogleError(failure *AntigravityNoUsableAnswerError) map[string]any {
	status := "UNAVAILABLE"
	if failure.Code == "antigravity_safety" {
		status = "PERMISSION_DENIED"
	}
	return map[string]any{"code": failure.status(), "status": status, "message": failure.message(), "details": []any{map[string]any{
		"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": failure.Code, "domain": "sub2api.antigravity",
	}}}
}
