package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codextelemetry"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestCodexTelemetryDisabledAndAttemptBoundaries(t *testing.T) {
	c, _ := telemetryContext(false)
	account := telemetryAccount("http://127.0.0.1:1")
	require.Nil(t, beginCodexHTTPAttempt(c, account))
	require.Nil(t, codexTelemetryAttemptFromContext(c))
	SetCodexTelemetryCapturePolicy(c, func() bool { return true })
	beginUpstreamResponseModelObservation(c)
	first := beginCodexHTTPAttempt(c, account)
	for _, event := range telemetryFrames("resp-one") {
		first.observe([]byte(event), gjsonType(event))
	}
	wrongRow := &OpenAIForwardResult{OpenAIWSMode: true, RequestID: "another-response"}
	first.finish(wrongRow, nil)
	require.Nil(t, wrongRow.CodexTelemetry, "do not copy telemetry into a mismatched native result")
	account.ID++
	second := beginCodexHTTPAttempt(c, account)
	result := &OpenAIForwardResult{}
	second.finish(result, nil)
	require.Nil(t, result.CodexTelemetry)
	require.Contains(t, string(codextelemetry.Marshal(first.snapshot())), "opaque-engine")
	ClearOpsUpstreamModel(c)
	require.Nil(t, codexTelemetryAttemptFromContext(c))
}

func gjsonType(raw string) string {
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	return envelope.Type
}

func TestCodexTelemetryOpsOnlyEnrichesAdmittedFailureAndOwnsData(t *testing.T) {
	c, _ := telemetryContext(true)
	account := telemetryAccount("http://127.0.0.1:1")
	attempt := beginCodexTelemetryAttempt(c, account, codextelemetry.WebSocket, false)
	attempt.observe([]byte(`{"type":"error","status":429,"headers":{"x-codex-primary-used-percent":"100","x-codex-safety-buffering-faster-model":"pending-hint"},"error":{"code":"rate_limit_exceeded"}}`), "error")
	entry := &OpsInsertErrorLogInput{StatusCode: 429, AccountID: &account.ID}
	AttachCodexTelemetryToOpsEntry(c, entry)
	require.Len(t, entry.UpstreamErrors, 1)
	require.Equal(t, codextelemetry.UpstreamAttempt, entry.UpstreamErrors[0].CodexTelemetry.Observations[0].Association)
	_, mutated := c.Get(OpsUpstreamErrorsKey)
	require.False(t, mutated, "enrichment must not create recovered-error logging eligibility")
	AttachCodexTelemetryToOpsEntry(c, entry)
	require.Len(t, entry.UpstreamErrors, 1, "the same admitted attempt is not duplicated")
	*entry.UpstreamErrors[0].CodexTelemetry.Observations[0].PrimaryUsedPercent = 1
	require.Equal(t, 100.0, *attempt.snapshot().Observations[0].PrimaryUsedPercent)

	account.ID++
	beginCodexHTTPAttempt(c, account).headers(http.Header{"X-Codex-Active-Limit": {"new-account"}}, codextelemetry.HTTPHeaders, 200)
	success := &OpsInsertErrorLogInput{StatusCode: 200}
	AttachCodexTelemetryToOpsEntry(c, success)
	require.Empty(t, success.UpstreamErrors, "successful capture alone is not an Ops error")

	oldSnapshot := SnapshotOpsUpstreamErrors(c)
	require.Nil(t, oldSnapshot)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, UpstreamStatusCode: 503, Message: "synthetic"})
	frozen := SnapshotOpsUpstreamErrors(c)
	require.Len(t, frozen, 1)
	stored, _ := c.Get(OpsUpstreamErrorsKey)
	stored.([]*OpsUpstreamErrorEvent)[0].CodexTelemetry.Observations[0].ActiveLimit = "later-mutation"
	require.Equal(t, "new-account", frozen[0].CodexTelemetry.Observations[0].ActiveLimit)
}

func TestCodexTelemetryOpsRecordBudget(t *testing.T) {
	entry := &OpsInsertErrorLogInput{}
	for i := 0; i < 20; i++ {
		collector := codextelemetry.New(codextelemetry.HTTP, false)
		collector.ObserveHeaders(http.Header{"X-Codex-Active-Limit": {fmt.Sprintf("%03d%s", i, strings.Repeat("x", 120))}}, codextelemetry.HTTPHeaders)
		entry.UpstreamErrors = append(entry.UpstreamErrors, &OpsUpstreamErrorEvent{AccountID: 1, UpstreamStatusCode: 429, Message: "synthetic", CodexTelemetry: collector.Snapshot()})
	}
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	var events []*OpsUpstreamErrorEvent
	require.NoError(t, json.Unmarshal([]byte(*entry.UpstreamErrorsJSON), &events))
	require.Len(t, events, 16, "existing Ops event cap is unchanged")
	bytes := 0
	for _, event := range events {
		if event.CodexTelemetry != nil {
			raw, err := json.Marshal(event.CodexTelemetry)
			require.NoError(t, err)
			bytes += len(raw) + len(`,"codex_telemetry":`)
		}
	}
	require.LessOrEqual(t, bytes, codextelemetry.MaxSnapshotBytes)
	require.Contains(t, *entry.UpstreamErrorsJSON, "019")
	require.True(t, events[len(events)-1].CodexTelemetry.Truncated)
	t.Logf("all telemetry additions in one Ops row: %d bytes", bytes)
}

func TestCodexTelemetryFrameConnIdleLatePrewarmAndSnapshots(t *testing.T) {
	c, _ := telemetryContext(true)
	upstream := newStagedPassthroughConn()
	defer upstream.Close()
	conn := &codexTelemetryFrameConn{inner: upstream, c: c, account: telemetryAccount("http://127.0.0.1:1"), headers: http.Header{"X-Codex-Active-Limit": {"handshake"}}}
	ctx := context.Background()
	observe := func(frame string) {
		upstream.Send(frame)
		_, got, err := conn.ReadFrame(ctx)
		require.NoError(t, err)
		require.Equal(t, frame, string(got), "observer preserves native frame bytes")
	}
	observe(`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["idle"]}}`)
	require.Nil(t, conn.take(""))
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","generate":false}`)))
	for _, event := range telemetryFrames("prewarm") {
		observe(event)
	}
	require.Nil(t, conn.take("prewarm"))
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
	observe(`{"type":"responsesapi.websocket_timing","response_id":"prewarm","timing_metrics":{"engine_ids":["late-prewarm"]}}`)
	for i, event := range telemetryFrames("business") {
		observe(event)
		if i == 0 {
			observe(`{"type":"responsesapi.websocket_timing","response_id":"prewarm","timing_metrics":{"engine_ids":["known-stale-during-active-response"]}}`)
			observe(`{"type":"response.completed","response":{"id":"prewarm"}}`)
		}
	}
	snapshot := conn.take("business")
	require.NotNil(t, snapshot)
	require.NotContains(t, string(codextelemetry.Marshal(snapshot)), "prewarm")
	require.NotContains(t, string(codextelemetry.Marshal(snapshot)), "handshake")
	observe(`{"type":"responsesapi.websocket_timing","timing_metrics":{"engine_ids":["after-terminal"]}}`)
	require.Nil(t, conn.take("business"), "duplicate settlement cannot reuse a previous snapshot")
	require.NotContains(t, string(codextelemetry.Marshal(snapshot)), "after-terminal")
}

func TestCodexTelemetryFrameConnAmbiguousHandshakeSettlement(t *testing.T) {
	c, _ := telemetryContext(true)
	upstream := newStagedPassthroughConn()
	defer upstream.Close()
	conn := &codexTelemetryFrameConn{inner: upstream, c: c, account: telemetryAccount("http://127.0.0.1:1"), headers: http.Header{"X-Codex-Active-Limit": {"real-handshake"}}}
	ctx := context.Background()
	observe := func(frame string) {
		upstream.Send(frame)
		_, got, err := conn.ReadFrame(ctx)
		require.NoError(t, err)
		require.Equal(t, frame, string(got))
	}
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp-one"}}`,
		`{"type":"responsesapi.websocket_timing","response_id":"resp-one","timing_metrics":{"engine_ids":["earlier-engine"]}}`,
		`{"type":"response.created","response":{"id":"overlapping-response"}}`,
		`{"type":"response.completed","response":{"id":"resp-one"}}`,
	} {
		observe(event)
	}
	snapshot := conn.take("resp-one")
	require.NotNil(t, snapshot)
	require.Empty(t, snapshot.ResponseID)
	require.Empty(t, snapshot.StreamID)
	require.Equal(t, []codextelemetry.Observation{{Source: codextelemetry.WSUpgradeHeaders, Association: codextelemetry.Connection, ActiveLimit: "real-handshake"}}, snapshot.Observations)
	require.Nil(t, conn.take("resp-one"), "the connection snapshot is settled only once")
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
	for _, event := range telemetryFrames("resp-next") {
		observe(event)
	}
	require.Nil(t, conn.take("unrelated-response"), "bound event evidence still requires the matching settlement")
	next := conn.take("resp-next")
	require.NotNil(t, next)
	require.True(t, next.ConnectionReused)
	require.Equal(t, "resp-next", next.ResponseID)
	require.NotContains(t, string(codextelemetry.Marshal(next)), "real-handshake")
	require.Contains(t, string(codextelemetry.Marshal(next)), "opaque-engine")
	require.Nil(t, conn.take("resp-next"))
}

func TestCodexTelemetryFrameConnDoesNotSettleUnboundEventAsConnectionHeader(t *testing.T) {
	c, _ := telemetryContext(true)
	upstream := newStagedPassthroughConn()
	defer upstream.Close()
	conn := &codexTelemetryFrameConn{inner: upstream, c: c, account: telemetryAccount("http://127.0.0.1:1"), headers: http.Header{"X-Codex-Active-Limit": {"real-handshake"}}}
	ctx := context.Background()
	require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
	upstream.Send(`{"type":"error","status":429,"headers":{"x-codex-active-limit":"pending-error"}}`)
	_, _, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.Nil(t, conn.take("unrelated-response"), "unbound error-event data does not qualify as real connection headers")
}

func TestCodexTelemetrySettingUsesExistingAtomicSnapshot(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	ops := &OpsService{settingRepo: repo}
	c, _ := telemetryContext(false)
	SetCodexTelemetryCapturePolicy(c, func() bool { return ops.OpsAdvancedSettingsSnapshot().CodexTelemetryEnabled })
	account := telemetryAccount("http://127.0.0.1:1")
	require.Nil(t, beginCodexHTTPAttempt(c, account))
	settings := defaultOpsAdvancedSettings()
	settings.CodexTelemetryEnabled = true
	_, err := ops.UpdateOpsAdvancedSettings(context.Background(), settings)
	require.NoError(t, err)
	first := beginCodexHTTPAttempt(c, account)
	require.NotNil(t, first)
	settings.CodexTelemetryEnabled = false
	_, err = ops.UpdateOpsAdvancedSettings(context.Background(), settings)
	require.NoError(t, err)
	first.headers(http.Header{"X-Codex-Active-Limit": {"in-flight"}}, codextelemetry.HTTPHeaders, 200)
	require.NotNil(t, first.snapshot(), "an already-started turn keeps its original policy")
	require.Nil(t, beginCodexHTTPAttempt(c, account), "disable applies to the next turn")
	require.Zero(t, repo.getValueCalls)
	require.Zero(t, repo.getMultipleCalls, "capture uses the existing atomic settings snapshot, never request-path I/O")
}

func TestCodexTelemetryRecordUsagePreservesModelsAndBilling(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	c, _ := telemetryContext(true)
	a := beginCodexHTTPAttempt(c, telemetryAccount("http://127.0.0.1:1"))
	a.headers(http.Header{"X-Codex-Safety-Buffering-Faster-Model": {"not-an-execution-model"}}, codextelemetry.HTTPHeaders, 200)
	result := &OpenAIForwardResult{RequestID: "record-telemetry", Model: "gpt-5.1", UpstreamModel: "mapped-model", UpstreamResponseModel: "returned-model", CodexTelemetry: a.snapshot(), Duration: time.Second}
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result, APIKey: &APIKey{ID: 1000, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 2000}, Account: &Account{ID: 3000, Type: AccountTypeAPIKey}})
	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, "gpt-5.1", usageRepo.lastLog.Model)
	require.Equal(t, "mapped-model", *usageRepo.lastLog.UpstreamModel)
	require.Equal(t, "returned-model", *usageRepo.lastLog.UpstreamResponseModel)
	require.True(t, usageRepo.lastLog.CodexTelemetryAvailable)
	require.Zero(t, billingRepo.lastCmd.BalanceCost, "the hint introduces no billing units")
	result.CodexTelemetry.Observations[0].FasterModel = "changed-after-submission"
	require.Equal(t, "not-an-execution-model", usageRepo.lastLog.CodexTelemetry.Observations[0].FasterModel)
}

func TestCodexTelemetryHandshakeIsFreshAndOneShot(t *testing.T) {
	newLease := func() *openAIWSConnLease {
		return &openAIWSConnLease{conn: newOpenAIWSConn("fixture", 1, nil, http.Header{"X-Codex-Active-Limit": {"handshake"}})}
	}
	lease := newLease()
	require.False(t, lease.codexTelemetryConnectionReused())
	require.Equal(t, "handshake", lease.takeCodexTelemetryHandshake(true).Get("X-Codex-Active-Limit"))
	require.Nil(t, lease.takeCodexTelemetryHandshake(true))
	require.True(t, lease.codexTelemetryConnectionReused())
	off := newLease()
	require.Nil(t, off.takeCodexTelemetryHandshake(false))
	require.Nil(t, off.takeCodexTelemetryHandshake(true), "enabling capture later must not reuse an old handshake")
	reused := newLease()
	reused.reused = true
	require.Nil(t, reused.takeCodexTelemetryHandshake(true))
	prewarm := newLease()
	prewarm.MarkPrewarmed()
	require.Nil(t, prewarm.takeCodexTelemetryHandshake(true))
}

func TestCodexTelemetrySameAccountAttemptsDoNotShareOpsEvidence(t *testing.T) {
	c, _ := telemetryContext(true)
	account := telemetryAccount("http://127.0.0.1:1")
	first := beginCodexHTTPAttempt(c, account)
	first.headers(http.Header{"X-Codex-Active-Limit": {"first-attempt"}}, codextelemetry.HTTPHeaders, 503)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, UpstreamStatusCode: 503, Message: "first failure"})
	second := beginCodexHTTPAttempt(c, account)
	second.headers(http.Header{"X-Codex-Active-Limit": {"second-attempt"}}, codextelemetry.HTTPHeaders, 429)
	entry := &OpsInsertErrorLogInput{AccountID: &account.ID, UpstreamErrors: SnapshotOpsUpstreamErrors(c)}
	AttachCodexTelemetryToOpsEntry(c, entry)
	require.Len(t, entry.UpstreamErrors, 2)
	require.Equal(t, "first-attempt", entry.UpstreamErrors[0].CodexTelemetry.Observations[0].ActiveLimit)
	require.Equal(t, "second-attempt", entry.UpstreamErrors[1].CodexTelemetry.Observations[0].ActiveLimit)
	third := beginCodexHTTPAttempt(c, account)
	third.headers(http.Header{"X-Codex-Active-Limit": {"successful-turn"}}, codextelemetry.HTTPHeaders, 200)
	third.observe([]byte(`{"type":"response.completed","response":{"id":"resp-success"}}`), "response.completed")
	third.finish(nil, io.EOF)
	AttachCodexTelemetryToOpsEntry(c, entry)
	require.Len(t, entry.UpstreamErrors, 2, "socket shutdown after a successful terminal is not a new failed attempt")
	require.Equal(t, "second-attempt", entry.UpstreamErrors[1].CodexTelemetry.Observations[0].ActiveLimit)
}

func TestCodexTelemetryStreamSnapshotDoesNotChangeErrorClassificationInputs(t *testing.T) {
	c, _ := telemetryContext(true)
	a := beginCodexHTTPAttempt(c, telemetryAccount("http://127.0.0.1:1"))
	a.headers(http.Header{"X-Codex-Active-Limit": {"failure"}}, codextelemetry.HTTPHeaders, 429)
	streamErr := OpsStreamError{ErrType: "client_error"}
	snapshotOpsStreamErrorContext(c, &streamErr)
	require.Empty(t, streamErr.UpstreamErrors, "metadata-only evidence must not change the logger's classification inputs")
	entry := &OpsInsertErrorLogInput{ErrorPhase: "client", ErrorOwner: "client"}
	ApplyOpsStreamTelemetrySnapshot(entry, streamErr)
	require.Len(t, entry.UpstreamErrors, 1)
	require.Equal(t, "client", entry.ErrorPhase)
	require.Equal(t, "client", entry.ErrorOwner)
}
