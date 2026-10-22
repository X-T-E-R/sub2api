package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func continuityFixture(t *testing.T) (*antigravityCacheContinuity, *time.Time, *[]antigravityCacheEvent) {
	t.Helper()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	events := []antigravityCacheEvent{}
	o := &antigravityCacheContinuity{salt: [32]byte{1, 2, 3}, entries: make(map[antigravityCacheDigest]*antigravityCacheEntry), now: func() time.Time { return now }, emit: func(e antigravityCacheEvent) { events = append(events, e) }}
	return o, &now, &events
}

func continuityContext(user int64, session string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("session_id", session)
	group := int64(8)
	c.Set("api_key", &APIKey{ID: 20, UserID: user, GroupID: &group})
	return c
}

func continuityWire(model, system, tools, contents string) []byte {
	return []byte(fmt.Sprintf(`{"project":"private-project","model":%q,"request":{"sessionId":"private-wire-session","systemInstruction":{"parts":[{"text":%q}]},"tools":%s,"contents":%s}}`, model, system, tools, contents))
}

const continuityFirst = `[{"role":"user","parts":[{"text":"private-prompt"}]}]`
const continuityGrown = `[{"role":"user","parts":[{"text":"private-prompt"}]},{"role":"model","parts":[{"thought":true,"thoughtSignature":"private-signature","text":"private-thinking"},{"functionCall":{"name":"private-tool","args":{"secret":"private-arg"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"private-tool","response":{"result":"private-result"}}}]}]`

func TestAntigravityCacheContinuityClassification(t *testing.T) {
	tests := []struct {
		name, contents, system, tools, model, effort, want string
		gap                                                time.Duration
		prompt, cached                                     int
		account                                            int64
	}{
		{name: "identical retry is stable", want: "stable_prefix", prompt: 1000, cached: 900},
		{name: "append only signed tool history", contents: continuityGrown, want: "stable_prefix", prompt: 1100, cached: 900},
		{name: "sudden zero identical prefix", want: "upstream_miss_candidate", prompt: 1000},
		{name: "sudden zero appended prefix", contents: continuityGrown, want: "upstream_miss_candidate", prompt: 1100},
		{name: "large real new input", contents: continuityGrown, want: "appended_input_dominant", prompt: 3000, cached: 900},
		{name: "history edited", contents: `[{"role":"user","parts":[{"text":"edited"}]}]`, want: "short_prefix_changed", prompt: 1000},
		{name: "system changed", system: "new system", want: "short_prefix_changed", prompt: 1000},
		{name: "tool schema content changed", tools: `[{"functionDeclarations":[{"name":"private-tool","parameters":{"type":"object","description":"changed schema"}}]}]`, want: "short_prefix_changed", prompt: 1000},
		{name: "model changed", model: "gemini-4-argon-low", want: "context_changed", prompt: 1000},
		{name: "effort changed", effort: "low", want: "context_changed", prompt: 1000},
		{name: "account changed", account: 73, want: "context_changed", prompt: 1000},
		{name: "long gap zero is cold candidate", gap: 45 * time.Minute, want: "cold_start_candidate", prompt: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, now, events := continuityFixture(t)
			c, account := continuityContext(1, "private-session"), &Account{ID: 74}
			wire := continuityWire("gemini-4-argon-high", "private-system", `[]`, continuityFirst)
			first := o.begin(c, account, "high", nil, wire)
			first.finish(&ClaudeUsage{InputTokens: 100, CacheReadInputTokens: 900}, true, false, false)
			require.Equal(t, "first_observation", (*events)[0].Class)
			gap := time.Second
			if tt.gap != 0 {
				gap = tt.gap
			}
			*now = now.Add(gap)
			contents, system, tools, model, effort := continuityFirst, "private-system", `[]`, "gemini-4-argon-high", "high"
			if tt.contents != "" {
				contents = tt.contents
			}
			if tt.system != "" {
				system = tt.system
			}
			if tt.tools != "" {
				tools = tt.tools
			}
			if tt.model != "" {
				model = tt.model
			}
			if tt.effort != "" {
				effort = tt.effort
			}
			if tt.account != 0 {
				account.ID = tt.account
			}
			next := o.begin(c, account, effort, nil, continuityWire(model, system, tools, contents))
			next.finish(&ClaudeUsage{InputTokens: tt.prompt - tt.cached, CacheReadInputTokens: tt.cached}, true, false, false)
			e := (*events)[1]
			require.Equal(t, tt.want, e.Class)
			require.Equal(t, gap.Milliseconds(), e.IntervalMS)
			require.Equal(t, tt.prompt, e.PromptTokens)
			if tt.name == "identical retry is stable" {
				require.True(t, e.RepeatRequest)
			}
			if tt.name == "large real new input" {
				require.True(t, e.AppendedInputDominant)
				require.Equal(t, 2000, e.PromptGrowthTokens)
				require.Equal(t, 2, e.AppendedContents)
			}
			encoded, err := json.Marshal(e)
			require.NoError(t, err)
			for _, secret := range []string{"private-", "gemini-4", "high", "sessionId"} {
				require.NotContains(t, string(encoded), secret)
			}
		})
	}
}

func TestAntigravityCacheContinuityUnknownAndIsolation(t *testing.T) {
	for _, reason := range []string{"missing_session", "missing_scope", "adjusted_retry", "gateway_continuation", "incomplete_or_missing_usage"} {
		t.Run(reason, func(t *testing.T) {
			o, now, events := continuityFixture(t)
			c := continuityContext(1, "session")
			if reason == "missing_session" {
				c.Request.Header.Del("session_id")
			}
			if reason == "missing_scope" {
				c.Set("api_key", &APIKey{ID: 20})
			}
			wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
			ticket := o.begin(c, &Account{ID: 74}, "high", nil, wire)
			ticket.finish(&ClaudeUsage{InputTokens: 1000}, reason != "incomplete_or_missing_usage", reason == "adjusted_retry", reason == "gateway_continuation")
			require.Equal(t, "unknown", (*events)[0].Class)
			require.Equal(t, reason, (*events)[0].Reason)
			*now = now.Add(time.Second)
			next := o.begin(c, &Account{ID: 74}, "high", nil, wire)
			next.finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
			require.False(t, (*events)[1].Comparable)
		})
	}
	o, now, events := continuityFixture(t)
	wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
	for _, user := range []int64{1, 2} {
		o.begin(continuityContext(user, "same-session"), &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	}
	require.NotEqual(t, (*events)[0].ScopeFP, (*events)[1].ScopeFP)
	require.Equal(t, "cold_start_candidate", (*events)[1].Class)
	*now = now.Add(antigravityContinuityRetention)
	o.begin(continuityContext(1, "same-session"), &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.False(t, (*events)[2].HasPrevious)
	require.Len(t, o.entries, 1)
	other, _, _ := continuityFixture(t)
	other.salt[0]++
	require.NotEqual(t, o.fingerprint("scope", "1"), other.fingerprint("scope", "1"), "restart/service salt breaks external correlation")
}

func TestAntigravityCacheContinuityConcurrencyAndBounds(t *testing.T) {
	o, _, events := continuityFixture(t)
	c := continuityContext(1, "session")
	wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
	a := o.begin(c, &Account{ID: 74}, "high", nil, wire)
	b := o.begin(c, &Account{ID: 74}, "high", nil, wire)
	b.finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	a.finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	for _, e := range *events {
		require.Equal(t, "concurrent_requests", e.Reason)
		require.Equal(t, "unknown", e.Class)
	}
	for _, entry := range o.entries {
		require.Nil(t, entry.baseline)
	}
	// A full table of in-flight sessions cannot evict a concurrency marker.
	tickets := make([]*antigravityCacheTicket, 0, antigravityContinuityEntries)
	for i := 0; i < antigravityContinuityEntries; i++ {
		tickets = append(tickets, o.begin(continuityContext(1, fmt.Sprint(i)), &Account{ID: 74}, "high", nil, wire))
	}
	require.Len(t, o.entries, antigravityContinuityEntries)
	o.begin(continuityContext(1, "overflow"), &Account{ID: 74}, "high", nil, wire).finish(nil, false, false, false)
	require.Equal(t, "capacity", (*events)[len(*events)-1].Reason)
	for _, ticket := range tickets {
		ticket.finish(nil, false, false, false)
	}
	oversize := continuityWire("gemini-4-argon-high", "system", `[]`, `[`+strings.TrimSuffix(strings.Repeat(`{"role":"user","parts":[]},`, antigravityContinuityContents+1), ",")+`]`)
	o.begin(c, &Account{ID: 74}, "high", nil, oversize).finish(nil, false, false, false)
	require.Equal(t, "observation_limit", (*events)[len(*events)-1].Reason)
	// Fixed retained representation: at most 256 digests+lengths per entry,
	// independent of prompt, schema, signatures or tool-result sizes.
	require.Less(t, int(unsafe.Sizeof(antigravityCacheContent{}))*antigravityContinuityContents, 11<<10)
}

func TestAntigravityCacheContinuityParallel(t *testing.T) {
	o, _, _ := continuityFixture(t)
	o.emit = func(antigravityCacheEvent) {}
	wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.begin(continuityContext(1, "parallel"), &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
		}()
	}
	wg.Wait()
	require.Len(t, o.entries, 1)
}

func TestAntigravityCacheContinuityForwardHook(t *testing.T) {
	gin.SetMode(gin.TestMode)
	o, now, events := continuityFixture(t)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse(), antigravityCompatSuccessResponse()}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	svc.cacheContinuityOnce.Do(func() { svc.cacheContinuity = o })
	body := []byte(`{"model":"client-alias","stream":true,"metadata":{"user_id":"private-metadata-session"},"messages":[{"role":"user","content":"private-prompt"}]}`)
	for i := 0; i < 2; i++ {
		c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
		c.Set("api_key", &APIKey{ID: 20, UserID: 1})
		result, err := svc.Forward(context.Background(), c, antigravityMessagesMappedTestAccount("gemini-4-argon-high"), body, false)
		require.NoError(t, err)
		require.Equal(t, result.Usage.InputTokens+result.Usage.CacheReadInputTokens, (*events)[i].PromptTokens)
		*now = now.Add(time.Second)
	}
	require.Len(t, *events, 2)
	require.True(t, (*events)[1].RepeatRequest)
	require.True(t, (*events)[1].Comparable)
	require.Equal(t, "stable_prefix", (*events)[1].Class)
	require.Equal(t, 2, upstream.callCount)
}

func TestAntigravityCacheContinuityExpiryWithoutTraffic(t *testing.T) {
	o, now, events := continuityFixture(t)
	var expire func()
	o.afterFunc = func(delay time.Duration, callback func()) *time.Timer {
		require.Equal(t, antigravityContinuityRetention, delay)
		expire = callback
		return nil // fake timer: the fixture invokes it without wall-clock waiting
	}
	wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
	ticket := o.begin(continuityContext(1, "session"), &Account{ID: 74}, "high", nil, wire)
	ticket.finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.NotNil(t, expire)
	*now = now.Add(antigravityContinuityRetention)
	expire()
	require.Empty(t, o.entries, "idle fingerprints expire even if no next request arrives")
	active := o.begin(continuityContext(1, "active"), &Account{ID: 74}, "high", nil, wire)
	*now = now.Add(antigravityContinuityRetention)
	expire()
	require.Len(t, o.entries, 1, "keep only the in-flight concurrency marker")
	active.finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.Empty(t, o.entries)
	require.Equal(t, "observation_expired", (*events)[1].Reason)
}

func TestAntigravityCacheContinuitySkippedRequestBreaksBaseline(t *testing.T) {
	o, now, events := continuityFixture(t)
	c := continuityContext(1, "session")
	wire := continuityWire("gemini-4-argon-high", "system", `[]`, continuityFirst)
	o.begin(c, &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 100, CacheReadInputTokens: 900}, true, false, false)
	*now = now.Add(time.Second)
	oversize := continuityWire("gemini-4-argon-high", "system", `[]`, `[`+strings.TrimSuffix(strings.Repeat(`{"role":"user","parts":[]},`, antigravityContinuityContents+1), ",")+`]`)
	o.begin(c, &Account{ID: 74}, "high", nil, oversize).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.Equal(t, "observation_limit", (*events)[1].Reason)
	*now = now.Add(time.Second)
	o.begin(c, &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.Equal(t, "unknown", (*events)[2].Class)
	require.Equal(t, "previous_unavailable", (*events)[2].Reason)
	// A disposable assistant-prefill instruction is an expected compatibility
	// transition, not evidence that caller history or conversion was corrupted.
	prefill := continuityWire("gemini-4-argon-high", "system", `[]`, `[{"role":"user","parts":[{"text":"[sub2api:assistant-prefill]\nContinue"}]}]`)
	*now = now.Add(time.Second)
	o.begin(c, &Account{ID: 74}, "high", nil, prefill).finish(&ClaudeUsage{InputTokens: 1000}, true, false, false)
	require.Equal(t, "compatibility_prefill", (*events)[3].Reason)
	require.False(t, (*events)[3].Comparable)
}

func TestAntigravityCacheContinuityContinuationUsesInitialMetering(t *testing.T) {
	o, _, events := continuityFixture(t)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityThoughtStreamResponse(antigravityThoughtGenerationOneLine), antigravityThoughtStreamResponse(antigravityThoughtGenerationTwoLine)}}
	svc := newAntigravityThoughtTestService(t, AntigravityThoughtPolicySettings{ContinueThoughtOnly: true}, upstream)
	svc.cacheContinuityOnce.Do(func() { svc.cacheContinuity = o })
	body, c, _ := newAntigravityThoughtClaudeRequest(true)
	c.Set("api_key", &APIKey{ID: 20, UserID: 1})
	c.Request.Header.Set("session_id", "private-session")
	result, err := svc.Forward(context.Background(), c, antigravityThoughtAccount(nil), body, false)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount)
	require.Equal(t, antigravityThoughtGenerationOneInput+antigravityThoughtGenerationTwoInput, result.Usage.InputTokens)
	require.Len(t, *events, 1)
	require.Equal(t, "gateway_continuation", (*events)[0].Reason)
	require.Equal(t, antigravityThoughtGenerationOneInput, (*events)[0].PromptTokens, "do not mislabel cumulative usage as one request's raw prompt")
}

func TestAntigravityCacheContinuitySchemaOnlyChangeAndNoSample(t *testing.T) {
	o, now, events := continuityFixture(t)
	c := continuityContext(1, "session")
	const schema = `[{"functionDeclarations":[{"name":"same-tool","parameters":{"type":"object","properties":{"value":{"type":"string","enum":["old"]}}}}]}]`
	for i, tools := range []string{schema, strings.Replace(schema, `"old"`, `"new"`, 1)} {
		wire := continuityWire("gemini-4-argon-high", "system", tools, continuityFirst)
		o.begin(c, &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{InputTokens: 100, CacheReadInputTokens: 900}, true, false, false)
		if i == 1 {
			e := (*events)[1]
			require.Equal(t, "short_prefix_changed", e.Class)
			require.False(t, e.ToolsSame, "same tool name and schema shape but changed enum must be detected")
			require.True(t, e.SystemSame)
			require.True(t, e.PrefixSame)
		}
		*now = now.Add(time.Second)
	}
	wire := continuityWire("gemini-4-argon-high", "system", schema, continuityFirst)
	o.begin(c, &Account{ID: 74}, "high", nil, wire).finish(&ClaudeUsage{}, true, false, false)
	require.Equal(t, "unknown", (*events)[2].Class)
	require.Equal(t, "incomplete_or_missing_usage", (*events)[2].Reason)
	require.False(t, (*events)[2].ReadDroppedToZero, "no input sample must not become a zero-cache miss")
}
