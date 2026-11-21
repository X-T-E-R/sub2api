package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This file measures the Antigravity Messages request-input lifecycle through the
// real Gin route (/v1/messages -> GatewayHandler.Messages -> AntigravityGatewayService.Forward)
// against a deterministic in-process upstream. The upstream drains the outbound
// body without retaining it and holds every response after its first SSE event, so
// a GC at that point reports what each in-flight request keeps alive while the
// response streams. Benchmarks are opt-in (go test -bench); the Test at the end is
// a default-run regression bound on the same harness.

type agMemScenario struct {
	name        string
	model       string // client model, mapped to itself on the account
	images      int    // tool_result screenshots (ReadMediaFile style)
	imageBytes  int    // raw bytes per image before base64
	textBytes   int    // total visible text spread over history turns
	stream      bool
	concurrency int
	// streamEvents adds this many text deltas after the midstream gate, so a held
	// request is followed by a long stream (2000 events ≈ a long agent answer).
	streamEvents int
}

func (sc agMemScenario) label() string {
	mode := "nonstream"
	if sc.stream {
		mode = "stream"
	}
	if sc.streamEvents > 0 {
		mode += fmt.Sprintf("-long%d", sc.streamEvents)
	}
	return fmt.Sprintf("%s/img%dx%dKiB/text%dKiB/%s/c%d", sc.model, sc.images, sc.imageBytes>>10, sc.textBytes>>10, mode, sc.concurrency)
}

// buildAgMemRequest returns a deterministic Claude Code-shaped Messages body:
// system blocks, tool schemas, a text history, ReadMediaFile tool_use/tool_result
// pairs carrying base64 PNG-like screenshots, and a final user turn.
func buildAgMemRequest(sc agMemScenario) []byte {
	rng := rand.New(rand.NewSource(20261122))
	randText := func(n int) string {
		const words = "alpha beta gamma delta epsilon zeta theta lambda sigma omega func return struct import package "
		var b strings.Builder
		for b.Len() < n {
			start := rng.Intn(len(words) - 12)
			b.WriteString(words[start : start+12])
		}
		return b.String()[:n]
	}
	type obj = map[string]any
	tools := make([]obj, 0, 24)
	for i := 0; i < 24; i++ {
		tools = append(tools, obj{
			"name":        fmt.Sprintf("Tool%02d", i),
			"description": randText(600),
			"input_schema": obj{
				"type": "object",
				"properties": obj{
					"path":    obj{"type": "string", "description": randText(120)},
					"options": obj{"type": "object", "properties": obj{"limit": obj{"type": "integer"}}},
				},
				"required": []string{"path"},
			},
		})
	}
	tools = append(tools, obj{"name": "ReadMediaFile", "description": "Read an image", "input_schema": obj{"type": "object", "properties": obj{"path": obj{"type": "string"}}}})

	turns := 40
	perTurn := sc.textBytes / turns
	messages := make([]obj, 0, turns*2+sc.images*2+1)
	for i := 0; i < turns; i++ {
		messages = append(messages,
			obj{"role": "user", "content": []obj{{"type": "text", "text": randText(perTurn / 2)}}},
			obj{"role": "assistant", "content": []obj{{"type": "text", "text": randText(perTurn / 2)}}},
		)
	}
	raw := make([]byte, sc.imageBytes)
	for i := 0; i < sc.images; i++ {
		_, _ = rng.Read(raw)
		copy(raw, []byte("\x89PNG\r\n\x1a\n"))
		id := fmt.Sprintf("toolu_media_%02d", i)
		messages = append(messages,
			obj{"role": "assistant", "content": []obj{{"type": "tool_use", "id": id, "name": "ReadMediaFile", "input": obj{"path": fmt.Sprintf("shot_%02d.png", i)}}}},
			obj{"role": "user", "content": []obj{{"type": "tool_result", "tool_use_id": id, "content": []obj{
				{"type": "text", "text": "Read image shot.png (3840x1984)"},
				{"type": "image", "source": obj{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(raw)}},
			}}}},
		)
	}
	messages = append(messages, obj{"role": "user", "content": "Describe the last screenshot and continue."})
	body := obj{
		"model":      sc.model,
		"max_tokens": 8192,
		"stream":     sc.stream,
		"thinking":   obj{"type": "adaptive"},
		"metadata":   obj{"user_id": "user_bench_account__session_7f6e5d4c-0000-4000-8000-000000000001"},
		"system":     []obj{{"type": "text", "text": randText(12 << 10)}},
		"tools":      tools,
		"messages":   messages,
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// gatedAntigravityUpstream drains each outbound request, answers with one SSE event,
// then blocks the response until the burst gate opens.
type gatedAntigravityUpstream struct {
	mu        sync.Mutex
	gate      chan struct{}
	reached   chan struct{} // one signal per response held midstream
	doCalls   atomic.Int64
	wireBytes atomic.Int64
	sendAt    atomic.Int64 // unix nanos of the latest client send (sequential latency)
	events    int          // text deltas emitted after the gate
	wireDump  string       // optional path for the next verified outbound body
	upLatency []time.Duration
	verify    func([]byte)
}

func (u *gatedAntigravityUpstream) reset() {
	u.mu.Lock()
	u.gate = make(chan struct{})
	u.reached = make(chan struct{}, 1024)
	u.upLatency = u.upLatency[:0]
	u.mu.Unlock()
}

func (u *gatedAntigravityUpstream) open() {
	u.mu.Lock()
	close(u.gate)
	u.mu.Unlock()
}

func (u *gatedAntigravityUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.doCalls.Add(1)
	if at := u.sendAt.Load(); at > 0 {
		u.mu.Lock()
		u.upLatency = append(u.upLatency, time.Since(time.Unix(0, at)))
		u.mu.Unlock()
	}
	u.mu.Lock()
	verify := u.verify
	u.verify = nil
	gate := u.gate
	reached := u.reached
	u.mu.Unlock()
	if verify != nil {
		buf, _ := io.ReadAll(req.Body)
		u.wireBytes.Add(int64(len(buf)))
		verify(buf)
		if u.wireDump != "" {
			_ = os.WriteFile(u.wireDump, buf, 0o644)
		}
	} else {
		n, _ := io.Copy(io.Discard, req.Body)
		u.wireBytes.Add(n)
	}
	_ = req.Body.Close()
	first := `data: {"response":{"responseId":"resp_mem","candidates":[{"content":{"role":"model","parts":[{"text":"screenshot shows "}]}}],"usageMetadata":{"promptTokenCount":9000,"candidatesTokenCount":3}}}` + "\n\n"
	last := `data: {"response":{"responseId":"resp_mem","candidates":[{"content":{"role":"model","parts":[{"text":"a dashboard."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9000,"candidatesTokenCount":7}}}` + "\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"req-mem"}},
		Body:       &gatedSSEBody{chunks: agMemSSEChunks(first, last, u.events), gate: gate, ctx: req.Context(), onBlock: func() { reached <- struct{}{} }},
		Request:    req,
	}, nil
}

func (u *gatedAntigravityUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func agMemSSEChunks(first, last string, events int) [][]byte {
	chunks := make([][]byte, 0, events+2)
	chunks = append(chunks, []byte(first))
	for i := 0; i < events; i++ {
		chunks = append(chunks, []byte(fmt.Sprintf(`data: {"response":{"responseId":"resp_mem","candidates":[{"content":{"role":"model","parts":[{"text":"tok%04d "}]}}],"usageMetadata":{"promptTokenCount":9000,"candidatesTokenCount":%d}}}`+"\n\n", i, 4+i)))
	}
	return append(chunks, []byte(last))
}

type gatedSSEBody struct {
	chunks  [][]byte
	idx     int
	off     int
	gated   bool
	gate    <-chan struct{}
	ctx     context.Context
	onBlock func()
}

func (b *gatedSSEBody) Read(p []byte) (int, error) {
	if b.idx >= len(b.chunks) {
		return 0, io.EOF
	}
	if b.idx == 1 && !b.gated {
		b.gated = true
		b.onBlock()
		select {
		case <-b.gate:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	n := copy(p, b.chunks[b.idx][b.off:])
	b.off += n
	if b.off == len(b.chunks[b.idx]) {
		b.idx++
		b.off = 0
	}
	return n, nil
}

func (b *gatedSSEBody) Close() error { return nil }

type agMemRig struct {
	harness  *gatewayHarness
	srv      *httptest.Server
	upstream *gatedAntigravityUpstream
	apiKey   string
	client   *http.Client
}

func newAgMemRig(tb testing.TB, model string) *agMemRig {
	tb.Helper()
	up := &gatedAntigravityUpstream{}
	up.reset()
	acc := newAntigravityAccount(701, "ag-mem", 1, map[string]any{model: model})
	acc.Concurrency = 128
	h := newGatewayHarness(tb, gatewayHarnessConfig{Accounts: []*service.Account{acc}, HTTPUpstream: up})
	srv := httptest.NewServer(h.router)
	tb.Cleanup(srv.Close)
	return &agMemRig{
		harness:  h,
		srv:      srv,
		upstream: up,
		apiKey:   h.apiKey.Key,
		client:   &http.Client{Transport: &http.Transport{MaxConnsPerHost: 256, MaxIdleConnsPerHost: 256, DisableCompression: true}},
	}
}

func (r *agMemRig) send(body []byte) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), err
}

type agMemSample struct {
	midLiveMiB  float64 // HeapAlloc after GC with every request held midstream
	baseLiveMiB float64 // HeapAlloc after GC before the burst
	afterMiB    float64 // HeapAlloc after GC once the burst drained
	peakObjMiB  float64 // max sampled live+unswept heap objects during the burst
	peakHeapMiB float64 // max sampled heap mapped-and-not-released (≈ heap RSS)
	allocPerReq float64 // MiB allocated per request (TotalAlloc delta / N)
	allMidMs    float64 // wall time until every request reached midstream
	upstreamMs  float64 // median client-send -> upstream Do (sequential only)
}

func mib(v uint64) float64 { return float64(v) / (1 << 20) }

func peakSampler(stop <-chan struct{}, done chan<- [2]uint64) {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
	}
	var peakObj, peakHeap uint64
	tick := time.NewTicker(500 * time.Microsecond)
	defer tick.Stop()
	for {
		metrics.Read(samples)
		obj := samples[0].Value.Uint64()
		heap := obj + samples[1].Value.Uint64() + samples[2].Value.Uint64()
		if obj > peakObj {
			peakObj = obj
		}
		if heap > peakHeap {
			peakHeap = heap
		}
		select {
		case <-stop:
			done <- [2]uint64{peakObj, peakHeap}
			return
		case <-tick.C:
		}
	}
}

// runBurst sends sc.concurrency identical requests, waits until all are held
// midstream, samples live heap (and optionally writes a heap profile), then drains.
func (r *agMemRig) runBurst(tb testing.TB, sc agMemScenario, body []byte, profilePrefix string) agMemSample {
	tb.Helper()
	r.upstream.reset()
	// Start every burst from a returned heap so peak_heap is not inherited from an
	// earlier scenario's free spans.
	debug.FreeOSMemory()
	runtime.GC()
	writeHeap := func(stage string) {
		if profilePrefix == "" {
			return
		}
		f, err := os.Create(profilePrefix + "." + stage + ".pb.gz")
		require.NoError(tb, err)
		require.NoError(tb, pprof.Lookup("heap").WriteTo(f, 0))
		require.NoError(tb, f.Close())
	}
	writeHeap("before")
	if profilePrefix != "" {
		f, err := os.Create(profilePrefix + ".cpu.pb.gz")
		require.NoError(tb, err)
		require.NoError(tb, pprof.StartCPUProfile(f))
		defer func() { pprof.StopCPUProfile(); _ = f.Close() }()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	totalBefore := ms.TotalAlloc

	stop := make(chan struct{})
	peaks := make(chan [2]uint64, 1)
	go peakSampler(stop, peaks)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, sc.concurrency)
	for i := 0; i < sc.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sc.concurrency == 1 {
				r.upstream.sendAt.Store(time.Now().UnixNano())
			}
			status, out, err := r.send(body)
			if err == nil && status != http.StatusOK {
				err = fmt.Errorf("status %d: %.300s", status, out)
			}
			if err == nil && !strings.Contains(out, "a dashboard.") {
				err = fmt.Errorf("response missing final text: %.300s", out)
			}
			if err == nil && sc.streamEvents > 0 && !strings.Contains(out, fmt.Sprintf("tok%04d ", sc.streamEvents-1)) {
				err = fmt.Errorf("long stream lost deltas: %.300s", out[max(0, len(out)-300):])
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	r.upstream.mu.Lock()
	reached := r.upstream.reached
	r.upstream.mu.Unlock()
	timeout := time.NewTimer(60 * time.Second)
	defer timeout.Stop()
	for held := 0; held < sc.concurrency; held++ {
		select {
		case <-reached:
		case err := <-errs:
			tb.Fatalf("request failed before midstream: %v", err)
		case <-timeout.C:
			tb.Fatalf("only %d/%d requests reached midstream", held, sc.concurrency)
		}
	}
	allMid := time.Since(start)
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	mid := ms.HeapAlloc
	writeHeap("midstream")
	r.upstream.open()
	wg.Wait()
	close(stop)
	pk := <-peaks
	close(errs)
	for err := range errs {
		tb.Fatalf("request failed: %v", err)
	}
	runtime.ReadMemStats(&ms)
	totalAfter := ms.TotalAlloc
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&ms)
	writeHeap("after")

	s := agMemSample{
		baseLiveMiB: mib(base),
		midLiveMiB:  mib(mid),
		afterMiB:    mib(ms.HeapAlloc),
		peakObjMiB:  mib(pk[0]),
		peakHeapMiB: mib(pk[1]),
		allocPerReq: mib(totalAfter-totalBefore) / float64(sc.concurrency),
		allMidMs:    float64(allMid.Microseconds()) / 1000,
	}
	r.upstream.mu.Lock()
	if len(r.upstream.upLatency) > 0 {
		lat := append([]time.Duration(nil), r.upstream.upLatency...)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		s.upstreamMs = float64(lat[len(lat)/2].Microseconds()) / 1000
	}
	r.upstream.mu.Unlock()
	r.upstream.sendAt.Store(0)
	return s
}

// verifyAgMemWire checks one outbound envelope: mapped model, every screenshot
// forwarded as inline media exactly once, and the text history present.
func verifyAgMemWire(tb testing.TB, sc agMemScenario, wire []byte) {
	tb.Helper()
	if !assert.True(tb, gjson.ValidBytes(wire), "outbound body must be valid JSON") {
		return
	}
	assert.Equal(tb, sc.model, gjson.GetBytes(wire, "model").String())
	inline := 0
	text := 0
	gjson.GetBytes(wire, "request.contents").ForEach(func(_, content gjson.Result) bool {
		content.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("inlineData.data").Exists() {
				inline++
			}
			part.Get("functionResponse.parts").ForEach(func(_, p gjson.Result) bool {
				if p.Get("inlineData.data").Exists() {
					inline++
				}
				return true
			})
			text += len(part.Get("text").String())
			return true
		})
		return true
	})
	assert.Equal(tb, sc.images, inline, "every tool_result screenshot must reach the upstream once")
	assert.GreaterOrEqual(tb, text, sc.textBytes*9/10, "text history must reach the upstream")
}

func agMemScenarios() []agMemScenario {
	var out []agMemScenario
	for _, model := range []string{"claude-opus-4-6", "gemini-3-pro-high"} {
		for _, c := range []int{1, 10} {
			out = append(out,
				agMemScenario{name: "image", model: model, images: 3, imageBytes: 1536 << 10, textBytes: 512 << 10, stream: true, concurrency: c},
				agMemScenario{name: "text", model: model, images: 0, textBytes: 2 << 20, stream: true, concurrency: c},
			)
		}
		out = append(out,
			agMemScenario{name: "image", model: model, images: 3, imageBytes: 1536 << 10, textBytes: 512 << 10, stream: false, concurrency: 10},
			agMemScenario{name: "image", model: model, images: 3, imageBytes: 1536 << 10, textBytes: 512 << 10, stream: true, concurrency: 10, streamEvents: 2000},
		)
	}
	return out
}

// BenchmarkAntigravityMessagesRequestMemory reports per-burst midstream live heap,
// sampled peaks and per-request allocation for the Antigravity Messages route.
// Set SUB2_MEM_PROFILE_DIR to also write a midstream heap profile per scenario.
func BenchmarkAntigravityMessagesRequestMemory(b *testing.B) {
	profileDir := os.Getenv("SUB2_MEM_PROFILE_DIR")
	for _, sc := range agMemScenarios() {
		b.Run(sc.label(), func(b *testing.B) {
			rig := newAgMemRig(b, sc.model)
			rig.upstream.events = sc.streamEvents
			body := buildAgMemRequest(sc)
			rig.upstream.verify = func(wire []byte) { verifyAgMemWire(b, sc, wire) }
			if dir := os.Getenv("SUB2_MEM_WIRE_DIR"); dir != "" && sc.concurrency == 1 {
				rig.upstream.wireDump = filepath.Join(dir, strings.NewReplacer("/", "_").Replace(sc.label())+".wire.json")
			}
			rig.runBurst(b, agMemScenario{concurrency: 1, stream: sc.stream}, body, "") // warm-up + wire check
			var agg agMemSample
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				path := ""
				if profileDir != "" && i == 0 {
					path = filepath.Join(profileDir, strings.NewReplacer("/", "_").Replace(sc.label()))
				}
				s := rig.runBurst(b, sc, body, path)
				agg.midLiveMiB += s.midLiveMiB - s.baseLiveMiB
				agg.afterMiB += s.afterMiB - s.baseLiveMiB
				agg.peakObjMiB += s.peakObjMiB
				agg.peakHeapMiB += s.peakHeapMiB
				agg.allocPerReq += s.allocPerReq
				agg.allMidMs += s.allMidMs
				agg.upstreamMs += s.upstreamMs
			}
			n := float64(b.N)
			b.ReportMetric(float64(len(body))/(1<<20), "body_MiB")
			b.ReportMetric(agg.midLiveMiB/n, "mid_live_MiB")
			b.ReportMetric(agg.midLiveMiB/n/float64(sc.concurrency)/(float64(len(body))/(1<<20)), "mid_live_x_body")
			b.ReportMetric(agg.afterMiB/n, "after_MiB")
			b.ReportMetric(agg.peakObjMiB/n, "peak_obj_MiB")
			b.ReportMetric(agg.peakHeapMiB/n, "peak_heap_MiB")
			b.ReportMetric(agg.allocPerReq/n, "alloc_MiB/req")
			b.ReportMetric(agg.allMidMs/n, "all_mid_ms")
			if sc.concurrency == 1 {
				b.ReportMetric(agg.upstreamMs/n, "to_upstream_ms")
			}
		})
	}
}

// TestAntigravityMessagesRequestBodyLifetime is the default-run regression for the
// request-input lifecycle: through the real route, with the harness usage repository
// retaining every UsageLog (the shape of a backed-up usage queue), a finished request
// must not keep its body alive, and an in-flight stream must keep at most the inbound
// body plus one outbound envelope per request (no decoded history, no read slack).
func TestAntigravityMessagesRequestBodyLifetime(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "gemini-3-pro-high"} {
		t.Run(model, func(t *testing.T) {
			sc := agMemScenario{model: model, images: 2, imageBytes: 1 << 20, textBytes: 256 << 10, stream: true, concurrency: 4}
			rig := newAgMemRig(t, sc.model)
			body := buildAgMemRequest(sc)
			bodyMiB := float64(len(body)) / (1 << 20)
			rig.upstream.verify = func(wire []byte) { verifyAgMemWire(t, sc, wire) }
			rig.runBurst(t, agMemScenario{concurrency: 1, stream: true}, body, "") // wire check + warm-up

			s := rig.runBurst(t, sc, body, "")
			perReqMid := (s.midLiveMiB - s.baseLiveMiB) / float64(sc.concurrency)
			retained := s.afterMiB - s.baseLiveMiB
			t.Logf("body=%.2fMiB mid=%.2fx body/req after=%.2fMiB alloc=%.1fMiB/req", bodyMiB, perReqMid/bodyMiB, retained, s.allocPerReq)

			logs := rig.harness.usageRecords()
			require.Len(t, logs, sc.concurrency+1, "every request must still be recorded")
			for _, log := range logs {
				require.Equal(t, sc.model, log.Model)
			}
			require.Less(t, retained, 0.5*bodyMiB, "finished requests must not pin their bodies through retained usage logs")
			require.Less(t, perReqMid, 2.6*bodyMiB, "a held stream must not keep decoded history or read-buffer slack alive")
		})
	}
}
