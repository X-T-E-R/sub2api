package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpsSystemLogSink_ShouldIndex(t *testing.T) {
	sink := &OpsSystemLogSink{}

	cases := []struct {
		name  string
		event *logger.LogEvent
		want  bool
	}{
		{
			name:  "warn level",
			event: &logger.LogEvent{Level: "warn", Component: "app"},
			want:  true,
		},
		{
			name:  "error level",
			event: &logger.LogEvent{Level: "error", Component: "app"},
			want:  true,
		},
		{
			name:  "access component",
			event: &logger.LogEvent{Level: "info", Component: "http.access"},
			want:  true,
		},
		{
			name: "rejected access excluded from database sink",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "http.access",
				Fields:    map[string]any{logger.OpsSystemLogSkipField: true},
			},
			want: false,
		},
		{
			name: "access component from fields (real zap path)",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "",
				Fields:    map[string]any{"component": "http.access"},
			},
			want: true,
		},
		{
			name:  "audit component",
			event: &logger.LogEvent{Level: "info", Component: "audit.log_config_change"},
			want:  true,
		},
		{
			name: "audit component from fields (real zap path)",
			event: &logger.LogEvent{
				Level:     "info",
				Component: "",
				Fields:    map[string]any{"component": "audit.log_config_change"},
			},
			want: true,
		},
		{name: "exact summary Info", event: &logger.LogEvent{Level: "info", Component: "service.antigravity_gateway", Message: "antigravity.response_summary"}, want: true},
		{name: "summary component from zap fields", event: &logger.LogEvent{Level: "info", Component: "slog", Message: "antigravity.response_summary", Fields: map[string]any{"component": "service.antigravity_gateway"}}, want: true},
		{name: "unrelated service Info", event: &logger.LogEvent{Level: "info", Component: "service.antigravity_gateway", Message: "ordinary info"}},
		{name: "wrong component summary Info", event: &logger.LogEvent{Level: "info", Component: "service.other", Message: "antigravity.response_summary"}},
		{name: "near message prefix", event: &logger.LogEvent{Level: "info", Component: "service.antigravity_gateway", Message: "antigravity.response_summary.extra"}},
		{name: "near component prefix", event: &logger.LogEvent{Level: "info", Component: "service.antigravity_gateway.extra", Message: "antigravity.response_summary"}},
		{name: "summary debug still skipped", event: &logger.LogEvent{Level: "debug", Component: "service.antigravity_gateway", Message: "antigravity.response_summary"}},
		{name: "explicit summary skip wins", event: &logger.LogEvent{Level: "info", Component: "service.antigravity_gateway", Message: "antigravity.response_summary", Fields: map[string]any{logger.OpsSystemLogSkipField: true}}},
		{
			name:  "plain info",
			event: &logger.LogEvent{Level: "info", Component: "app"},
			want:  false,
		},
	}

	for _, tc := range cases {
		if got := sink.shouldIndex(tc.event); got != tc.want {
			t.Fatalf("%s: shouldIndex()=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOpsSystemLogSink_WriteLogEvent_ShouldDropWhenQueueFull(t *testing.T) {
	sink := &OpsSystemLogSink{
		queue: make(chan *logger.LogEvent, 1),
	}

	sink.WriteLogEvent(&logger.LogEvent{Level: "warn", Component: "app"})
	sink.WriteLogEvent(&logger.LogEvent{Level: "warn", Component: "app"})

	if got := len(sink.queue); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	if dropped := atomic.LoadUint64(&sink.droppedCount); dropped != 1 {
		t.Fatalf("droppedCount = %d, want 1", dropped)
	}
}

func TestOpsSystemLogSink_Health(t *testing.T) {
	sink := &OpsSystemLogSink{
		queue: make(chan *logger.LogEvent, 10),
	}
	sink.lastError.Store("db timeout")
	atomic.StoreUint64(&sink.droppedCount, 3)
	atomic.StoreUint64(&sink.writeFailed, 2)
	atomic.StoreUint64(&sink.writtenCount, 5)
	atomic.StoreUint64(&sink.totalDelayNs, uint64(5000000)) // 5ms total -> avg 1ms
	sink.queue <- &logger.LogEvent{Level: "warn", Component: "app"}
	sink.queue <- &logger.LogEvent{Level: "warn", Component: "app"}

	health := sink.Health()
	if health.QueueDepth != 2 {
		t.Fatalf("queue depth = %d, want 2", health.QueueDepth)
	}
	if health.QueueCapacity != 10 {
		t.Fatalf("queue capacity = %d, want 10", health.QueueCapacity)
	}
	if health.DroppedCount != 3 {
		t.Fatalf("dropped = %d, want 3", health.DroppedCount)
	}
	if health.WriteFailed != 2 {
		t.Fatalf("write failed = %d, want 2", health.WriteFailed)
	}
	if health.WrittenCount != 5 {
		t.Fatalf("written = %d, want 5", health.WrittenCount)
	}
	if health.AvgWriteDelayMs != 1 {
		t.Fatalf("avg delay ms = %d, want 1", health.AvgWriteDelayMs)
	}
	if health.LastError != "db timeout" {
		t.Fatalf("last error = %q, want db timeout", health.LastError)
	}
}

func TestOpsSystemLogSink_StartStopAndFlushSuccess(t *testing.T) {
	done := make(chan struct{}, 1)
	var captured []*OpsInsertSystemLogInput
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			captured = append(captured, inputs...)
			select {
			case done <- struct{}{}:
			default:
			}
			return int64(len(inputs)), nil
		},
	}

	sink := NewOpsSystemLogSink(repo)
	sink.host = "api-node-1"
	sink.batchSize = 1
	sink.flushInterval = 10 * time.Millisecond
	sink.Start()
	defer sink.Stop()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "http.access",
		Message:   `authorization="Bearer sk-test-123"`,
		Fields: map[string]any{
			"component":         "http.access",
			"request_id":        "req-1",
			"client_request_id": "creq-1",
			"user_id":           "12",
			"api_key_id":        int64(56),
			"account_id":        json.Number("34"),
			"platform":          "openai",
			"model":             "gpt-5",
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for sink flush")
	}

	if len(captured) != 1 {
		t.Fatalf("captured len = %d, want 1", len(captured))
	}
	item := captured[0]
	if item.Host != "api-node-1" {
		t.Fatalf("host = %q, want api-node-1", item.Host)
	}
	if item.RequestID != "req-1" || item.ClientRequestID != "creq-1" {
		t.Fatalf("unexpected request ids: %+v", item)
	}
	if item.UserID == nil || *item.UserID != 12 {
		t.Fatalf("unexpected user_id: %+v", item.UserID)
	}
	if item.APIKeyID == nil || *item.APIKeyID != 56 {
		t.Fatalf("unexpected api_key_id: %+v", item.APIKeyID)
	}
	if item.AccountID == nil || *item.AccountID != 34 {
		t.Fatalf("unexpected account_id: %+v", item.AccountID)
	}
	if strings.TrimSpace(item.Message) == "" {
		t.Fatalf("message should not be empty")
	}
	// writtenCount is incremented after BatchInsertSystemLogsFn returns,
	// so poll briefly to avoid a race between the done signal and the atomic add.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sink.Health().WrittenCount > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	health := sink.Health()
	if health.WrittenCount == 0 {
		t.Fatalf("written_count should be >0")
	}
}

func TestOpsSystemLogSink_FlushFailureUpdatesHealth(t *testing.T) {
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			return 0, errors.New("db unavailable")
		},
	}
	sink := NewOpsSystemLogSink(repo)
	sink.batchSize = 1
	sink.flushInterval = 10 * time.Millisecond
	sink.Start()
	defer sink.Stop()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "app",
		Message:   "boom",
		Fields:    map[string]any{},
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		health := sink.Health()
		if health.WriteFailed > 0 {
			if !strings.Contains(health.LastError, "db unavailable") {
				t.Fatalf("unexpected last error: %s", health.LastError)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("write_failed_count not updated")
}

func TestOpsSystemLogSink_StopFlushUsesActiveContextAndDrainsQueue(t *testing.T) {
	var inserted int64
	var canceledCtxCalls int64
	repo := &opsRepoMock{
		BatchInsertSystemLogsFn: func(ctx context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
			if err := ctx.Err(); err != nil {
				atomic.AddInt64(&canceledCtxCalls, 1)
				return 0, err
			}
			atomic.AddInt64(&inserted, int64(len(inputs)))
			return int64(len(inputs)), nil
		},
	}

	sink := NewOpsSystemLogSink(repo)
	sink.batchSize = 200
	sink.flushInterval = time.Hour
	sink.Start()

	sink.WriteLogEvent(&logger.LogEvent{
		Time:      time.Now().UTC(),
		Level:     "warn",
		Component: "app",
		Message:   "pending-on-shutdown",
		Fields:    map[string]any{"component": "http.access"},
	})

	sink.Stop()

	if got := atomic.LoadInt64(&inserted); got != 1 {
		t.Fatalf("inserted = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&canceledCtxCalls); got != 0 {
		t.Fatalf("canceled ctx calls = %d, want 0", got)
	}
	health := sink.Health()
	if health.WrittenCount != 1 {
		t.Fatalf("written_count = %d, want 1", health.WrittenCount)
	}
}

type stringerValue string

func (s stringerValue) String() string { return string(s) }

func TestOpsSystemLogSink_HelperFunctions(t *testing.T) {
	src := map[string]any{"a": 1}
	cloned := copyMap(src)
	src["a"] = 2
	v, ok := cloned["a"].(int)
	if !ok || v != 1 {
		t.Fatalf("copyMap should create copy")
	}
	if got := asString(stringerValue(" hello ")); got != "hello" {
		t.Fatalf("asString stringer = %q", got)
	}
	if got := asString(fmt.Errorf("x")); got != "" {
		t.Fatalf("asString error should be empty, got %q", got)
	}
	if got := asString(123); got != "" {
		t.Fatalf("asString non-string should be empty, got %q", got)
	}

	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{in: 5, want: 5, ok: true},
		{in: int64(6), want: 6, ok: true},
		{in: float64(7), want: 7, ok: true},
		{in: json.Number("8"), want: 8, ok: true},
		{in: "9", want: 9, ok: true},
		{in: "0", ok: false},
		{in: -1, ok: false},
		{in: "abc", ok: false},
	}
	for _, tc := range cases {
		got := asInt64Ptr(tc.in)
		if tc.ok {
			if got == nil || *got != tc.want {
				t.Fatalf("asInt64Ptr(%v) = %+v, want %d", tc.in, got, tc.want)
			}
		} else if got != nil {
			t.Fatalf("asInt64Ptr(%v) should be nil, got %d", tc.in, *got)
		}
	}
}

func TestNormalizeSystemLogHost(t *testing.T) {
	if got := normalizeSystemLogHost(" api-node-1 ", nil); got != "api-node-1" {
		t.Fatalf("trimmed host = %q, want api-node-1", got)
	}
	if got := normalizeSystemLogHost("", nil); got != "unknown" {
		t.Fatalf("empty host = %q, want unknown", got)
	}
	if got := normalizeSystemLogHost("api-node-1", errors.New("hostname unavailable")); got != "unknown" {
		t.Fatalf("errored host = %q, want unknown", got)
	}
	longHost := strings.Repeat("节", maxSystemLogHostLength+1)
	got := normalizeSystemLogHost(longHost, nil)
	if runeCount := len([]rune(got)); runeCount != maxSystemLogHostLength {
		t.Fatalf("truncated host rune count = %d, want %d", runeCount, maxSystemLogHostLength)
	}
}

func TestOpsSystemLogSink_AntigravitySummaryIndexAndFlushProjection(t *testing.T) {
	var captured []*OpsInsertSystemLogInput
	repo := &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		captured = append(captured, inputs...)
		return int64(len(inputs)), nil
	}}
	sink := NewOpsSystemLogSink(repo)
	sink.flushInterval = time.Hour
	for _, partial := range []bool{false, true} {
		core, logs := observer.New(zapcore.InfoLevel)
		// Production handlers bind model/platform through typed context keys;
		// the ingress logger itself need not contain either field.
		ctx := logger.IntoContext(context.Background(), zap.New(core))
		ctx = context.WithValue(ctx, ctxkey.Model, "gemini-3.8-flash-high")
		ctx = context.WithValue(ctx, ctxkey.Platform, PlatformAntigravity)
		id := "success-public-id"
		if partial {
			id = "recovery-public-id"
		}
		ctx = context.WithValue(ctx, ctxkey.RequestID, id)
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-"+id)
		part := `{"text":"private-body-sentinel"}`
		if partial {
			part = `{"text":"private-thinking-sentinel","thought":true,"thoughtSignature":"private-signature-sentinel","functionCall":null}`
		}
		line := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[` + part + `]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"thoughtsTokenCount":2}}}`
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(line)}}
		svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{SuppressThoughtOnlyStop: true}, upstream)
		body, c, _ := newAntigravityThoughtClaudeRequest(true)
		c.Request = c.Request.WithContext(ctx)
		_, err := svc.Forward(ctx, c, antigravityThoughtAccount(nil), body, false)
		if partial {
			require.True(t, IsAntigravityNoUsableAnswerError(err))
		} else {
			require.NoError(t, err)
		}
		entries := logs.FilterMessage("antigravity.response_summary").All()
		require.Len(t, entries, 1)
		entry := entries[0]
		sink.WriteLogEvent(&logger.LogEvent{Time: entry.Time, Level: "info", Component: entry.LoggerName, Message: entry.Message, Fields: entry.ContextMap()})
	}
	sink.WriteLogEvent(&logger.LogEvent{Level: "info", Component: "service.antigravity_gateway", Message: "ordinary service info"})
	require.Len(t, sink.queue, 2, "only the two exact summary Info events enter the index queue")
	sink.Start()
	sink.Stop() // synchronous shutdown flush; no timer or polling.
	require.Len(t, captured, 2)
	for i, row := range captured {
		require.Equal(t, "info", row.Level)
		require.Equal(t, "service.antigravity_gateway", row.Component)
		require.Equal(t, "antigravity.response_summary", row.Message)
		require.Equal(t, "gemini-3.8-flash-high", row.Model)
		require.Equal(t, PlatformAntigravity, row.Platform)
		require.NotNil(t, row.AccountID)
		require.Equal(t, int64(3757), *row.AccountID)
		id := "success-public-id"
		decision := "skipped"
		reason := "visible_content"
		think, text := 0, 1
		if i == 1 {
			id = "recovery-public-id"
			decision = "explicit_error"
			reason = "thinking_only"
			think, text = 1, 0
		}
		require.Equal(t, id, row.RequestID)
		require.Equal(t, "client-"+id, row.ClientRequestID)
		var extra map[string]any
		require.NoError(t, json.Unmarshal([]byte(row.ExtraJSON), &extra))
		require.Equal(t, decision, extra["a_decision"])
		require.Equal(t, reason, extra["a_reason"])
		require.EqualValues(t, think, extra["downstream_thinking_events"])
		require.EqualValues(t, text, extra["downstream_text_events"])
		require.EqualValues(t, 0, extra["downstream_tool_events"])
		require.Equal(t, "STOP", extra["upstream_finish_reason"])
		require.Equal(t, true, extra["effective_a"])
		require.Equal(t, false, extra["effective_b"])
		require.Equal(t, "messages", extra["protocol"])
		all, err := json.Marshal(row)
		require.NoError(t, err)
		for _, secret := range []string{"private-body-sentinel", "private-thinking-sentinel", "private-signature-sentinel"} {
			require.NotContains(t, string(all), secret)
		}
	}
	require.EqualValues(t, 2, sink.Health().WrittenCount)
	// No context model/IDs means no indexed model/IDs: never guess from model
	// mapping, account name, a response ID or another request.
	emptyRepo := &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		require.Empty(t, inputs[0].RequestID)
		require.Empty(t, inputs[0].ClientRequestID)
		require.Empty(t, inputs[0].Model)
		return 1, nil
	}}
	_, missingContext, _ := newAntigravityThoughtClaudeRequest(true)
	missingSummary := (&antigravityThoughtContinuation{account: antigravityThoughtAccount(nil), generation: newAntigravityThoughtGeneration()}).summaryFields(missingContext)
	for _, key := range []string{"request_id", "client_request_id", "model", "platform"} {
		require.NotContains(t, missingSummary, key, "missing context metadata is not inferred")
	}
	_, err := NewOpsSystemLogSink(emptyRepo).flushBatch(context.Background(), []*logger.LogEvent{{Level: "info", Component: "service.antigravity_gateway", Message: "antigravity.response_summary", Fields: missingSummary}})
	require.NoError(t, err)
}
