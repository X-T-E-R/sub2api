package service

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

const antigravityNoUsableAnswerMessage = "Upstream completed with thinking but no usable answer. Please retry the request."

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
// cancellation. Keep A independent of B and classify only the original eligible
// STOP + thinking-only shape; an empty or interrupted response is not this error.
func (c *antigravityThoughtContinuation) completeNoUsableAnswer(ginCtx *gin.Context, stage string) bool {
	if c == nil || c.continuationFailed || !c.policy.SuppressThoughtOnlyStop || !c.effectiveGeneration().thoughtOnlySuccessful() {
		return false
	}
	if ginCtx.Request.Context().Err() != nil {
		return false
	}
	if c.noUsableAnswerErr == nil {
		c.noUsableAnswerErr = &AntigravityNoUsableAnswerError{Stage: stage}
		appendOpsUpstreamError(ginCtx, OpsUpstreamErrorEvent{
			Platform: c.account.Platform, AccountID: c.account.ID, AccountName: c.account.Name,
			UpstreamStatusCode: http.StatusOK, Kind: "no_usable_answer", Stage: stage,
			Reason: "thinking_only", Message: antigravityNoUsableAnswerMessage,
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
