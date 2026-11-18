package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type warmupTestAccountRepo struct {
	AccountRepository
	mu                   sync.Mutex
	accounts             map[int64]*Account
	clearedModelLimitIDs []int64
	extraUpdates         map[int64]map[string]any
}

func newWarmupTestAccountRepo(accounts ...*Account) *warmupTestAccountRepo {
	m := make(map[int64]*Account, len(accounts))
	for _, acc := range accounts {
		m[acc.ID] = acc
	}
	return &warmupTestAccountRepo{
		accounts:     m,
		extraUpdates: make(map[int64]map[string]any),
	}
}

func (r *warmupTestAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.accounts[id]
	if !ok {
		return nil, ErrAccountNotFound
	}
	return acc, nil
}

func (r *warmupTestAccountRepo) ClearModelRateLimits(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearedModelLimitIDs = append(r.clearedModelLimitIDs, id)
	return nil
}

func (r *warmupTestAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraUpdates[id] = updates
	return nil
}

func (r *warmupTestAccountRepo) SetModelRateLimit(_ context.Context, _ int64, _ string, _ time.Time, _ ...string) error {
	return nil
}

func (r *warmupTestAccountRepo) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	return nil
}

type warmupMockUpstream struct {
	mu      sync.Mutex
	handler func(req *http.Request, body []byte, accountID int64) (*http.Response, error)
	calls   []warmupUpstreamCall
}

type warmupUpstreamCall struct {
	accountID int64
	proxyURL  string
	body      []byte
}

func (u *warmupMockUpstream) Do(req *http.Request, proxyURL string, accountID int64, _ int) (*http.Response, error) {
	return u.invoke(req, proxyURL, accountID)
}

func (u *warmupMockUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.invoke(req, proxyURL, accountID)
}

func (u *warmupMockUpstream) invoke(req *http.Request, proxyURL string, accountID int64) (*http.Response, error) {
	var body []byte
	if req != nil && req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.mu.Lock()
	u.calls = append(u.calls, warmupUpstreamCall{
		accountID: accountID,
		proxyURL:  proxyURL,
		body:      body,
	})
	fn := u.handler
	u.mu.Unlock()
	if fn == nil {
		return warmupGeminiSSEResponse("42"), nil
	}
	return fn(req, body, accountID)
}

type warmupUsageRefresherStub struct {
	mu    sync.Mutex
	calls []warmupUsageRefreshCall
	fn    func(accountID int64, force bool) (*UsageInfo, error)
}

type warmupUsageRefreshCall struct {
	accountID int64
	force     bool
}

func (s *warmupUsageRefresherStub) GetUsage(_ context.Context, accountID int64, force ...bool) (*UsageInfo, error) {
	f := len(force) > 0 && force[0]
	s.mu.Lock()
	s.calls = append(s.calls, warmupUsageRefreshCall{accountID: accountID, force: f})
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		return fn(accountID, f)
	}
	frac := 0.99
	return &UsageInfo{
		Source: "active",
		AntigravityWindows: map[string]*AntigravityQuotaWindow{
			"gemini_5h": {
				SourceBucketID:    "gemini_5h",
				RemainingFraction: frac,
				ResetTime:         "2026-11-18T09:15:00Z",
			},
			"claude_5h": {
				SourceBucketID:    "claude_5h",
				RemainingFraction: frac,
				ResetTime:         "2026-11-18T09:15:05Z",
			},
		},
	}, nil
}

func warmupGeminiSSEResponse(answerText string) *http.Response {
	sse := fmt.Sprintf(
		"data: {\"response\":{\"responseId\":\"resp_warmup\",\"candidates\":[{\"content\":{\"parts\":[{\"thought\":true,\"text\":\"calculating...\"},{\"text\":%q}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":18,\"candidatesTokenCount\":6}}}\n\n",
		answerText,
	)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req-warmup-ok"},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}
}

func newWarmupGatewayForTest(repo AccountRepository, upstream HTTPUpstream) *AntigravityGatewayService {
	tokenProvider := NewAntigravityTokenProvider(
		repo,
		&antigravityCompatTokenCache{token: "oauth-warmup-token"},
		nil,
	)
	settingSvc := NewSettingService(&antigravitySettingRepoStub{}, &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
	})
	return NewAntigravityGatewayService(
		repo,
		nil,
		nil,
		tokenProvider,
		nil,
		upstream,
		settingSvc,
		nil,
		NewProxyStreamCircuit(&config.Config{}),
		nil,
	)
}

func TestWarmupPrompts_AllTemplatesDeterministicMathAndUnique(t *testing.T) {
	// Verify every template produces accurate arithmetic and never static "hi"/"ping".
	for idx := 0; idx < warmupPromptTemplateCount; idx++ {
		for iter := 0; iter < 10; iter++ {
			p, err := generateWarmupPromptByTemplate(idx)
			require.NoError(t, err)
			require.NotEmpty(t, p.Prompt)
			require.NotEqual(t, "hi", strings.ToLower(strings.TrimSpace(p.Prompt)))
			require.NotEqual(t, "ping", strings.ToLower(strings.TrimSpace(p.Prompt)))

			switch idx {
			case 0: // add4d: a+b
				require.True(t, strings.HasPrefix(p.PromptSummary, "add4d: "))
				parts := strings.Split(strings.TrimPrefix(p.PromptSummary, "add4d: "), "+")
				require.Len(t, parts, 2)
				a, _ := strconv.Atoi(parts[0])
				b, _ := strconv.Atoi(parts[1])
				require.GreaterOrEqual(t, a, 1000)
				require.LessOrEqual(t, a, 9999)
				require.Equal(t, strconv.Itoa(a+b), p.ExpectedAnswer)
			case 1: // sub4d: a-b
				require.True(t, strings.HasPrefix(p.PromptSummary, "sub4d: "))
				parts := strings.Split(strings.TrimPrefix(p.PromptSummary, "sub4d: "), "-")
				require.Len(t, parts, 2)
				a, _ := strconv.Atoi(parts[0])
				b, _ := strconv.Atoi(parts[1])
				require.Greater(t, a, b)
				require.Equal(t, strconv.Itoa(a-b), p.ExpectedAnswer)
			case 2: // muladd: a*b+c
				require.True(t, strings.HasPrefix(p.PromptSummary, "muladd: "))
				re := regexp.MustCompile(`^muladd: (\d+)\*(\d+)\+(\d+)$`)
				m := re.FindStringSubmatch(p.PromptSummary)
				require.Len(t, m, 4)
				a, _ := strconv.Atoi(m[1])
				b, _ := strconv.Atoi(m[2])
				c, _ := strconv.Atoi(m[3])
				require.Equal(t, strconv.Itoa(a*b+c), p.ExpectedAnswer)
			case 3: // sum4: n1+n2+n3+n4
				require.True(t, strings.HasPrefix(p.PromptSummary, "sum4: "))
				parts := strings.Split(strings.TrimPrefix(p.PromptSummary, "sum4: "), "+")
				require.Len(t, parts, 4)
				sum := 0
				for _, part := range parts {
					v, _ := strconv.Atoi(part)
					sum += v
				}
				require.Equal(t, strconv.Itoa(sum), p.ExpectedAnswer)
			case 4: // mod: a%b
				require.True(t, strings.HasPrefix(p.PromptSummary, "mod: "))
				parts := strings.Split(strings.TrimPrefix(p.PromptSummary, "mod: "), "%")
				require.Len(t, parts, 2)
				a, _ := strconv.Atoi(parts[0])
				b, _ := strconv.Atoi(parts[1])
				require.Equal(t, strconv.Itoa(a%b), p.ExpectedAnswer)
			case 5: // seq: s,+d
				require.True(t, strings.HasPrefix(p.PromptSummary, "seq: "))
				parts := strings.Split(strings.TrimPrefix(p.PromptSummary, "seq: "), ",+")
				require.Len(t, parts, 2)
				s, _ := strconv.Atoi(parts[0])
				d, _ := strconv.Atoi(parts[1])
				require.Equal(t, strconv.Itoa(s+4*d), p.ExpectedAnswer)
			}
		}
	}

	// Verify batch uniqueness across 60 consecutive calls.
	seen := &sync.Map{}
	uniquePrompts := make(map[string]struct{}, 60)
	for i := 0; i < 60; i++ {
		p, err := generateUniqueWarmupPrompt(seen)
		require.NoError(t, err)
		_, duplicate := uniquePrompts[p.Prompt]
		require.False(t, duplicate, "duplicate warmup prompt generated: %s", p.Prompt)
		uniquePrompts[p.Prompt] = struct{}{}
	}

	// Verify answer matching and rune-safe excerpt truncation.
	require.True(t, matchWarmupExpectedAnswer("The answer is 15,463.", "15463"))
	require.False(t, matchWarmupExpectedAnswer("The answer is 15462.", "15463"))
	longText := strings.Repeat("结果", 80)
	excerpt := formatWarmupResponseExcerpt(longText, 120)
	require.LessOrEqual(t, len([]rune(excerpt)), 120)
	require.True(t, strings.HasSuffix(excerpt, "..."))
}

func TestResolveWarmupPlannedRuns_AutoBucketsAndProArgon(t *testing.T) {
	// #70-style account: claude-opus-5-5 + gemini-4-carbon-{effort}
	acc70 := &Account{
		ID:          70,
		Name:        "brad",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Schedulable: false,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-opus-5-5":   "claude-opus-5-5",
				"claude-sonnet-4-6": "claude-sonnet-4-6",
				"gemini-4-carbon":   "gemini-4-carbon-{effort}",
				"gemini-3-flash":    "gemini-3-flash",
			},
		},
	}
	runs70, err := resolveWarmupPlannedRuns(context.Background(), acc70, WarmupBucketModeAutoBuckets, nil, "low")
	require.NoError(t, err)
	require.Equal(t, []warmupPlannedRun{
		{bucket: "gemini", requestedModel: "gemini-4-carbon"},
		{bucket: "claude", requestedModel: "claude-opus-5-5"},
	}, runs70)

	// #63-style account: claude-opus-4-6-thinking + gemini-4-carbon-{effort}
	acc63 := &Account{
		ID:          63,
		Name:        "acc-63",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Schedulable: false,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
				"gemini-4-carbon":          "gemini-4-carbon-{effort}",
			},
		},
	}
	runs63, err := resolveWarmupPlannedRuns(context.Background(), acc63, WarmupBucketModeAutoBuckets, nil, "low")
	require.NoError(t, err)
	require.Equal(t, []warmupPlannedRun{
		{bucket: "gemini", requestedModel: "gemini-4-carbon"},
		{bucket: "claude", requestedModel: "claude-opus-4-6-thinking"},
	}, runs63)

	// Pro plan with gemini-4-argon mapped alongside gemini-4-carbon includes separate Argon 7d pool.
	accPro := &Account{
		ID:       88,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type": "Pro",
			"model_mapping": map[string]any{
				"gemini-4-carbon": "gemini-4-carbon-{effort}",
				"gemini-4-argon":  "gemini-4-argon-{effort}",
				"claude-opus-5-5": "claude-opus-5-5-{effort}",
			},
		},
	}
	runsPro, err := resolveWarmupPlannedRuns(context.Background(), accPro, WarmupBucketModeAutoBuckets, nil, "low")
	require.NoError(t, err)
	require.Equal(t, []warmupPlannedRun{
		{bucket: "gemini", requestedModel: "gemini-4-carbon"},
		{bucket: "gemini", requestedModel: "gemini-4-argon"},
		{bucket: "claude", requestedModel: "claude-opus-5-5"},
	}, runsPro)

	// Custom models validation against account.GetModelMapping().
	_, err = resolveWarmupPlannedRuns(context.Background(), acc70, WarmupBucketModeCustomModels, []string{"gpt-5-nonexistent"}, "low")
	require.ErrorContains(t, err, "not configured in account model_mapping")
}

func TestBuildWarmupRequestPayload_EffortVsNonEffortAndLowFallback(t *testing.T) {
	effortTestSettings(t)

	account := &Account{
		ID:       70,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"project_id": "proj-70",
			"model_mapping": map[string]any{
				"gemini-4-carbon":          "gemini-4-carbon-{effort}",
				"claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
				"claude-sonnet-4-6":        "claude-sonnet-4-6",
			},
		},
	}

	// 1. {effort} template model: max_tokens=4096, output_config.effort=low, thinking enabled (1024).
	effortBody, effortCtx, resolved, err := buildWarmupRequestPayload(context.Background(), account, "gemini-4-carbon", "", "compute 1+1")
	require.NoError(t, err)
	require.Equal(t, "gemini-4-carbon-low", resolved)
	require.Equal(t, int64(4096), gjson.GetBytes(effortBody, "max_tokens").Int())
	require.Equal(t, "low", gjson.GetBytes(effortBody, "output_config.effort").String())
	require.Equal(t, "enabled", gjson.GetBytes(effortBody, "thinking.type").String())
	require.Equal(t, int64(1024), gjson.GetBytes(effortBody, "thinking.budget_tokens").Int())
	require.True(t, gjson.GetBytes(effortBody, "stream").Bool())
	require.Regexp(t, `^user_[0-9a-f]{64}_account__session_[0-9a-f-]+$`, gjson.GetBytes(effortBody, "metadata.user_id").String())
	singleRetry, _ := SingleAccountRetryFromContext(effortCtx)
	require.True(t, singleRetry)

	// 2. Non-{effort} -thinking suffix model: max_tokens=4096, thinking enabled, NO output_config.
	thinkBody, _, resolvedThink, err := buildWarmupRequestPayload(context.Background(), account, "claude-opus-4-6-thinking", "low", "compute 2+2")
	require.NoError(t, err)
	require.Equal(t, "claude-opus-4-6-thinking", resolvedThink)
	require.Equal(t, int64(4096), gjson.GetBytes(thinkBody, "max_tokens").Int())
	require.False(t, gjson.GetBytes(thinkBody, "output_config").Exists())
	require.Equal(t, "enabled", gjson.GetBytes(thinkBody, "thinking.type").String())
	require.Equal(t, int64(1024), gjson.GetBytes(thinkBody, "thinking.budget_tokens").Int())

	// 3. Plain non-{effort} literal model: max_tokens=512, NO output_config, NO thinking.
	plainBody, _, resolvedPlain, err := buildWarmupRequestPayload(context.Background(), account, "claude-sonnet-4-6", "low", "compute 3+3")
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-6", resolvedPlain)
	require.Equal(t, int64(512), gjson.GetBytes(plainBody, "max_tokens").Int())
	require.False(t, gjson.GetBytes(plainBody, "output_config").Exists())
	require.False(t, gjson.GetBytes(plainBody, "thinking").Exists())

	// 4. Fallback to "high" when "low" is rejected by global AntigravityModelEffortSettings.
	highOnlyPolicy := &AntigravityModelEffortSettings{
		Levels:        []string{"high"},
		DefaultEffort: "high",
	}
	highOnlyCtx := context.WithValue(context.Background(), antigravityEffortPolicyKey{}, highOnlyPolicy)
	fbBody, _, resolvedFallback, err := buildWarmupRequestPayload(highOnlyCtx, account, "gemini-4-carbon", "", "compute 4+4")
	require.NoError(t, err)
	require.Equal(t, "gemini-4-carbon-high", resolvedFallback)
	require.Equal(t, "high", gjson.GetBytes(fbBody, "output_config.effort").String())
}

func TestAccountWarmupService_SinglePausedAccountClearsStaleRateLimitAndRefreshesUsage(t *testing.T) {
	effortTestSettings(t)

	futureReset := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	expiredReset := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	acc70 := &Account{
		ID:          70,
		Name:        "brad",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: false, // paused account must still warm up!
		Credentials: map[string]any{
			"project_id": "proj-brad-70",
			"model_mapping": map[string]any{
				"gemini-4-carbon": "gemini-4-carbon-{effort}",
				"claude-opus-5-5": "claude-opus-5-5-{effort}",
			},
		},
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"gemini-4-carbon-low": map[string]any{
					"rate_limit_reset_at": futureReset,
				},
				"claude-sonnet-4-6": map[string]any{
					"rate_limit_reset_at": expiredReset,
				},
			},
		},
	}

	repo := newWarmupTestAccountRepo(acc70)
	upstream := &warmupMockUpstream{
		handler: func(_ *http.Request, body []byte, _ int64) (*http.Response, error) {
			// Verify 2.31.0 <identity> injection on the wire!
			sysText := gjson.GetBytes(body, "request.systemInstruction.parts.0.text").String()
			require.Contains(t, sysText, "<identity>")
			// Extract the prompt text and compute or echo an answer so answer_matched is verified.
			userPrompt := gjson.GetBytes(body, "request.contents.0.parts.0.text").String()
			require.NotEmpty(t, userPrompt)
			return warmupGeminiSSEResponse("Result: 42."), nil
		},
	}
	gateway := newWarmupGatewayForTest(repo, upstream)
	refresher := &warmupUsageRefresherStub{}

	var recordedSleeps []time.Duration
	var sleepMu sync.Mutex
	warmupSvc := NewAccountWarmupService(
		repo,
		gateway,
		nil,
		WithWarmupUsageRefresher(refresher),
		WithWarmupRandDuration(func(minDur, _ time.Duration) time.Duration { return minDur }),
		WithWarmupSleep(func(_ context.Context, d time.Duration) error {
			sleepMu.Lock()
			recordedSleeps = append(recordedSleeps, d)
			sleepMu.Unlock()
			return nil
		}),
	)

	resp, err := warmupSvc.WarmupAccounts(context.Background(), &WarmupAccountsRequest{
		AccountIDs: []int64{70},
	})
	require.NoError(t, err)
	require.Equal(t, 1, resp.TotalAccounts)
	require.Equal(t, 1, resp.SucceededAccounts)
	require.Equal(t, 0, resp.FailedAccounts)
	require.Equal(t, WarmupBucketModeAutoBuckets, resp.BucketMode)
	require.Len(t, resp.Results, 1)

	acctRes := resp.Results[0]
	require.True(t, acctRes.Success)
	require.False(t, acctRes.Schedulable)
	require.Empty(t, acctRes.Error)
	require.Len(t, acctRes.Runs, 2)
	require.Equal(t, "gemini", acctRes.Runs[0].Bucket)
	require.Equal(t, "gemini-4-carbon", acctRes.Runs[0].RequestedModel)
	require.Equal(t, "gemini-4-carbon-low", acctRes.Runs[0].ResolvedModel)
	require.Equal(t, http.StatusOK, acctRes.Runs[0].StatusCode)
	require.Equal(t, "claude", acctRes.Runs[1].Bucket)
	require.Equal(t, "claude-opus-5-5", acctRes.Runs[1].RequestedModel)
	require.Equal(t, "claude-opus-5-5-low", acctRes.Runs[1].ResolvedModel)
	require.Equal(t, http.StatusOK, acctRes.Runs[1].StatusCode)

	// Prompts across the two buckets must be distinct!
	require.Len(t, upstream.calls, 2)
	p1 := gjson.GetBytes(upstream.calls[0].body, "request.contents.0.parts.0.text").String()
	p2 := gjson.GetBytes(upstream.calls[1].body, "request.contents.0.parts.0.text").String()
	require.NotEqual(t, p1, p2)

	// Stale gemini-4-carbon-low and expired claude-sonnet-4-6 rate limits must be cleared!
	require.Contains(t, repo.clearedModelLimitIDs, int64(70))
	require.NotContains(t, acc70.Extra, "model_rate_limits")

	// Inter-bucket jitter (1200ms) + post-warmup refresh delay (1200ms) + forced usage refresh!
	require.Equal(t, []time.Duration{warmupInterBucketMinJitter, warmupPostRefreshDelay}, recordedSleeps)
	require.Len(t, refresher.calls, 1)
	require.Equal(t, warmupUsageRefreshCall{accountID: 70, force: true}, refresher.calls[0])
	require.NotNil(t, acctRes.Usage)
	require.Contains(t, acctRes.Usage.AntigravityWindows, "gemini_5h")
}

func TestAccountWarmupService_BatchPartialFailureIsolation(t *testing.T) {
	effortTestSettings(t)

	goodAcc := &Account{
		ID:          63,
		Name:        "good-63",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Schedulable: false,
		Credentials: map[string]any{
			"project_id": "proj-63",
			"model_mapping": map[string]any{
				"claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
				"gemini-4-carbon":          "gemini-4-carbon-{effort}",
			},
		},
	}
	apiKeyAcc := &Account{
		ID:       75,
		Name:     "apikey-unsupported",
		Platform: PlatformAntigravity,
		Type:     AccountTypeAPIKey,
	}
	openAIAcc := &Account{
		ID:       76,
		Name:     "openai-unsupported",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}
	rateLimitedAcc := &Account{
		ID:       77,
		Name:     "upstream-429",
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"project_id": "proj-77",
			"model_mapping": map[string]any{
				"gemini-3-flash": "gemini-3-flash",
			},
		},
	}

	repo := newWarmupTestAccountRepo(goodAcc, apiKeyAcc, openAIAcc, rateLimitedAcc)
	upstream := &warmupMockUpstream{
		handler: func(_ *http.Request, _ []byte, accountID int64) (*http.Response, error) {
			if accountID == 77 {
				errPayload, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"code":    429,
						"status":  "RESOURCE_EXHAUSTED",
						"message": "Resource has been exhausted (e.g. check quota).",
					},
				})
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(string(errPayload))),
				}, nil
			}
			return warmupGeminiSSEResponse("123"), nil
		},
	}

	gateway := newWarmupGatewayForTest(repo, upstream)
	refresher := &warmupUsageRefresherStub{
		fn: func(accountID int64, force bool) (*UsageInfo, error) {
			require.True(t, force)
			if accountID == 63 {
				return &UsageInfo{Source: "active"}, nil
			}
			return nil, errors.New("unexpected refresh for failed account")
		},
	}

	warmupSvc := NewAccountWarmupService(
		repo,
		gateway,
		nil,
		WithWarmupUsageRefresher(refresher),
		WithWarmupRandDuration(func(minDur, _ time.Duration) time.Duration { return minDur }),
		WithWarmupSleep(func(_ context.Context, _ time.Duration) error { return nil }),
	)

	resp, err := warmupSvc.WarmupAccounts(context.Background(), &WarmupAccountsRequest{
		AccountIDs:  []int64{63, 75, 76, 77, 999},
		Concurrency: 4,
	})
	require.NoError(t, err)
	require.Equal(t, 5, resp.TotalAccounts)
	require.Equal(t, 1, resp.SucceededAccounts)
	require.Equal(t, 4, resp.FailedAccounts)

	// Preserves caller order across concurrent batch workers!
	require.Equal(t, int64(63), resp.Results[0].AccountID)
	require.True(t, resp.Results[0].Success)
	require.NotNil(t, resp.Results[0].Usage)

	require.Equal(t, int64(75), resp.Results[1].AccountID)
	require.False(t, resp.Results[1].Success)
	require.Contains(t, resp.Results[1].Error, "apikey")

	require.Equal(t, int64(76), resp.Results[2].AccountID)
	require.False(t, resp.Results[2].Success)
	require.Contains(t, resp.Results[2].Error, "antigravity")

	require.Equal(t, int64(77), resp.Results[3].AccountID)
	require.False(t, resp.Results[3].Success)
	require.Len(t, resp.Results[3].Runs, 1)
	require.Equal(t, http.StatusTooManyRequests, resp.Results[3].Runs[0].StatusCode)

	require.Equal(t, int64(999), resp.Results[4].AccountID)
	require.False(t, resp.Results[4].Success)
	require.Contains(t, resp.Results[4].Error, "999")

	// Only account 63 succeeded, so only account 63 triggered usage refresh!
	require.Len(t, refresher.calls, 1)
	require.Equal(t, int64(63), refresher.calls[0].accountID)
}
