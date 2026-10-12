package service

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

const antigravityNoUsableAnswerMessage = "Upstream completed without a usable answer. Please retry the request."

// AntigravityNoUsableAnswerError is a billable partial response, already reported
// downstream as a retryable provider error. It is not a gateway failover signal:
// only the client may start another attempt after receiving the original thinking.
type AntigravityNoUsableAnswerError struct {
	Stage string
}

func (*AntigravityNoUsableAnswerError) Error() string { return antigravityNoUsableAnswerMessage }

func IsAntigravityNoUsableAnswerError(err error) bool {
	var target *AntigravityNoUsableAnswerError
	return errors.As(err, &target)
}

// IsAntigravityPartialResponseError identifies errors whose partial result must
// be billed, without gateway failover or a second generic error response.
func IsAntigravityPartialResponseError(err error) bool {
	return IsAntigravityNoUsableAnswerError(err) || IsAntigravityThoughtContinuationError(err)
}

// Called only at clean upstream EOF, never on refusal, length, read error or
// cancellation. A includes thinking-only/empty STOP and actual thinking with
// a missing terminal. B still requires thinking-only STOP and never appends either.
func (c *antigravityThoughtContinuation) completeNoUsableAnswer(ginCtx *gin.Context, stage string) bool {
	if c == nil || c.continuationFailed || !c.policy.SuppressThoughtOnlyStop || !c.effectiveGeneration().noUsableAnswerEligible() {
		return false
	}
	if ginCtx.Request.Context().Err() != nil {
		return false
	}
	if c.noUsableAnswerErr == nil {
		c.noUsableAnswerErr = &AntigravityNoUsableAnswerError{Stage: stage}
		reason := "empty"
		generation := c.effectiveGeneration()
		if generation.finishReason == "" || generation.finishReason == "FINISH_REASON_UNSPECIFIED" {
			reason = "missing_terminal"
		} else if generation.thoughtOnlySuccessful() {
			reason = "thinking_only"
		}
		appendOpsUpstreamError(ginCtx, OpsUpstreamErrorEvent{
			Platform: c.account.Platform, AccountID: c.account.ID, AccountName: c.account.Name,
			UpstreamStatusCode: http.StatusOK, Kind: "no_usable_answer", Stage: stage,
			Reason: reason, Message: antigravityNoUsableAnswerMessage,
		})
	}
	return true
}

func writeAntigravityNoUsableAnswerAnthropic(writer *antigravityClientWriter) {
	writer.Fprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", antigravityNoUsableAnswerMessage)
}

func antigravityNoUsableAnswerGoogleError() map[string]any {
	return map[string]any{"code": http.StatusBadGateway, "status": "UNAVAILABLE", "message": antigravityNoUsableAnswerMessage}
}
