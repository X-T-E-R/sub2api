package service

import (
	"errors"
	"net/http"
)

// Each existing inference attempt owns its header wait. The helper never wraps
// the surrounding retry loop and cannot add a request of its own.
func (s *AntigravityGatewayService) doAntigravityInference(p antigravityRetryLoopParams, req *http.Request, capture *GeminiCaptureAttempt, observe ...func(*http.Response, []byte)) (*http.Response, error, bool) {
	send := func(request *http.Request) (*http.Response, error) {
		resp, err := p.httpUpstream.Do(request, p.proxyURL, p.account.ID, p.account.Concurrency)
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

func antigravityPreheaderIncomplete(g *antigravityThoughtGeneration, cont *antigravityThoughtContinuation) bool {
	if g == nil || g.hasRefusal || (g.finishReason != "" && g.finishReason != "FINISH_REASON_UNSPECIFIED") {
		return false
	}
	// An empty appended generation retains Task24's original explicit outcome
	// for the already-complete first generation; a partial B still needs error.
	return !(cont != nil && cont.hasExtraGeneration() && !g.sawPayload)
}

func antigravityPreheaderHasUsage(res *antigravityStreamResult) bool {
	return res != nil && res.usage != nil && *res.usage != (ClaudeUsage{})
}
