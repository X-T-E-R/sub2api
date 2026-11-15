package service

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Each existing inference attempt owns its header wait. The helper never wraps
// the surrounding retry loop and cannot add a request of its own.
func (s *AntigravityGatewayService) doAntigravityInference(p antigravityRetryLoopParams, req *http.Request, capture *GeminiCaptureAttempt, observe ...func(*http.Response, []byte)) (*http.Response, error, bool) {
	send := func(request *http.Request) (*http.Response, error) {
		resp, err := p.httpUpstream.DoWithTLS(request, p.proxyURL, p.account.ID, p.account.Concurrency, s.resolveTLSProfile(p.account))
		s.proxyStreamCircuit.observeTransport(p.ctx, p.account, resp, err)
		if capture != nil {
			if resp != nil {
				resp = capture.AttachResponse(resp)
			} else if err != nil {
				capture.MarkRequestError(err)
			} else {
				capture.MarkRequestError(errors.New("upstream returned nil response"))
			}
		}
		return resp, err
	}
	if !p.allowPreheader {
		resp, err := send(req)
		return resp, err, false
	}
	return requestStreamPreheader(p.c, req, p.account, send, func(response *http.Response, body []byte) []byte {
		if len(observe) > 0 {
			observe[0](response, body)
			return body
		}
		status := response.StatusCode
		if handled, _, _ := s.applyErrorPolicy(p, status, response.Header, body); !handled &&
			!s.handleAntigravityModelRateLimitBeforePolicy(p, status, response.Header, body) {
			s.handleUpstreamError(p.ctx, p.prefix, p.account, status, response.Header, body,
				p.requestedModel, p.groupID, p.sessionHash, p.isStickySession)
		}
		return body
	})
}

func antigravityPreheaderIncomplete(c *gin.Context, g *antigravityThoughtGeneration, cont *antigravityThoughtContinuation) bool {
	if g == nil || g.hasRefusal || cont.canContinue() || (g.finishReason != "" && g.finishReason != "FINISH_REASON_UNSPECIFIED") {
		return false
	}
	// Let the Antigravity finalizer report its existing typed failure after a
	// clean EOF. Only otherwise-unclassified missing terminals use the generic
	// gateway error; neither classification allows a synthetic success.
	if c.Request.Context().Err() == nil && cont.noUsableAnswerCode() != "" {
		return false
	}
	// An empty appended generation retains Task24's original explicit outcome
	// for the already-complete first generation; a partial B still needs error.
	return !(cont != nil && cont.hasExtraGeneration() && !g.sawPayload)
}

func antigravityPreheaderHasUsage(res *antigravityStreamResult) bool {
	return res != nil && res.usage != nil && *res.usage != (ClaudeUsage{})
}

// A legal append keeps its original same-account bookkeeping and budget. Only
// its already-committed, opt-in client framing changes to retain the real error.
func finishStreamPreheaderContinuationError(c *gin.Context, writer *antigravityClientWriter, err error, response ...streamPreheaderResponseMeta) bool {
	if !StreamPreheaderConfigured(c) || !c.Writer.Written() {
		return false
	}
	var upstream *antigravityThoughtContinuationStatusError
	if !errors.As(err, &upstream) || upstream.StatusCode < 400 || upstream.StatusCode > 599 {
		return false
	}
	if StreamPreheaderFinalized(c) {
		return true
	}
	meta := streamPreheaderResponseMeta{}
	if len(response) > 0 {
		meta = response[0]
	}
	meta.retryDelay = upstream.RetryDelay
	value, _ := c.Get(streamPreheaderPolicyKey)
	protocol := value.(streamPreheaderPolicy).protocol
	errType := upstream.ErrorType
	if protocol == streamPreheaderGemini {
		errType = upstream.Code
	}
	message := upstream.Message
	if message == "" {
		message = http.StatusText(upstream.StatusCode)
	}
	if frame := recordStreamPreheaderFailure(c, protocol, upstream.StatusCode, errType, message, upstream.RetryAfter, meta); frame != "" {
		writer.Write([]byte(frame))
	}
	return true
}
