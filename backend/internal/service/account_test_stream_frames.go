package service

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
)

// Shared plumbing for the account-test SSE parsers in account_test_service.go
// (processClaudeStream, processGeminiStream, processOpenAIStream and
// processOpenAIChatCompletionsStream).
//
// Two concerns are factored out of the per-protocol loops:
//
//  1. Reading the "data:" lines of an upstream body incrementally. Every frame is
//     handled as soon as its line is read, which is what keeps content deltas
//     truly incremental, and no token-size ceiling is imposed because Gemini image
//     frames carry multi-hundred-KB base64 payloads.
//  2. Publishing the upstream response model the provider echoed in its raw
//     frames as test_metadata events, so the admin test UI can show a model echo
//     mismatch without the parser having to interpret every frame dialect twice.
//
// A stream only counts as a success when it both produced meaningful ordinary
// output (text, or a Gemini image part) and reached a real terminal event. An
// empty stream, a thought/reasoning-only stream, a premature EOF and a cancelled
// request all end in sendErrorAndEnd instead of a fake test_complete.

// sseFrameKind selects which Observe method of the shared upstream response
// model observer understands the raw frame dialect in hand.
type sseFrameKind int

const (
	sseFrameClaude sseFrameKind = iota
	sseFrameGemini
	sseFrameOpenAI
)

// sseDataFrameReader yields the payload of every SSE "data:" line of an upstream
// body. Lines that are not data frames (blank separator lines, comments,
// "event:"/"id:"/"retry:" fields) are skipped.
type sseDataFrameReader struct {
	reader *bufio.Reader
	done   bool
}

func newSSEDataFrameReader(reader *bufio.Reader) *sseDataFrameReader {
	return &sseDataFrameReader{reader: reader}
}

// next returns the payload of the next data frame (the "data:" prefix and the
// surrounding whitespace already stripped, so "[DONE]" is delivered verbatim).
// ok is false once the stream is exhausted; a nil err with ok false is a clean
// EOF. A non-EOF read error — a truncated/failed transfer or a cancelled request
// context — is reported as err.
func (r *sseDataFrameReader) next() (payload string, ok bool, err error) {
	if r.done {
		return "", false, nil
	}
	for {
		line, readErr := r.reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && sseDataPrefix.MatchString(trimmed) {
			if readErr != nil {
				r.done = true
				if readErr != io.EOF {
					// A terminal payload cannot turn a failed transfer into success.
					return "", false, readErr
				}
			}
			return sseDataPrefix.ReplaceAllString(trimmed, ""), true, nil
		}
		if readErr != nil {
			r.done = true
			if readErr == io.EOF {
				return "", false, nil
			}
			return "", false, readErr
		}
	}
}

// upstreamModelMetadataState publishes the raw upstream response model observed
// while parsing one account-test stream as test_metadata events.
type upstreamModelMetadataState struct {
	observer *upstreamResponseModelObserver
	known    bool
	model    string
	conflict bool
}

// newUpstreamModelMetadataState starts the response model observation for this
// stream, adopting an observer already published on the context instead of
// replacing it: a caller that observes raw upstream frames itself (the
// Antigravity raw-frame observer) keeps ownership of that observation.
func newUpstreamModelMetadataState(c *gin.Context) *upstreamModelMetadataState {
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	return &upstreamModelMetadataState{observer: observer}
}

// observe feeds one untouched upstream frame to the shared observer.
func (st *upstreamModelMetadataState) observe(kind sseFrameKind, eventType string, payload []byte) {
	if st == nil || st.observer == nil || len(payload) == 0 {
		return
	}
	switch kind {
	case sseFrameGemini:
		st.observer.ObserveGemini(payload)
	case sseFrameClaude:
		st.observer.ObserveAnthropic(payload)
	default:
		st.observer.ObserveOpenAI(payload, eventType)
	}
}

// emit sends a test_metadata event when the observed model or its conflict flag
// changed since the previous emission. Nothing is emitted while the upstream has
// not declared a model at all, so a provider that never echoes a model does not
// produce an empty metadata event.
func (st *upstreamModelMetadataState) emit(s *AccountTestService, c *gin.Context) {
	if st == nil || st.observer == nil || s == nil {
		return
	}
	model := st.observer.Model()
	conflict := st.observer.Conflict()
	if model == "" && !conflict {
		return
	}
	if st.known && st.model == model && st.conflict == conflict {
		return
	}
	st.known = true
	st.model = model
	st.conflict = conflict
	s.sendEvent(c, TestEvent{
		Type:                          "test_metadata",
		UpstreamResponseModel:         model,
		UpstreamResponseModelConflict: conflict,
	})
}

// finishGeminiStream reports the outcome of a finished Gemini stream. Only a
// stream that produced text or an image and declared completion via finishReason
// or [DONE] succeeds.
func (s *AccountTestService) finishGeminiStream(c *gin.Context, sawOutput, sawTerminal bool) error {
	if sawOutput && sawTerminal {
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}
	switch {
	case !sawOutput && sawTerminal:
		return s.sendErrorAndEnd(c, "Gemini stream completed without any text or image output")
	case sawOutput:
		return s.sendErrorAndEnd(c, "Gemini stream ended before completion (no finishReason or [DONE])")
	default:
		return s.sendErrorAndEnd(c, "Gemini stream ended without any text or image output")
	}
}

// finishClaudeStream reports the outcome of a Claude stream that reached a
// terminal event ([DONE] or message_stop). Thought-only and empty responses fail
// the probe instead of reporting a fake success.
func (s *AccountTestService) finishClaudeStream(c *gin.Context, sawOutput bool) error {
	if !sawOutput {
		return s.sendErrorAndEnd(c, "Claude stream completed without any text output")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// finishOpenAIStream reports the outcome of an OpenAI Responses stream. Only a
// stream that produced text and reached response.completed / response.done (or a
// [DONE] sentinel) succeeds.
func (s *AccountTestService) finishOpenAIStream(c *gin.Context, sawOutput, sawTerminal bool) error {
	if sawOutput && sawTerminal {
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}
	if !sawTerminal {
		return s.sendErrorAndEnd(c, "Stream ended before response.completed")
	}
	return s.sendErrorAndEnd(c, "OpenAI stream completed without any text output")
}

// finishChatCompletionsStream reports the outcome of a Chat Completions stream
// that reached [DONE] or a finish_reason. Only a stream that actually delivered
// text passes.
func (s *AccountTestService) finishChatCompletionsStream(c *gin.Context, sawOutput, sawTerminal bool) error {
	if sawOutput && sawTerminal {
		s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 /v1/chat/completions 验证"})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}
	if !sawTerminal {
		return s.sendErrorAndEnd(c, "Chat Completions stream from /v1/chat/completions ended before [DONE]")
	}
	return s.sendErrorAndEnd(c, "Chat Completions stream from /v1/chat/completions completed without any text output")
}

// claudeStreamStopReason returns the stop_reason declared by a Claude
// message_delta event (message_delta.delta.stop_reason, with a tolerant
// top-level fallback), or "" when the event carries none.
func claudeStreamStopReason(data map[string]any) string {
	if delta, ok := data["delta"].(map[string]any); ok {
		if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
			return reason
		}
	}
	if reason, ok := data["stop_reason"].(string); ok && reason != "" {
		return reason
	}
	return ""
}

// openAIStreamAbnormalTerminalMessage describes an OpenAI Responses terminal
// event that reports a non-successful end of the response.
func openAIStreamAbnormalTerminalMessage(eventType string, data map[string]any) string {
	base := "OpenAI stream ended before response.completed"
	switch eventType {
	case "response.incomplete":
		base = "OpenAI stream ended incomplete before response.completed"
	case "response.cancelled", "response.canceled":
		base = "OpenAI stream was cancelled before response.completed"
	}
	responseData, _ := data["response"].(map[string]any)
	if responseData == nil {
		return base
	}
	if errData, ok := responseData["error"].(map[string]any); ok {
		if msg, ok := errData["message"].(string); ok && msg != "" {
			return fmt.Sprintf("%s: %s", base, msg)
		}
	}
	if details, ok := responseData["incomplete_details"].(map[string]any); ok {
		if reason, ok := details["reason"].(string); ok && reason != "" {
			return fmt.Sprintf("%s (%s)", base, reason)
		}
	}
	return base
}
