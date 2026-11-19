package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Gate B: Contract-Verifying Request-Rewrite & Chaos Failover Crucible.
// Exercises the full in-process Gin HTTP pipeline under concurrency with a
// contract-verifying mock upstream, chaos faults (429/502 failover across
// heterogeneous account mappings, two-stage malformed function recovery,
// soft-safety stream cuts + streak guard, mid-stream client disconnects),
// asserting zero data races, zero leaked concurrency slots or goroutines,
// and exact usage billing.

func TestChaosCrucible_AntigravityConcurrentRewriteFailoverAndDisconnects(t *testing.T) {
	accPrimary := newAntigravityAccount(701, "ag-crucible-primary", 1, map[string]any{
		"crucible-effort": "gemini-4-hydrogen-{effort}",
		"crucible-mm":     "gemini-3-pro",
		"crucible-tool":   "gemini-3-pro",
		"crucible-mfc":    "gemini-3-pro",
		"crucible-safe":   "gemini-3-pro",
		"crucible-stream": "gemini-3-pro",
	})
	accPrimary.Concurrency = 64

	accBackup := newAntigravityAccount(702, "ag-crucible-backup", 2, map[string]any{
		"crucible-effort": "gemini-4-neon-{effort}",
		"crucible-mm":     "gemini-3-pro",
		"crucible-tool":   "gemini-3-pro",
		"crucible-mfc":    "gemini-3-pro",
		"crucible-safe":   "gemini-3-pro",
		"crucible-stream": "gemini-3-pro",
	})
	accBackup.Concurrency = 64

	var (
		contractMu       sync.Mutex
		contractErrors   []string
		mfcAttempts      atomic.Int32
		failoverInjected atomic.Int32
		disconnectMu     sync.Mutex
		disconnectChans  = map[string]chan struct{}{}
	)
	getDisconnectChan := func(key string) chan struct{} {
		disconnectMu.Lock()
		defer disconnectMu.Unlock()
		ch, ok := disconnectChans[key]
		if !ok {
			ch = make(chan struct{})
			disconnectChans[key] = ch
		}
		return ch
	}
	closeDisconnectChan := func(key string) {
		ch := getDisconnectChan(key)
		disconnectMu.Lock()
		select {
		case <-ch:
		default:
			close(ch)
		}
		disconnectMu.Unlock()
	}
	recordContractErr := func(format string, args ...any) {
		contractMu.Lock()
		defer contractMu.Unlock()
		contractErrors = append(contractErrors, fmt.Sprintf(format, args...))
	}

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Accounts: []*service.Account{accPrimary, accBackup},
		Config: &config.Config{
			RunMode: config.RunModeSimple,
			Gateway: config.GatewayConfig{
				StreamPreheaderKeepaliveEnabled: true,
				StreamKeepaliveInterval:         1,
			},
		},
		Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
			if req.Headers.Get("Authorization") == "" {
				recordContractErr("missing Authorization header on attempt=%d account=%d", req.Attempt, req.AccountID)
			}
			model := gjson.GetBytes(req.Body, "model").String()
			raw := string(req.Body)

			switch {
			case strings.HasPrefix(model, "gemini-4-hydrogen-"):
				// Force 429 failover on primary for effort template requests to prove
				// failover re-expands cleanly on backup (gemini-4-neon-{effort}).
				failoverInjected.Add(1)
				return jsonHTTPResponse(http.StatusTooManyRequests, `{
					"error": {
						"code": 429,
						"message": "capacity exhausted; quota will reset after 30s",
						"status": "RESOURCE_EXHAUSTED",
						"details": [{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]
					}
				}`), nil

			case strings.HasPrefix(model, "gemini-4-neon-"):
				if gjson.GetBytes(req.Body, "request.generationConfig.thinkingConfig.thinkingLevel").Exists() {
					recordContractErr("effort template model %q must not leak thinkingLevel", model)
				}
				return antigravityTextSSE("effort-failover-ok:"+model, 20, 8, 4), nil

			case strings.Contains(raw, "CRUCIBLE_MULTIMODAL"):
				// Verify top-level image + tool_result video/audio inlineData made it cleanly.
				var hasImage, hasVideo, hasAudio bool
				checkMime := func(m string) {
					switch m {
					case "image/png":
						hasImage = true
					case "video/mp4":
						hasVideo = true
					case "audio/mp3":
						hasAudio = true
					}
				}
				for _, c := range gjson.GetBytes(req.Body, "request.contents").Array() {
					for _, p := range c.Get("parts").Array() {
						checkMime(p.Get("inlineData.mimeType").String())
						for _, frp := range p.Get("functionResponse.parts").Array() {
							checkMime(frp.Get("inlineData.mimeType").String())
						}
					}
				}
				if !hasImage || !hasVideo || !hasAudio {
					recordContractErr("multimodal inlineData missing (img=%v vid=%v aud=%v)", hasImage, hasVideo, hasAudio)
				}
				return antigravityTextSSE("mm-ok", 30, 10, 0), nil

			case strings.Contains(raw, "CRUCIBLE_HETERO_TURN"):
				contents := gjson.GetBytes(req.Body, "request.contents").Array()
				var foundFuncResp, foundSteerText bool
				for _, c := range contents {
					for _, p := range c.Get("parts").Array() {
						if p.Get("functionResponse.name").String() == "inspect_env" {
							foundFuncResp = true
						}
						if strings.Contains(p.Get("text").String(), "CRUCIBLE_HETERO_TURN") {
							foundSteerText = true
						}
					}
				}
				if !foundFuncResp || !foundSteerText {
					recordContractErr("heterogeneous turn lost parts (funcResp=%v steer=%v)", foundFuncResp, foundSteerText)
				}
				return antigravityTextSSE("hetero-ok", 24, 6, 0), nil

			case strings.Contains(raw, "CRUCIBLE_MFC_SEQ"):
				step := mfcAttempts.Add(1)
				mode := gjson.GetBytes(req.Body, "request.toolConfig.functionCallingConfig.mode").String()
				switch step {
				case 1:
					if mode != "VALIDATED" {
						recordContractErr("MFC step 1 expected VALIDATED mode, got %q", mode)
					}
					return sseHTTPResponse(
						`data: {"response":{"responseId":"mfc_1","candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3}}}`,
					), nil
				case 2:
					if mode != "AUTO" {
						recordContractErr("MFC step 2 expected AUTO mode, got %q", mode)
					}
					return sseHTTPResponse(
						`data: {"response":{"responseId":"mfc_2","candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3}}}`,
					), nil
				case 3:
					if mode != "AUTO" {
						recordContractErr("MFC step 3 expected AUTO mode, got %q", mode)
					}
					if !strings.Contains(raw, "emit at most ONE tool call") {
						recordContractErr("MFC step 3 missing Stage-2 single-tool recovery note")
					}
					return antigravityTextSSE("mfc-recovered", 12, 5, 0), nil
				default:
					if mode != "VALIDATED" {
						recordContractErr("MFC step 4 expected recovery cleared to VALIDATED, got %q", mode)
					}
					return antigravityTextSSE("mfc-clean-again", 12, 5, 0), nil
				}

			case strings.Contains(raw, "CRUCIBLE_SOFT_SAFETY"):
				return sseHTTPResponse(
					`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"This request was blocked by Gemini's filters."}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}}`,
					`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":" See https://policies.google.com/terms/generative-"}]}}]}}`,
					`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ai/use-policy"}]}}]}}`,
				), nil

			case strings.Contains(raw, "CRUCIBLE_DISCONNECT"):
				tag := "ag-disc-0"
				if strings.Contains(raw, "CRUCIBLE_DISCONNECT-1") {
					tag = "ag-disc-1"
				}
				stopCh := getDisconnectChan(tag)
				pr, pw := io.Pipe()
				go func() {
					_, _ = io.WriteString(pw, "data: {\"response\":{\"responseId\":\"disc_1\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"chunk-1\"}]}}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2}}}\n\n")
					select {
					case <-stopCh:
					case <-req.ContextDone():
					case <-time.After(5 * time.Second):
					}
					_ = pw.CloseWithError(context.Canceled)
				}()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       pr,
				}, nil

			default:
				return antigravityTextSSE("default-ok", 10, 4, 0), nil
			}
		},
	})
	h.apiKey.User.Concurrency = 128

	// Snapshot goroutines AFTER constructing the harness (which starts
	// BillingCacheService workers & go-cache janitors cleaned up via t.Cleanup)
	// so the post-run check strictly catches per-request goroutine leaks.
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()

	var (
		wg               sync.WaitGroup
		completedNormal  atomic.Int32
		mfcAndSafeLogs   atomic.Int32
		errCh            = make(chan error, 64)
		efforts          = []string{"low", "medium", "high", "xhigh"}
		pngB64           = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII="
		mp4B64           = "AAAAIGZ0eXBpc29tAAACAGlzb21pc28y"
		mp3B64           = "//uQxAAAAAAAAAAAAAAAAAAAAAA="
	)

	// 1) 8 concurrent effort-template + 429 failover workers.
	for i := 0; i < 8; i++ {
		effort := efforts[i%len(efforts)]
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"crucible-effort","max_tokens":64,"stream":false,"output_config":{"effort":%q},"messages":[{"role":"user","content":"effort-worker-%d"}]}`, effort, idx)
			rec := h.post(t, "/v1/messages", nil, body)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("effort worker %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			wantMarker := "effort-failover-ok:gemini-4-neon-" + effort
			if !strings.Contains(rec.Body.String(), wantMarker) {
				errCh <- fmt.Errorf("effort worker %d missing %q in %s", idx, wantMarker, rec.Body.String())
				return
			}
			completedNormal.Add(1)
		}()
	}

	// 2) 6 concurrent multimodal inlineData workers.
	for i := 0; i < 6; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{
				"model": "crucible-mm",
				"max_tokens": 128,
				"stream": false,
				"tools": [{"name": "read_media", "description": "Read media", "input_schema": {"type": "object", "properties": {"id": {"type": "string"}}}}],
				"messages": [
					{"role": "user", "content": [
						{"type": "text", "text": "CRUCIBLE_MULTIMODAL-%d"},
						{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": %q}}
					]},
					{"role": "assistant", "content": [
						{"type": "tool_use", "id": "tu_mm_%d", "name": "read_media", "input": {"id": "clip-1"}}
					]},
					{"role": "user", "content": [
						{"type": "tool_result", "tool_use_id": "tu_mm_%d", "content": [
							{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": %q}},
							{"type": "audio", "source": {"type": "base64", "media_type": "audio/mp3", "data": %q}}
						]}
					]}
				]
			}`, idx, pngB64, idx, idx, mp4B64, mp3B64)
			rec := h.post(t, "/v1/messages", nil, body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mm-ok") {
				errCh <- fmt.Errorf("mm worker %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			completedNormal.Add(1)
		}()
	}

	// 3) 6 concurrent heterogeneous tool_result + steer text workers.
	for i := 0; i < 6; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{
				"model": "crucible-tool",
				"max_tokens": 128,
				"stream": false,
				"tools": [{"name": "inspect_env", "description": "Inspect", "input_schema": {"type": "object", "properties": {"k": {"type": "string"}}}}],
				"messages": [
					{"role": "user", "content": "check env"},
					{"role": "assistant", "content": [{"type": "tool_use", "id": "tu_h_%d", "name": "inspect_env", "input": {"k": "mode"}}]},
					{"role": "user", "content": [
						{"type": "tool_result", "tool_use_id": "tu_h_%d", "content": "ok"},
						{"type": "text", "text": "CRUCIBLE_HETERO_TURN-%d steer"}
					]}
				]
			}`, idx, idx, idx)
			rec := h.post(t, "/v1/messages", nil, body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hetero-ok") {
				errCh <- fmt.Errorf("hetero worker %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			completedNormal.Add(1)
		}()
	}

	// 4) 1 sequential 4-step two-stage MALFORMED_FUNCTION_CALL worker running in
	// parallel with all other traffic in its own session.
	wg.Add(1)
	go func() {
		defer wg.Done()
		hdr := map[string]string{"X-Session-Id": "crucible-mfc-session-1"}
		body := `{
			"model": "crucible-mfc",
			"max_tokens": 128,
			"stream": true,
			"tools": [{"name": "run_cmd", "description": "Run", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}}}],
			"messages": [{"role": "user", "content": "CRUCIBLE_MFC_SEQ"}]
		}`
		for step := 1; step <= 4; step++ {
			rec := h.post(t, "/v1/messages", hdr, body)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("mfc step %d unexpected status=%d body=%s", step, rec.Code, rec.Body.String())
				return
			}
			if step <= 2 && !strings.Contains(rec.Body.String(), "antigravity_malformed_function_call") {
				errCh <- fmt.Errorf("mfc step %d missing malformed error in %s", step, rec.Body.String())
				return
			}
			if step == 3 && !strings.Contains(rec.Body.String(), "mfc-recovered") {
				errCh <- fmt.Errorf("mfc step 3 missing mfc-recovered in %s", rec.Body.String())
				return
			}
			if step == 4 && !strings.Contains(rec.Body.String(), "mfc-clean-again") {
				errCh <- fmt.Errorf("mfc step 4 missing mfc-clean-again in %s", rec.Body.String())
				return
			}
			mfcAndSafeLogs.Add(1)
		}
	}()

	// 5) 1 sequential 4-step soft-safety stream cut + session streak guard worker.
	wg.Add(1)
	go func() {
		defer wg.Done()
		hdr := map[string]string{"X-Session-Id": "crucible-safety-session-1"}
		body := `{"model":"crucible-safe","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"CRUCIBLE_SOFT_SAFETY"}]}`
		for step := 1; step <= 3; step++ {
			rec := h.post(t, "/v1/messages", hdr, body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "antigravity_soft_safety_stream_cut") {
				errCh <- fmt.Errorf("safety step %d status=%d body=%s", step, rec.Code, rec.Body.String())
				return
			}
			mfcAndSafeLogs.Add(1)
		}
		// 4th request must trip streak guard pre-upstream with HTTP 400.
		rec4 := h.post(t, "/v1/messages", hdr, body)
		if rec4.Code != http.StatusBadRequest || !strings.Contains(rec4.Body.String(), "antigravity_safety_streak_aborted") {
			errCh <- fmt.Errorf("safety step 4 expected 400 antigravity_safety_streak_aborted, got status=%d body=%s", rec4.Code, rec4.Body.String())
		}
	}()

	// 6) 2 concurrent mid-stream client disconnect workers using cancellable contexts.
	for i := 0; i < 2; i++ {
		idx := i
		tag := fmt.Sprintf("ag-disc-%d", idx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer closeDisconnectChan(tag)
			body := fmt.Sprintf(`{"model":"crucible-stream","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"CRUCIBLE_DISCONNECT-%d"}]}`, idx)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+h.apiKey.Key)
			rec := newNotifyingRecorder(func() {
				cancel()
				closeDisconnectChan(tag)
			})
			h.router.ServeHTTP(rec, req)
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	contractMu.Lock()
	errsCopy := append([]string(nil), contractErrors...)
	contractMu.Unlock()
	require.Empty(t, errsCopy, "upstream wire contract violations: %v", errsCopy)

	require.Equal(t, int32(20), completedNormal.Load(), "all 20 normal/failover/multimodal/hetero workers must succeed")
	require.GreaterOrEqual(t, failoverInjected.Load(), int32(1), "429 failover path must have been exercised")
	require.Equal(t, int32(7), mfcAndSafeLogs.Load(), "4 MFC + 3 soft-safety turns must complete")

	// Usage accounting: at least the 20 normal + 7 MFC/safety billed requests must
	// be recorded (disconnected mid-stream turns may also record partial usage).
	logs := h.usageRecords()
	require.GreaterOrEqual(t, len(logs), 27)

	// Zero concurrency slot leaks!
	h.assertSlotsBalanced(t)

	// Bounded goroutine check: allow GC to reap finished HTTP/pipe goroutines.
	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baselineGoroutines+8
	}, 2*time.Second, 10*time.Millisecond, "goroutine leak detected: baseline=%d current=%d", baselineGoroutines, runtime.NumGoroutine())
}

func TestChaosCrucible_OpenAIConcurrentResponsesChatFailoverAndDisconnects(t *testing.T) {
	const groupID int64 = 750
	group := openAIGroup(groupID)
	channel := service.Channel{
		ID:       751,
		Name:     "crucible-openai-channel",
		Status:   service.StatusActive,
		GroupIDs: []int64{groupID},
		ModelMapping: map[string]map[string]string{
			service.PlatformOpenAI: {
				"crucible-gpt": "canonical-crucible-gpt",
			},
		},
	}

	// Account 1: supports both /v1/responses and raw /v1/chat/completions; fails pre-content
	// with 429 on /v1/responses and 502 on /v1/chat/completions to trigger failover.
	acc1 := newOpenAIAPIKeyAccount(751, "oa-crucible-1", 1, "https://vendor-1.example.test/v1", "sk-vendor-1", map[string]any{
		"canonical-crucible-gpt": "vendor1-gpt-alpha",
	}, false)
	acc1.Extra = map[string]any{}
	acc1.Concurrency = 64

	// Account 2: official-host backup supporting both /v1/responses and raw /v1/chat/completions
	// with a distinct model mapping.
	acc2 := newOpenAIAPIKeyAccount(752, "oa-crucible-2", 2, "https://api.openai.com/v1", "sk-vendor-2", map[string]any{
		"canonical-crucible-gpt": "gpt-5.4",
	}, false)
	acc2.Extra = map[string]any{}
	acc2.Concurrency = 64

	var (
		contractMu       sync.Mutex
		contractErrors   []string
		failoversSeen    atomic.Int32
		responsesSuccess atomic.Int32
		chatSuccess      atomic.Int32
		oaDiscMu         sync.Mutex
		oaDiscChans      = map[string]chan struct{}{}
	)
	getOADiscChan := func(key string) chan struct{} {
		oaDiscMu.Lock()
		defer oaDiscMu.Unlock()
		ch, ok := oaDiscChans[key]
		if !ok {
			ch = make(chan struct{})
			oaDiscChans[key] = ch
		}
		return ch
	}
	closeOADiscChan := func(key string) {
		ch := getOADiscChan(key)
		oaDiscMu.Lock()
		select {
		case <-ch:
		default:
			close(ch)
		}
		oaDiscMu.Unlock()
	}
	recordContractErr := func(format string, args ...any) {
		contractMu.Lock()
		defer contractMu.Unlock()
		contractErrors = append(contractErrors, fmt.Sprintf(format, args...))
	}

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Group:    group,
		Accounts: []*service.Account{acc1, acc2},
		Channels: []service.Channel{channel},
		Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
			model := gjson.GetBytes(req.Body, "model").String()
			auth := req.Headers.Get("Authorization")
			raw := string(req.Body)

			if req.AccountID == acc1.ID {
				if auth != "Bearer sk-vendor-1" {
					recordContractErr("acc1 bad auth header: %q", auth)
				}
				if model != "vendor1-gpt-alpha" {
					recordContractErr("acc1 expected model vendor1-gpt-alpha, got %q", model)
				}
				failoversSeen.Add(1)
				if strings.HasSuffix(req.URL, "/chat/completions") {
					return jsonHTTPResponse(http.StatusBadGateway, `{"error":{"type":"server_error","message":"vendor1 down"}}`), nil
				}
				return jsonHTTPResponse(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"vendor1 limited"}}`, map[string]string{
					"Retry-After": "1",
				}), nil
			}

			if req.AccountID != acc2.ID {
				recordContractErr("unexpected accountID=%d", req.AccountID)
			}
			if auth != "Bearer sk-vendor-2" {
				recordContractErr("acc2 bad auth header: %q", auth)
			}
			if model != "gpt-5.4" {
				recordContractErr("acc2 expected model gpt-5.4, got %q (failover contamination?)", model)
			}

			if strings.Contains(raw, "OA_CRUCIBLE_DISCONNECT") {
				tag := "oa-disc-0"
				if strings.Contains(raw, "OA_CRUCIBLE_DISCONNECT-1") {
					tag = "oa-disc-1"
				}
				stopCh := getOADiscChan(tag)
				pr, pw := io.Pipe()
				go func() {
					if strings.HasSuffix(req.URL, "/responses") {
						_, _ = io.WriteString(pw, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
					} else {
						_, _ = io.WriteString(pw, "data: {\"id\":\"chatcmpl-disc\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
					}
					select {
					case <-stopCh:
					case <-req.ContextDone():
					case <-time.After(5 * time.Second):
					}
					_ = pw.CloseWithError(context.Canceled)
				}()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       pr,
				}, nil
			}

			if strings.HasSuffix(req.URL, "/chat/completions") {
				if gjson.GetBytes(req.Body, "stream").Bool() && !gjson.GetBytes(req.Body, "stream_options.include_usage").Bool() {
					recordContractErr("streaming chat/completions missing stream_options.include_usage=true")
				}
				if gjson.GetBytes(req.Body, "stream").Bool() {
					return sseHTTPResponse(
						`data: {"id":"chatcmpl-c","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"cc-stream-ok"},"finish_reason":"stop"}]}`,
						``,
						`data: {"id":"chatcmpl-c","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":15,"total_tokens":55}}`,
						``,
						`data: [DONE]`,
					), nil
				}
				return jsonHTTPResponse(http.StatusOK, `{
					"id": "chatcmpl-ns",
					"object": "chat.completion",
					"model": "gpt-5.4",
					"choices": [{"index":0,"message":{"role":"assistant","content":"cc-ns-ok"},"finish_reason":"stop"}],
					"usage": {"prompt_tokens": 30, "completion_tokens": 12, "total_tokens": 42}
				}`), nil
			}

			// /v1/responses bridged streaming chat/completions request:
			if gjson.GetBytes(req.Body, "stream").Bool() {
				return sseHTTPResponse(
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"resp_cc_bridge","model":"gpt-5.4","status":"in_progress"}}`,
					``,
					`event: response.output_text.delta`,
					`data: {"type":"response.output_text.delta","delta":"cc-stream-ok"}`,
					``,
					`event: response.completed`,
					`data: {"type":"response.completed","response":{"id":"resp_cc_bridge","model":"gpt-5.4","status":"completed","usage":{"input_tokens":40,"output_tokens":15}}}`,
				), nil
			}

			// Direct /v1/responses contract checks: reasoning.effort minimal -> none on api.openai.com,
			// max_tokens normalized -> max_output_tokens, unsupported fields stripped.
			if gjson.GetBytes(req.Body, "reasoning.effort").String() != "none" {
				recordContractErr("official responses expected reasoning.effort=none, got %q", gjson.GetBytes(req.Body, "reasoning.effort").String())
			}
			if gjson.GetBytes(req.Body, "max_tokens").Exists() || gjson.GetBytes(req.Body, "max_output_tokens").Int() != 256 {
				recordContractErr("official responses max_tokens normalization failed: %s", raw)
			}
			return jsonHTTPResponse(http.StatusOK, `{
				"id": "resp_crucible",
				"object": "response",
				"status": "completed",
				"model": "gpt-5.4",
				"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses-crucible-ok"}]}],
				"usage": {"input_tokens": 50, "output_tokens": 20}
			}`), nil
		},
	})
	h.apiKey.User.Concurrency = 128

	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, 64)
	)

	// 12 concurrent /v1/responses workers with channel override + reasoning normalization + failover.
	for i := 0; i < 12; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"crucible-gpt","stream":false,"max_tokens":256,"reasoning":{"effort":"minimal"},"input":"resp-%d"}`, idx)
			rec := h.post(t, "/v1/responses", nil, body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "responses-crucible-ok") {
				errCh <- fmt.Errorf("responses worker %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			responsesSuccess.Add(1)
		}()
	}

	// 10 concurrent /v1/chat/completions streaming workers with channel override + include_usage + failover.
	for i := 0; i < 10; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"crucible-gpt","stream":true,"messages":[{"role":"user","content":"cc-%d"}]}`, idx)
			rec := h.post(t, "/v1/chat/completions", nil, body)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cc-stream-ok") {
				errCh <- fmt.Errorf("chat worker %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			chatSuccess.Add(1)
		}()
	}

	// 2 concurrent mid-stream disconnect workers on /v1/chat/completions.
	for i := 0; i < 2; i++ {
		idx := i
		tag := fmt.Sprintf("oa-disc-%d", idx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer closeOADiscChan(tag)
			body := fmt.Sprintf(`{"model":"crucible-gpt","stream":true,"messages":[{"role":"user","content":"OA_CRUCIBLE_DISCONNECT-%d"}]}`, idx)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+h.apiKey.Key)
			rec := newNotifyingRecorder(func() {
				cancel()
				closeOADiscChan(tag)
			})
			h.router.ServeHTTP(rec, req)
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	contractMu.Lock()
	errsCopy := append([]string(nil), contractErrors...)
	contractMu.Unlock()
	require.Empty(t, errsCopy, "OpenAI upstream wire contract violations: %v", errsCopy)

	require.Equal(t, int32(12), responsesSuccess.Load())
	require.Equal(t, int32(10), chatSuccess.Load())
	require.GreaterOrEqual(t, failoversSeen.Load(), int32(1))

	// Verify exact usage billing chain on completed records.
	logs := h.usageRecords()
	require.GreaterOrEqual(t, len(logs), 22)
	var billedResponses, billedChat int
	for _, l := range logs {
		require.Equal(t, acc2.ID, l.AccountID)
		require.Equal(t, "crucible-gpt", l.RequestedModel)
		require.Equal(t, "canonical-crucible-gpt", l.Model)
		requireUpstreamModel(t, l, "gpt-5.4")
		require.NotNil(t, l.ModelMappingChain)
		require.Equal(t, "crucible-gpt→canonical-crucible-gpt→gpt-5.4", *l.ModelMappingChain)
		if l.InputTokens == 50 && l.OutputTokens == 20 {
			billedResponses++
		} else if l.InputTokens == 40 && l.OutputTokens == 15 {
			billedChat++
		}
	}
	require.Equal(t, 12, billedResponses)
	require.Equal(t, 10, billedChat)

	h.assertSlotsBalanced(t)
	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baselineGoroutines+8
	}, 2*time.Second, 10*time.Millisecond, "goroutine leak detected: baseline=%d current=%d", baselineGoroutines, runtime.NumGoroutine())
}

// notifyingRecorder wraps httptest.ResponseRecorder and invokes onFirstWrite
// once when the handler flushes/writes the first response bytes (used to trigger
// deterministic mid-stream client cancellation without sleeps).
type notifyingRecorder struct {
	*httptest.ResponseRecorder
	once         sync.Once
	onFirstWrite func()
}

func newNotifyingRecorder(onFirstWrite func()) *notifyingRecorder {
	return &notifyingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		onFirstWrite:     onFirstWrite,
	}
}

func (r *notifyingRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if len(p) > 0 && r.onFirstWrite != nil {
		r.once.Do(r.onFirstWrite)
	}
	return n, err
}

func (r *notifyingRecorder) WriteString(s string) (int, error) {
	n, err := r.ResponseRecorder.WriteString(s)
	if len(s) > 0 && r.onFirstWrite != nil {
		r.once.Do(r.onFirstWrite)
	}
	return n, err
}
