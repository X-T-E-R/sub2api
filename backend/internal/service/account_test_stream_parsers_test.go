//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Narrow fixtures for the account-test stream parsers. They pin the two
// behaviours the parsers own: a stream only reports success when it delivered
// meaningful ordinary output AND reached a real terminal, and the raw upstream
// model echo is published as test_metadata whenever it changes.

// sseData wraps one upstream payload as an SSE data frame.
func sseData(payload string) string {
	return "data: " + payload + "\n\n"
}

// failingStreamReader serves its data and then fails with err, the way a
// truncated transfer or a cancelled request body does.
type failingStreamReader struct {
	data []byte
	err  error
}

func (r *failingStreamReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

type capturedTestEvent struct {
	Type                          string `json:"type"`
	Text                          string `json:"text"`
	Status                        string `json:"status"`
	Error                         string `json:"error"`
	Success                       bool   `json:"success"`
	ImageURL                      string `json:"image_url"`
	MimeType                      string `json:"mime_type"`
	UpstreamResponseModel         string `json:"upstream_response_model"`
	UpstreamResponseModelConflict bool   `json:"upstream_response_model_conflict"`
}

func captureTestEvents(t *testing.T, body string) []capturedTestEvent {
	t.Helper()
	var events []capturedTestEvent
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event capturedTestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		events = append(events, event)
	}
	return events
}

func eventTypes(events []capturedTestEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func countEventType(events []capturedTestEvent, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

// joinedContent concatenates the text of every content event in stream order.
func joinedContent(events []capturedTestEvent) string {
	var builder strings.Builder
	for _, event := range events {
		if event.Type == "content" {
			builder.WriteString(event.Text)
		}
	}
	return builder.String()
}

func metadataEvents(events []capturedTestEvent) []capturedTestEvent {
	var found []capturedTestEvent
	for _, event := range events {
		if event.Type == "test_metadata" {
			found = append(found, event)
		}
	}
	return found
}

// requireFailure asserts the parser ended with an error event, no success event
// and a non-nil error.
func requireFailure(t *testing.T, events []capturedTestEvent, err error, wantSubstring string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, 1, countEventType(events, "error"), "expected exactly one error event, got %v", eventTypes(events))
	require.Zero(t, countEventType(events, "test_complete"), "a failed probe must not report test_complete: %v", eventTypes(events))
	for _, event := range events {
		if event.Type == "error" {
			require.Contains(t, event.Error, wantSubstring)
			require.Contains(t, err.Error(), event.Error)
		}
	}
}

func requireSuccess(t *testing.T, events []capturedTestEvent, err error) {
	t.Helper()
	require.NoError(t, err)
	require.Equal(t, 1, countEventType(events, "test_complete"), "events: %v", eventTypes(events))
	for _, event := range events {
		if event.Type == "test_complete" {
			require.True(t, event.Success)
		}
	}
	require.Zero(t, countEventType(events, "error"), "events: %v", eventTypes(events))
}

// --- Gemini ---

func TestProcessGeminiStream_TextAndTerminalSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"text":"hi "}]}}],"modelVersion":"gemini-2.5-flash"}`) +
		sseData(`{"candidates":[{"content":{"parts":[{"text":"there"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-flash"}`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "hi there", joinedContent(events))

	meta := metadataEvents(events)
	require.Len(t, meta, 1, "unchanged echo must not re-emit metadata: %v", eventTypes(events))
	require.Equal(t, "gemini-2.5-flash", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
}

func TestProcessGeminiStream_DoneSentinelTerminalSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`) + sseData(`[DONE]`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "ok", joinedContent(events))
}

func TestProcessGeminiStream_ImageOnlyWithTerminalSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]},"finishReason":"STOP"}]}`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, 1, countEventType(events, "image"))
	require.Equal(t, "", joinedContent(events))
	for _, event := range events {
		if event.Type == "image" {
			require.Equal(t, "data:image/png;base64,QUJD", event.ImageURL)
			require.Equal(t, "image/png", event.MimeType)
		}
	}
}

func TestProcessGeminiStream_ThoughtOnlyFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"thought":true,"text":"pondering"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-pro"}`)

	err := svc.processGeminiStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text or image output")
	require.Equal(t, "", joinedContent(events), "reasoning text must not be surfaced as content")
	// The raw echo is still observed even though the probe failed.
	require.Len(t, metadataEvents(events), 1)
}

func TestProcessGeminiStream_EmptyStreamFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processGeminiStream(c, strings.NewReader(""))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended without any text or image output")
	require.Empty(t, metadataEvents(events), "no model echo means no metadata event")
}

func TestProcessGeminiStream_PrematureEOFFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	// Text arrived but the upstream never declared finishReason or [DONE].
	stream := sseData(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`)

	err := svc.processGeminiStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended before completion")
	require.Equal(t, "partial", joinedContent(events), "deltas still stream incrementally before the failure")
}

func TestProcessGeminiStream_ChangedModelEchoReportsConflict(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"modelVersion":"gemini-2.5-flash"}`) +
		sseData(`{"candidates":[{"content":{"parts":[{"text":"b"}]}}],"modelVersion":"gemini-2.5-pro"}`) +
		sseData(`[DONE]`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)

	meta := metadataEvents(events)
	require.Len(t, meta, 2, "a changed echo must emit a new metadata event: %v", eventTypes(events))
	require.Equal(t, "gemini-2.5-flash", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
	require.Equal(t, "gemini-2.5-pro", meta[1].UpstreamResponseModel)
	require.True(t, meta[1].UpstreamResponseModelConflict)
}

func TestProcessGeminiStream_MissingModelEchoEmitsNoMetadata(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Empty(t, metadataEvents(events))
}

func TestProcessGeminiStream_ErrorPayloadFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processGeminiStream(c, strings.NewReader(sseData(`{"error":{"message":"quota exceeded"}}`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "quota exceeded")
}

func TestProcessGeminiStream_MalformedFrameIsSkipped(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"candidates":`) + sseData(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)

	require.NoError(t, svc.processGeminiStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "ok", joinedContent(events))
}

func TestProcessGeminiStream_ReadErrorFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	body := &failingStreamReader{
		data: []byte(sseData(`{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`)),
		err:  context.Canceled,
	}

	err := svc.processGeminiStream(c, body)
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "Stream read error: context canceled")
}

// --- Claude ---

func TestProcessClaudeStream_TextAndMessageStopSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := "event: message_start\n" +
		sseData(`{"type":"message_start","message":{"model":"claude-sonnet-4-6"}}`) +
		"event: content_block_delta\n" +
		sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`) +
		"event: message_stop\n" +
		sseData(`{"type":"message_stop"}`)

	require.NoError(t, svc.processClaudeStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "Hello", joinedContent(events))

	meta := metadataEvents(events)
	require.Len(t, meta, 1)
	require.Equal(t, "claude-sonnet-4-6", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
}

func TestProcessClaudeStream_ThinkingOnlyFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`) +
		// A provider that mislabels reasoning text as "text" must not slip past
		// the delta-type guard either.
		sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","text":"secret"}}`) +
		sseData(`{"type":"message_stop"}`)

	err := svc.processClaudeStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
	require.Equal(t, "", joinedContent(events))
}

func TestProcessClaudeStream_EmptyStreamFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processClaudeStream(c, strings.NewReader(""))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended without any text output")
}

func TestProcessClaudeStream_PrematureEOFFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`)

	err := svc.processClaudeStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended before message_stop")
	require.Equal(t, "Hi", joinedContent(events))
}

func TestProcessClaudeStream_ErrorEventFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processClaudeStream(c, strings.NewReader(sseData(`{"type":"error","error":{"message":"overloaded"}}`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "overloaded")
}

func TestProcessClaudeStream_MessageStopWithoutTextFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processClaudeStream(c, strings.NewReader(sseData(`{"type":"message_stop"}`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
}

func TestProcessClaudeStream_MessageDeltaStopReasonIsATerminal(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	// Anthropic declares stop_reason on message_delta; this upstream omits the
	// trailing message_stop entirely.
	stream := sseData(`{"type":"message_start","message":{"model":"claude-sonnet-4-6"}}`) +
		sseData(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done"}}`) +
		sseData(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}`)

	require.NoError(t, svc.processClaudeStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "Done", joinedContent(events))
}

func TestProcessClaudeStream_MessageDeltaStopReasonWithoutTextFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)

	err := svc.processClaudeStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
}

func TestProcessClaudeStream_MessageDeltaWithoutStopReasonIsNotTerminal(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`) +
		sseData(`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":1}}`)

	err := svc.processClaudeStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended before message_stop")
	require.Equal(t, "Hi", joinedContent(events))
}

func TestProcessClaudeStream_ToolUseCountsAsUsableOutput(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"message_start","message":{"model":"claude-sonnet-4-6"}}`) +
		sseData(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`) +
		sseData(`{"type":"message_stop"}`)

	require.NoError(t, svc.processClaudeStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Contains(t, joinedContent(events), "tool_use")
	require.Contains(t, joinedContent(events), "get_weather")
}

func TestProcessClaudeStream_NonTextNonToolBlockIsNotUsable(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		sseData(`{"type":"message_stop"}`)

	err := svc.processClaudeStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
}

// --- OpenAI Chat Completions ---

func TestProcessOpenAIChatCompletionsStream_TextAndFinishSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`) +
		sseData(`{"model":"gpt-5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`) +
		sseData(`{"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		sseData(`[DONE]`)

	require.NoError(t, svc.processOpenAIChatCompletionsStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "Hello", joinedContent(events))
	require.Equal(t, 1, countEventType(events, "status"))

	meta := metadataEvents(events)
	require.Len(t, meta, 1, "unchanged echo must not re-emit metadata: %v", eventTypes(events))
	require.Equal(t, "gpt-5", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
}

func TestProcessOpenAIChatCompletionsStream_DoneOnlyFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(sseData(`[DONE]`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
	require.Zero(t, countEventType(events, "status"))
}

func TestProcessOpenAIChatCompletionsStream_FinishReasonWithoutTextFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}`)

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
}

func TestProcessOpenAIChatCompletionsStream_PrematureEOFFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"model":"gpt-5","choices":[{"index":0,"delta":{"content":"Hi"}}]}`)

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "ended before [DONE]")
	require.Equal(t, "Hi", joinedContent(events))
}

func TestProcessOpenAIChatCompletionsStream_InvalidJSONFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(sseData(`{oops}`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "expected JSON data")
}

func TestProcessOpenAIChatCompletionsStream_ErrorPayloadFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(sseData(`{"error":{"message":"bad key"}}`)))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "bad key")
}

func TestProcessOpenAIChatCompletionsStream_EmptyStreamFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processOpenAIChatCompletionsStream(c, strings.NewReader(""))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "expected SSE JSON data")
}

// --- OpenAI Responses ---

func TestProcessOpenAIStream_TextAndCompletedSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.created","response":{"model":"gpt-5"}}`) +
		sseData(`{"type":"response.output_text.delta","delta":"Hel"}`) +
		sseData(`{"type":"response.output_text.delta","delta":"lo"}`) +
		sseData(`{"type":"response.completed","response":{"model":"gpt-5"}}`)

	require.NoError(t, svc.processOpenAIStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "Hello", joinedContent(events))

	meta := metadataEvents(events)
	require.Len(t, meta, 1, "unchanged echo must not re-emit metadata: %v", eventTypes(events))
	require.Equal(t, "gpt-5", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
}

func TestProcessOpenAIStream_ReasoningOnlyCompletionFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`) +
		sseData(`{"type":"response.completed","response":{"model":"gpt-5"}}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "completed without any text output")
	require.Equal(t, "", joinedContent(events))
}

func TestProcessOpenAIStream_IncompleteTerminalFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.incomplete","response":{"model":"gpt-5","incomplete_details":{"reason":"max_output_tokens"}}}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "incomplete before response.completed")
	for _, event := range events {
		if event.Type == "error" {
			require.Contains(t, event.Error, "max_output_tokens")
		}
	}
}

func TestProcessOpenAIStream_CancelledTerminalFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.output_text.delta","delta":"partial"}`) +
		sseData(`{"type":"response.cancelled","response":{"model":"gpt-5"}}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "was cancelled")
	require.Equal(t, "partial", joinedContent(events))
}

func TestProcessOpenAIStream_EOFFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.output_text.delta","delta":"partial"}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "Stream ended before response.completed")
	require.Equal(t, "partial", joinedContent(events))
}

func TestProcessOpenAIStream_DoneSentinelSucceeds(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.output_text.delta","delta":"done-text"}`) + sseData(`[DONE]`)

	require.NoError(t, svc.processOpenAIStream(c, strings.NewReader(stream)))
	events := captureTestEvents(t, recorder.Body.String())
	requireSuccess(t, events, nil)
	require.Equal(t, "done-text", joinedContent(events))
}

func TestProcessOpenAIStream_ChangedTerminalModelReportsConflict(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.created","response":{"model":"gpt-5"}}`) +
		sseData(`{"type":"response.completed","response":{"model":"gpt-5-mini"}}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	// The terminal event carried no text, so the probe fails, but the changed
	// echo is still reported.
	requireFailure(t, events, err, "completed without any text output")

	meta := metadataEvents(events)
	require.Len(t, meta, 2)
	require.Equal(t, "gpt-5", meta[0].UpstreamResponseModel)
	require.False(t, meta[0].UpstreamResponseModelConflict)
	require.Equal(t, "gpt-5-mini", meta[1].UpstreamResponseModel)
	require.True(t, meta[1].UpstreamResponseModelConflict)
}

func TestProcessOpenAIStream_FailedEventFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	stream := sseData(`{"type":"response.failed","response":{"error":{"message":"upstream boom"}}}`)

	err := svc.processOpenAIStream(c, strings.NewReader(stream))
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "upstream boom")
}

func TestProcessOpenAIStream_ReadErrorFails(t *testing.T) {
	t.Parallel()
	c, recorder := newTestContext()
	svc := &AccountTestService{}

	body := &failingStreamReader{
		data: []byte(sseData(`{"type":"response.output_text.delta","delta":"x"}`)),
		err:  context.Canceled,
	}

	err := svc.processOpenAIStream(c, body)
	events := captureTestEvents(t, recorder.Body.String())
	requireFailure(t, events, err, "Stream read error: context canceled")
}
