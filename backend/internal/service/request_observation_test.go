package service

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newRequestObservationTestContext(t *testing.T, start time.Time) (*gin.Context, *requestObservation, *time.Time) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "gateway-request")
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-request")
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	BeginOpenAIRequestObservation(c, start)
	observer := requestObservationFromGin(c)
	now := start
	observer.now = func() time.Time { return now }
	return c, observer, &now
}

func TestRequestObservationUsageOnlyCompletionIsSuccessfulButNotSemantic(t *testing.T) {
	start := time.Unix(100, 0)
	c, _, now := newRequestObservationTestContext(t, start)
	BeginRequestObservationSelection(c)
	*now = start.Add(10 * time.Millisecond)
	EndRequestObservationSelection(c)
	sequence := BeginRequestObservationAttempt(c, 7, PlatformOpenAI)
	observer := beginUpstreamResponseModelObservation(c)
	*now = start.Add(50 * time.Millisecond)
	observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":1}}}`), "response.completed")
	result := &OpenAIForwardResult{RequestID: "upstream-request", Stream: true, Duration: 40 * time.Millisecond}
	FinishRequestObservationAttempt(c, sequence, result, nil, result.Duration)

	snapshot := SnapshotRequestObservation(c)
	require.NotNil(t, snapshot)
	require.False(t, snapshot.SemanticOutputSeen)
	require.Nil(t, snapshot.FirstVisibleOutputMs)
	require.Equal(t, "response.completed", snapshot.TerminalKind)
	require.Equal(t, 1, snapshot.AttemptCount)
	require.NotNil(t, snapshot.AttemptLedger)
	require.Equal(t, "success", snapshot.AttemptLedger.Attempts[0].Outcome)
}

func TestRequestObservationSevenAttemptsCoverFailedForwardsAndWaits(t *testing.T) {
	start := time.Unix(200, 0)
	c, _, now := newRequestObservationTestContext(t, start)
	var finalDuration time.Duration
	for attempt := 1; attempt <= 7; attempt++ {
		BeginRequestObservationSelection(c)
		*now = now.Add(5 * time.Millisecond)
		EndRequestObservationSelection(c)
		RecordRequestObservationSlotWait(c, 2*time.Millisecond)
		sequence := BeginRequestObservationAttempt(c, int64(attempt), PlatformOpenAI)
		beginUpstreamResponseModelObservation(c)
		forwardDuration := time.Duration(attempt*10) * time.Millisecond
		*now = now.Add(forwardDuration)
		if attempt < 7 {
			failover := &UpstreamFailoverError{StatusCode: 503, Stage: GatewayFailureStageInference, Scope: GatewayFailureScopeProvider}
			FinishRequestObservationAttempt(c, sequence, nil, failover, forwardDuration)
			if attempt%2 == 0 {
				RecordRequestObservationRetryWait(c, 25*time.Millisecond)
				*now = now.Add(25 * time.Millisecond)
			} else {
				RecordRequestObservationAccountSwitch(c)
				*now = now.Add(3 * time.Millisecond)
			}
			continue
		}
		finalDuration = forwardDuration
		upstream := upstreamResponseModelObserverFromContext(c)
		upstream.ObserveOpenAI([]byte(`{"type":"response.output_text.delta","delta":"ok"}`), "response.output_text.delta")
		upstream.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"output":[]}}`), "response.completed")
		FinishRequestObservationAttempt(c, sequence, &OpenAIForwardResult{RequestID: "final", Stream: true, Duration: forwardDuration}, nil, forwardDuration)
	}

	snapshot := SnapshotRequestObservation(c)
	require.Equal(t, 7, snapshot.AttemptCount)
	require.Equal(t, 3, snapshot.AccountSwitchCount)
	require.Equal(t, 210, snapshot.FailedAttemptDurationMs)
	require.Equal(t, 75, snapshot.RetryWaitMs)
	require.Greater(t, snapshot.HandlerDurationMs, int(finalDuration.Milliseconds()))
	require.True(t, snapshot.SemanticOutputSeen)
	require.NotNil(t, snapshot.FirstVisibleOutputMs)
	require.Len(t, snapshot.AttemptLedger.Attempts, 7)
}

func TestRequestObservationNormalSemanticSuccessOmitsLedger(t *testing.T) {
	start := time.Unix(300, 0)
	c, _, now := newRequestObservationTestContext(t, start)
	sequence := BeginRequestObservationAttempt(c, 1, PlatformOpenAI)
	observer := beginUpstreamResponseModelObservation(c)
	*now = start.Add(12 * time.Millisecond)
	observer.ObserveOpenAI([]byte(`{"id":"chatcmpl","choices":[{"delta":{"content":"hello"}}]}`), "chat.completion.chunk")
	FinishRequestObservationAttempt(c, sequence, &OpenAIForwardResult{Stream: true}, nil, 12*time.Millisecond)

	snapshot := SnapshotRequestObservation(c)
	require.True(t, snapshot.SemanticOutputSeen)
	require.NotNil(t, snapshot.FirstVisibleOutputMs)
	require.Equal(t, 12, *snapshot.FirstVisibleOutputMs)
	require.Nil(t, snapshot.AttemptLedger)
}

func TestRequestObservationWithoutProtocolEventStaysUnknown(t *testing.T) {
	start := time.Unix(350, 0)
	c, _, _ := newRequestObservationTestContext(t, start)
	sequence := BeginRequestObservationAttempt(c, 1, PlatformOpenAI)
	FinishRequestObservationAttempt(c, sequence, &OpenAIForwardResult{Stream: false}, nil, 10*time.Millisecond)
	require.Nil(t, SnapshotRequestObservation(c), "uninstalled/unreached protocol observers must persist NULL, not semantic=false")
}

func TestRequestObservationSemanticClassifiers(t *testing.T) {
	openAICases := []struct {
		name, event, payload string
		want                 bool
	}{
		{"preamble", "response.created", `{"type":"response.created","response":{"id":"r"}}`, false},
		{"empty delta", "response.output_text.delta", `{"type":"response.output_text.delta","delta":""}`, false},
		{"usage only", "response.completed", `{"type":"response.completed","response":{"usage":{"input_tokens":1}}}`, false},
		{"text", "response.output_text.delta", `{"type":"response.output_text.delta","delta":"hello"}`, true},
		{"reasoning", "response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`, true},
		{"tool call", "response.output_item.added", `{"type":"response.output_item.added","item":{"type":"function_call","name":"lookup"}}`, true},
		{"image", "response.image_generation_call.partial_image", `{"type":"response.image_generation_call.partial_image","partial_image_b64":"abc"}`, true},
		{"chat content", "chat.completion.chunk", `{"object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`, true},
		{"legacy streamed function call", "chat.completion.chunk", `{"object":"chat.completion.chunk","choices":[{"delta":{"function_call":{"name":"lookup","arguments":""}}}]}`, true},
		{"legacy buffered function call", "", `{"object":"chat.completion","choices":[{"message":{"role":"assistant","function_call":{"name":"lookup","arguments":"{}"}}}]}`, true},
	}
	for _, test := range openAICases {
		t.Run("openai "+test.name, func(t *testing.T) {
			require.Equal(t, test.want, openAIObservationHasSemanticOutput([]byte(test.payload), test.event))
		})
	}

	anthropicCases := []struct {
		name, payload string
		want          bool
	}{
		{"message start", `{"type":"message_start","message":{"content":[]}}`, false},
		{"empty delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`, false},
		{"text delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, true},
		{"tool start", `{"type":"content_block_start","content_block":{"type":"tool_use","name":"lookup"}}`, true},
	}
	for _, test := range anthropicCases {
		t.Run("anthropic "+test.name, func(t *testing.T) {
			require.Equal(t, test.want, anthropicObservationHasSemanticOutput([]byte(test.payload)))
		})
	}
}

func TestRequestObservationLegacyFunctionCallFirstVisible(t *testing.T) {
	tests := []struct {
		name      string
		stream    bool
		eventType string
		payload   string
		visibleMs int
	}{
		{
			name:      "streamed delta",
			stream:    true,
			eventType: "chat.completion.chunk",
			payload:   `{"object":"chat.completion.chunk","choices":[{"delta":{"function_call":{"name":"lookup","arguments":""}}}]}`,
			visibleMs: 17,
		},
		{
			name:      "buffered message",
			stream:    false,
			payload:   `{"object":"chat.completion","choices":[{"message":{"role":"assistant","function_call":{"name":"lookup","arguments":"{}"}}}]}`,
			visibleMs: 23,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start := time.Unix(400, 0)
			c, _, now := newRequestObservationTestContext(t, start)
			sequence := BeginRequestObservationAttempt(c, 1, PlatformOpenAI)
			observer := beginUpstreamResponseModelObservation(c)
			*now = start.Add(time.Duration(test.visibleMs) * time.Millisecond)
			observer.ObserveOpenAI([]byte(test.payload), test.eventType)
			FinishRequestObservationAttempt(c, sequence, &OpenAIForwardResult{Stream: test.stream}, nil, time.Duration(test.visibleMs)*time.Millisecond)

			snapshot := SnapshotRequestObservation(c)
			require.NotNil(t, snapshot)
			require.True(t, snapshot.SemanticOutputSeen)
			require.NotNil(t, snapshot.FirstVisibleOutputMs)
			require.Equal(t, test.visibleMs, *snapshot.FirstVisibleOutputMs)
			require.Nil(t, snapshot.AttemptLedger, "normal single-attempt semantic success must not retain a ledger")
		})
	}
}

func TestRequestAttemptLedgerFoldsAndContainsNoRawEvidence(t *testing.T) {
	attempts := make([]RequestAttemptEvidence, 40)
	for i := range attempts {
		attempts[i] = RequestAttemptEvidence{
			Sequence:          i + 1,
			AccountID:         int64(i + 1),
			Platform:          "openai",
			ForwardMs:         i + 10,
			Outcome:           "failover",
			Reason:            "http_503",
			UpstreamRequestID: strings.Repeat("r", 128),
		}
	}
	ledger := buildRequestAttemptLedger(attempts)
	require.True(t, ledger.Truncated)
	require.Len(t, ledger.Attempts, 32)
	require.Equal(t, 8, ledger.Folded.Count)
	require.Equal(t, 1, ledger.Attempts[0].Sequence)
	require.Equal(t, 40, ledger.Attempts[len(ledger.Attempts)-1].Sequence)

	payload, err := json.Marshal(ledger)
	require.NoError(t, err)
	for _, forbidden := range []string{"Authorization", "Bearer secret", "prompt", "query", "raw_error"} {
		require.NotContains(t, string(payload), forbidden)
	}
}
