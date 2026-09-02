package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCyberBlockTestCtx(headers map[string]string, body string) (*gin.Context, []byte) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c, []byte(body)
}

func TestCyberSessionExplicitBlockKey(t *testing.T) {
	c1, b1 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	k1 := CyberSessionExplicitBlockKey(101, c1, b1)
	require.NotEmpty(t, k1)

	// Same session, different apiKey → different key (isolation).
	c2, b2 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.NotEqual(t, k1, CyberSessionExplicitBlockKey(202, c2, b2))

	// Same session + same apiKey → stable key.
	c3, b3 := newCyberBlockTestCtx(map[string]string{"session_id": "sess-abc"}, `{}`)
	require.Equal(t, k1, CyberSessionExplicitBlockKey(101, c3, b3))

	// prompt_cache_key is cache/scheduling metadata, not semantic identity.
	c4, b4 := newCyberBlockTestCtx(nil, `{"prompt_cache_key":"pck-1"}`)
	require.Empty(t, CyberSessionExplicitBlockKey(101, c4, b4))

	// No explicit signal → empty key → caller must skip blocking entirely.
	c5, b5 := newCyberBlockTestCtx(nil, `{"input":"hello world"}`)
	require.Empty(t, CyberSessionExplicitBlockKey(101, c5, b5))

	// conversation_id header counts as explicit; key is stable and non-empty.
	c6, b6 := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	k6 := CyberSessionExplicitBlockKey(101, c6, b6)
	require.NotEmpty(t, k6)
	c6b, b6b := newCyberBlockTestCtx(map[string]string{"conversation_id": "conv-xyz"}, `{}`)
	require.Equal(t, k6, CyberSessionExplicitBlockKey(101, c6b, b6b), "conversation_id key must be stable")
}

func TestCyberSemanticSessionResolverUsesTrustedSourcesOnly(t *testing.T) {
	body := `{"prompt_cache_key":"shared-cache","client_metadata":{"session_id":"native-body-session"},"input":"hello"}`
	c, raw := newCyberBlockTestCtx(map[string]string{
		openCodeSessionAffinityHeader: "affinity-only",
		"conversation_id":             "header-conversation",
	}, body)
	require.Equal(t, "native-body-session", resolveCyberSemanticSession(c, raw).id)
	require.Equal(t, "metadata:codex-session-id", resolveCyberSemanticSession(c, raw).source)

	c2, raw2 := newCyberBlockTestCtx(map[string]string{openCodeSessionAffinityHeader: "affinity-only"}, body)
	require.Equal(t, "native-body-session", resolveCyberSemanticSession(c2, raw2).id)
	require.Equal(t, "metadata:codex-session-id", resolveCyberSemanticSession(c2, raw2).source)

	c3, raw3 := newCyberBlockTestCtx(map[string]string{openCodeSessionAffinityHeader: "affinity-only"}, `{"prompt_cache_key":"shared-cache","input":"hello"}`)
	require.Empty(t, resolveCyberSemanticSession(c3, raw3).id)

	overlong := strings.Repeat("x", maxPersistedSessionIDLength+1)
	c4, raw4 := newCyberBlockTestCtx(map[string]string{"session_id": overlong}, `{}`)
	require.Empty(t, resolveCyberSemanticSession(c4, raw4).id)

	c5, raw5 := newCyberBlockTestCtx(nil, `{"metadata":{"user_id":"{\"session_id\":\"claude-native-session\"}"},"messages":[]}`)
	c5.Request.URL.Path = "/v1/messages"
	require.Equal(t, "claude-native-session", resolveCyberSemanticSession(c5, raw5).id)
	c5Responses, raw5Responses := newCyberBlockTestCtx(nil, `{"metadata":{"user_id":"{\"session_id\":\"must-not-bind-responses\"}"},"input":"hello"}`)
	require.Empty(t, resolveCyberSemanticSession(c5Responses, raw5Responses).id)
	c5Chat, raw5Chat := newCyberBlockTestCtx(nil, `{"metadata":{"user_id":"{\"session_id\":\"must-not-bind-chat\"}"},"messages":[]}`)
	c5Chat.Request.URL.Path = "/v1/chat/completions"
	require.Empty(t, resolveCyberSemanticSession(c5Chat, raw5Chat).id)

	c6, raw6 := newCyberBlockTestCtx(map[string]string{"X-Codex-Turn-Metadata": `{"session_id":"codex-turn-session","turn_id":"rotating"}`}, `{"input":"hello"}`)
	require.Equal(t, "codex-turn-session", resolveCyberSemanticSession(c6, raw6).id)

	c7, raw7 := newCyberBlockTestCtx(map[string]string{grokConversationIDHeader: "grok-native-conversation"}, `{"input":"hello"}`)
	c7.Set("api_key", &APIKey{ID: 7, Group: &Group{Platform: PlatformGrok}})
	require.Equal(t, "grok-native-conversation", resolveCyberSemanticSession(c7, raw7).id)
}

func TestCyberTranscriptBlockKeysStoreOnlyExactRefusedTranscript(t *testing.T) {
	first := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question one"}]}`)
	second := []byte(`{"instructions":"shared","input":[{"role":"user","content":"fixed environment"},{"role":"user","content":"question two"}]}`)
	firstKeys := CyberSessionTranscriptBlockKeys(77, first)
	secondKeys := CyberSessionTranscriptBlockKeys(77, second)
	require.Len(t, firstKeys, 1)
	require.Len(t, secondKeys, 1)
	require.NotEqual(t, firstKeys[0], secondKeys[0])

	hit := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	changed := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	continuation := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	hitKeys := CyberSessionTranscriptBlockKeys(77, hit)
	require.Len(t, hitKeys, 1)
	require.NotContains(t, CyberSessionTranscriptLookupKeys(77, changed), hitKeys[0])
	require.Contains(t, CyberSessionTranscriptLookupKeys(77, continuation), hitKeys[0])
}

func TestCyberTranscriptCanonicalizationIncludesSystemAndExactNumbers(t *testing.T) {
	canonicalA := []byte(`{"system":[{"type":"text","text":"policy","meta":{"b":2,"a":1}}],"messages":[{"role":"user","content":{"value":9007199254740992}}]}`)
	canonicalB := []byte(" { \"messages\" : [ { \"content\" : { \"value\" : 9007199254740992 }, \"role\" : \"user\" } ], \"system\" : [ { \"meta\" : { \"a\" : 1, \"b\" : 2 }, \"text\" : \"policy\", \"type\" : \"text\" } ] }")
	differentSystem := []byte(`{"system":[{"type":"text","text":"different policy","meta":{"a":1,"b":2}}],"messages":[{"role":"user","content":{"value":9007199254740992}}]}`)
	differentLargeInteger := []byte(`{"system":[{"type":"text","text":"policy","meta":{"a":1,"b":2}}],"messages":[{"role":"user","content":{"value":9007199254740993}}]}`)
	differentNumericLexeme := []byte(`{"system":[{"type":"text","text":"policy","meta":{"a":1.0,"b":2}}],"messages":[{"role":"user","content":{"value":9007199254740992}}]}`)

	keyA := CyberSessionTranscriptBlockKeys(91, canonicalA)
	require.Equal(t, keyA, CyberSessionTranscriptBlockKeys(91, canonicalB), "whitespace and object order must canonicalize")
	require.NotEqual(t, keyA, CyberSessionTranscriptBlockKeys(91, differentSystem), "top-level Anthropic system participates in identity")
	require.NotEqual(t, keyA, CyberSessionTranscriptBlockKeys(91, differentLargeInteger), "integers above 2^53 must not collapse through float64")
	require.NotEqual(t, keyA, CyberSessionTranscriptBlockKeys(91, differentNumericLexeme), "distinct JSON numeric lexemes must remain distinct")
}

func TestCyberTranscriptBlockKeysWebSocketResponseCreate(t *testing.T) {
	body := []byte(`{"type":"response.create","response":{"prompt_cache_key":"ws-session","input":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}}`)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	require.Empty(t, CyberSessionExplicitBlockKey(88, c, body))
	require.Len(t, CyberSessionTranscriptBlockKeys(88, body), 1)
}

func TestCyberTranscriptLookupKeysAreBoundedAndKeepNewestOrder(t *testing.T) {
	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+44)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "message-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)

	keys := CyberSessionTranscriptLookupKeys(77, body)
	require.Len(t, keys, maxOpenAICyberTranscriptLookupKeys)

	firstRetainedBody, err := json.Marshal(map[string]any{"messages": messages[:45]})
	require.NoError(t, err)
	firstRetainedPrefix := CyberSessionTranscriptLookupKeys(77, firstRetainedBody)
	require.Equal(t, firstRetainedPrefix[len(firstRetainedPrefix)-1], keys[0])

	fullKey := CyberSessionTranscriptBlockKeys(77, body)[0]
	require.Equal(t, fullKey, keys[len(keys)-1])
}

// --- fakes ---

type fakeCyberBlockStore struct {
	blocked   map[string]bool
	scopes    map[string]bool
	findCalls int
	findTTL   *time.Duration
}

var _ CyberSessionBlockStore = (*fakeCyberBlockStore)(nil)

func (f *fakeCyberBlockStore) SetCyberSessionBlocked(_ context.Context, kind CyberSessionBlockKind, scopeKey, digest string, _ time.Duration) error {
	if f.blocked == nil {
		f.blocked = map[string]bool{}
	}
	f.blocked[string(kind)+":"+digest] = true
	if scopeKey != "" {
		if f.scopes == nil {
			f.scopes = map[string]bool{}
		}
		f.scopes[scopeKey] = true
	}
	return nil
}

func (f *fakeCyberBlockStore) IsCyberSessionScopeActive(_ context.Context, scopeKey string) (bool, error) {
	return f.scopes[scopeKey], nil
}

func (f *fakeCyberBlockStore) FindCyberSessionBlocked(_ context.Context, kind CyberSessionBlockKind, keys []string) (string, time.Duration, error) {
	f.findCalls++
	for _, key := range keys {
		if f.blocked[string(kind)+":"+key] {
			ttl := time.Minute
			if f.findTTL != nil {
				ttl = *f.findTTL
			}
			return key, ttl, nil
		}
	}
	return "", 0, nil
}

func TestCyberLookupReceiptRequiresPositiveTTL(t *testing.T) {
	zero := time.Duration(0)
	store := &fakeCyberBlockStore{
		blocked: map[string]bool{string(CyberSessionBlockKindExplicit) + ":digest": true},
		findTTL: &zero,
	}
	receipt := findCyberSessionReceipt(context.Background(), store, CyberSessionBlockKindExplicit, "header:session_id", []string{"digest"}, 1, false)
	require.False(t, receipt.Matched())
	require.False(t, receipt.Stored)
	require.Empty(t, receipt.Digest)
}

// fakeSettingRepo is a minimal SettingRepository stub for unit tests.
// Only GetValue is exercised by GetCyberSessionBlockRuntime; all other methods
// panic so accidental calls are caught immediately.
type fakeSettingRepo struct {
	vals map[string]string
}

func (r *fakeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.vals[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (r *fakeSettingRepo) Get(_ context.Context, _ string) (*Setting, error) {
	panic("fakeSettingRepo.Get not implemented")
}
func (r *fakeSettingRepo) Set(_ context.Context, _, _ string) error {
	panic("fakeSettingRepo.Set not implemented")
}
func (r *fakeSettingRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	return r.vals, nil
}
func (r *fakeSettingRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	panic("fakeSettingRepo.SetMultiple not implemented")
}
func (r *fakeSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	panic("fakeSettingRepo.GetAll not implemented")
}
func (r *fakeSettingRepo) Delete(_ context.Context, _ string) error {
	panic("fakeSettingRepo.Delete not implemented")
}

var _ SettingRepository = (*fakeSettingRepo)(nil)

// comboCacheAndStore implements both GatewayCache (no-op stubs) and
// CyberSessionBlockStore (delegates to fakeCyberBlockStore) so it can be
// injected as s.cache and successfully type-asserted to CyberSessionBlockStore.
type comboCacheAndStore struct {
	store fakeCyberBlockStore
}

var _ GatewayCache = (*comboCacheAndStore)(nil)
var _ CyberSessionBlockStore = (*comboCacheAndStore)(nil)

func (c *comboCacheAndStore) GetSessionAccountID(_ context.Context, _ int64, _ string) (int64, error) {
	return 0, errors.New("stub")
}
func (c *comboCacheAndStore) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) RefreshSessionTTL(_ context.Context, _ int64, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) DeleteSessionAccountID(_ context.Context, _ int64, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (c *comboCacheAndStore) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *comboCacheAndStore) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}

func (c *comboCacheAndStore) SetReasoningContent(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (c *comboCacheAndStore) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func (c *comboCacheAndStore) SetCyberSessionBlocked(ctx context.Context, kind CyberSessionBlockKind, scopeKey, digest string, ttl time.Duration) error {
	return c.store.SetCyberSessionBlocked(ctx, kind, scopeKey, digest, ttl)
}
func (c *comboCacheAndStore) IsCyberSessionScopeActive(ctx context.Context, scopeKey string) (bool, error) {
	return c.store.IsCyberSessionScopeActive(ctx, scopeKey)
}
func (c *comboCacheAndStore) FindCyberSessionBlocked(ctx context.Context, kind CyberSessionBlockKind, keys []string) (string, time.Duration, error) {
	return c.store.FindCyberSessionBlocked(ctx, kind, keys)
}

// --- tests ---

// TestIsCyberSessionBlocked_EmptyKeyAndNilService covers the fail-open paths:
// empty key, nil service, store missing → always false / no panic.
func TestFindCyberSessionBlocked_EmptyAndNilService(t *testing.T) {
	var nilSvc *OpenAIGatewayService
	require.False(t, nilSvc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", "").Matched())
	require.NotPanics(t, func() { nilSvc.MarkCyberSessionBlocked(context.Background(), CyberSessionBlockPlan{}) })

	svc := &OpenAIGatewayService{}
	require.False(t, svc.FindCyberSessionBlockedForRequest(context.Background(), 1, nil, nil, "", "").Matched())
}

// TestCyberSessionBlock_RoundTrip exercises the type-assertion success path:
// mark a session blocked via a combo cache+store, then confirm IsCyberSessionBlocked
// returns true, and an unrelated key returns false.
func TestCyberSessionBlock_RoundTrip(t *testing.T) {
	// SettingService with only settingRepo set — GetCyberSessionBlockRuntime needs
	// nothing else (cfg/proxyRepo/etc. are not touched by this code path).
	settingSvc := &SettingService{
		settingRepo: &fakeSettingRepo{
			vals: map[string]string{
				SettingKeyCyberSessionBlockEnabled:    "true",
				SettingKeyCyberSessionBlockTTLSeconds: "60",
			},
		},
	}

	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{
		cache:          combo,
		settingService: settingSvc,
	}

	ctx := context.Background()
	c, body := newCyberBlockTestCtx(map[string]string{"session_id": "sess-roundtrip"}, `{}`)
	explicitKey := CyberSessionExplicitBlockKey(1, c, body)
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0").Matched())

	plan := ResolveCyberSessionBlockWritePlan(1, c, body, "203.0.113.1", "client/1.0")
	receipt := svc.MarkCyberSessionBlocked(ctx, plan)
	require.True(t, receipt.Stored)

	require.Equal(t, explicitKey, svc.FindCyberSessionBlockedForRequest(ctx, 1, c, body, "203.0.113.1", "client/1.0").Digest)
}

func TestFindCyberSessionBlockedForRequestUsesScopeForTranscript(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()

	hitBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"}]}`)
	nextBody := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"trigger"},{"role":"assistant","content":"blocked"},{"role":"user","content":"continue"}]}`)
	nextCtx, _ := newCyberBlockTestCtx(nil, string(nextBody))
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"
	blockKey := CyberSessionTranscriptBlockKeys(9, hitBody)[0]

	// Without an active source scope, transcript candidates are never blocks.
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, userAgent).Matched())
	scopeKey := CyberSessionScopeKey(9, clientIP, userAgent)
	svc.MarkCyberSessionBlocked(ctx, CyberSessionBlockPlan{Digest: blockKey, Kind: CyberSessionBlockKindTranscript, Source: "transcript", ScopeKey: scopeKey, Count: 1})
	require.Equal(t, blockKey, svc.FindCyberSessionBlockedForRequest(ctx, 9, nextCtx, nextBody, clientIP, "Codex CLI 1.2.4").Digest)
}

func TestCyberSessionExplicitModeIsExclusiveAndPromptCacheIsolated(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()
	transcript := `{"prompt_cache_key":"reused-cache","messages":[{"role":"user","content":"same prompt"}]}`
	cA, bodyA := newCyberBlockTestCtx(map[string]string{"session_id": "session-A"}, transcript)
	cB, bodyB := newCyberBlockTestCtx(map[string]string{"session_id": "session-B"}, transcript)
	planA := ResolveCyberSessionBlockWritePlan(12, cA, bodyA, "203.0.113.9", "client/1")
	require.Equal(t, CyberSessionBlockKindExplicit, planA.Kind)
	svc.MarkCyberSessionBlocked(ctx, planA)
	require.True(t, svc.FindCyberSessionBlockedForRequest(ctx, 12, cA, bodyA, "203.0.113.9", "client/1").Matched())
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, 12, cB, bodyB, "203.0.113.9", "client/1").Matched(), "explicit B must not fall back to matching transcript ancestry")

	cacheOnlyA, cacheBodyA := newCyberBlockTestCtx(nil, `{"prompt_cache_key":"reused-cache","input":"first"}`)
	cacheOnlyB, cacheBodyB := newCyberBlockTestCtx(nil, `{"prompt_cache_key":"reused-cache","input":"second"}`)
	planCacheA := ResolveCyberSessionBlockWritePlan(12, cacheOnlyA, cacheBodyA, "203.0.113.9", "client/1")
	require.Equal(t, CyberSessionBlockKindTranscript, planCacheA.Kind)
	svc.MarkCyberSessionBlocked(ctx, planCacheA)
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, 12, cacheOnlyB, cacheBodyB, "203.0.113.9", "client/1").Matched(), "reused prompt_cache_key cannot link unrelated prompts")
}

func TestCyberTranscriptChangedLatestUserPassesButExactAppendBlocks(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()
	const ipAddr, userAgent = "203.0.113.10", "client/1"
	refused := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"blocked trigger"}]}`)
	refusedCtx, _ := newCyberBlockTestCtx(nil, string(refused))
	svc.MarkCyberSessionBlocked(ctx, ResolveCyberSessionBlockWritePlan(13, refusedCtx, refused, ipAddr, userAgent))

	changed := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"different question"}]}`)
	changedCtx, _ := newCyberBlockTestCtx(nil, string(changed))
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, 13, changedCtx, changed, ipAddr, userAgent).Matched())

	appended := []byte(`{"messages":[{"role":"user","content":"setup"},{"role":"assistant","content":"ready"},{"role":"user","content":"blocked trigger"},{"role":"assistant","content":"refused"},{"role":"user","content":"continue"}]}`)
	appendedCtx, _ := newCyberBlockTestCtx(nil, string(appended))
	require.True(t, svc.FindCyberSessionBlockedForRequest(ctx, 13, appendedCtx, appended, ipAddr, userAgent).Matched())
}

func TestCyberTranscript257thOldPrefixMissesFailOpen(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled: "true", SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()
	const ipAddr, userAgent = "203.0.113.11", "client/1"
	first := []byte(`{"messages":[{"role":"user","content":"old refused"}]}`)
	firstCtx, _ := newCyberBlockTestCtx(nil, string(first))
	svc.MarkCyberSessionBlocked(ctx, ResolveCyberSessionBlockWritePlan(14, firstCtx, first, ipAddr, userAgent))

	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+1)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "unrelated-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": append([]map[string]string{{"role": "user", "content": "old refused"}}, messages...)})
	require.NoError(t, err)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	receipt := svc.FindCyberSessionBlockedForRequest(ctx, 14, c, body, ipAddr, userAgent)
	require.False(t, receipt.Matched())
	require.True(t, receipt.Truncated)
	require.Equal(t, maxOpenAICyberTranscriptLookupKeys, receipt.Count)
}

func TestCyberTwoSourceFixtureHasDistinctOpaqueKeysAndPrivateTrace(t *testing.T) {
	c1, b1 := newCyberBlockTestCtx(map[string]string{"session_id": "production-session-source-one"}, `{"input":"secret-one"}`)
	c2, b2 := newCyberBlockTestCtx(map[string]string{"session_id": "production-session-source-two"}, `{"input":"secret-two"}`)
	p1 := ResolveCyberSessionBlockWritePlan(55, c1, b1, "", "")
	p2 := ResolveCyberSessionBlockWritePlan(55, c2, b2, "", "")
	require.NotEqual(t, p1.Digest, p2.Digest)

	ctx1, observer1, _ := newRequestObservationTestContext(t, time.Now())
	observer1.beginAttempt(3, PlatformOpenAI)
	RecordRequestObservationCyberSessionReceipt(ctx1, CyberSessionBlockReceipt{Digest: p1.Digest, Kind: p1.Kind, Source: p1.Source, Count: 1, Stored: true, ExpiryUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	ctx2, observer2, _ := newRequestObservationTestContext(t, time.Now())
	observer2.beginAttempt(56, PlatformOpenAI)
	RecordRequestObservationCyberSessionReceipt(ctx2, CyberSessionBlockReceipt{Digest: p2.Digest, Kind: p2.Kind, Source: p2.Source, Count: 1, Stored: true, ExpiryUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	encoded, err := json.Marshal([]*RequestAttemptLedger{buildRequestAttemptLedger(observer1.attempts), buildRequestAttemptLedger(observer2.attempts)})
	require.NoError(t, err)
	require.Contains(t, string(encoded), p1.Digest)
	require.Contains(t, string(encoded), p2.Digest)
	require.Contains(t, string(encoded), `"account_id":3`)
	require.Contains(t, string(encoded), `"account_id":56`)
	for _, raw := range []string{"production-session-source-one", "production-session-source-two", "secret-one", "secret-two"} {
		require.NotContains(t, string(encoded), raw)
	}
}

func TestFindCyberSessionBlockedForRequestFailsOpenOnScopedTranscriptOverflow(t *testing.T) {
	settingSvc := &SettingService{settingRepo: &fakeSettingRepo{vals: map[string]string{
		SettingKeyCyberSessionBlockEnabled:    "true",
		SettingKeyCyberSessionBlockTTLSeconds: "60",
	}}}
	combo := &comboCacheAndStore{}
	svc := &OpenAIGatewayService{cache: combo, settingService: settingSvc}
	ctx := context.Background()
	const apiKeyID = int64(9)
	const clientIP = "203.0.113.20"
	const userAgent = "Codex CLI 1.2.3"

	messages := make([]map[string]string, maxOpenAICyberTranscriptLookupKeys+1)
	for i := range messages {
		messages[i] = map[string]string{"role": "user", "content": "message-" + strconv.Itoa(i)}
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)
	c, _ := newCyberBlockTestCtx(nil, string(body))
	require.False(t, svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent).Matched(),
		"overflow alone must not bypass the scope gate")
	combo.store.scopes = map[string]bool{CyberSessionScopeKey(apiKeyID, clientIP, userAgent): true}

	receipt := svc.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent)
	require.False(t, receipt.Matched())
	require.True(t, receipt.Truncated)
	require.Equal(t, maxOpenAICyberTranscriptLookupKeys, receipt.Count)
	require.Equal(t, 1, combo.store.findCalls, "overflow must issue only the bounded exact lookup")
}

func TestCyberSessionScopeKeyNormalizesUserAgentVersion(t *testing.T) {
	base := CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.3")
	require.NotEmpty(t, base)
	require.Equal(t, base, CyberSessionScopeKey(7, "203.0.113.10", "Codex CLI 1.2.4"))
	require.NotEqual(t, base, CyberSessionScopeKey(8, "203.0.113.10", "Codex CLI 1.2.3"))
	require.NotEqual(t, base, CyberSessionScopeKey(7, "203.0.113.11", "Codex CLI 1.2.3"))
}
