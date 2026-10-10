package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func effortTestAccount(mapping map[string]string) *Account {
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	raw := map[string]any{}
	for key, value := range mapping {
		raw[key] = value
	}
	account.Credentials["model_mapping"] = raw
	return account
}

func effortTestSettings(t *testing.T, additional ...string) (*SettingService, *codexVersionSyncSettingRepoStub) {
	t.Helper()
	old := antigravityEffortSettingsSnapshot.Load()
	t.Cleanup(func() { antigravityEffortSettingsSnapshot.Store(old) })
	repo := newCodexVersionSyncSettingRepoStub(nil)
	svc := NewSettingService(repo, nil)
	settings := defaultAntigravityModelEffortSettings()
	settings.Levels = append(settings.Levels, additional...)
	require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), settings))
	return svc, repo
}

func TestAntigravityEffortRealForward(t *testing.T) {
	effortTestSettings(t, "ultra")
	for _, model := range []string{"gemini-3.8-flash", "opus5.5", "bespoke-model"} {
		// All protocols carry the same opt-in rule; no Gemini admission branch.
		for _, protocol := range []string{"chat", "responses", "messages", "gemini"} {
			for _, effort := range []string{"low", "medium", "high", ""} {
				t.Run(model+"/"+protocol+"/"+effort, func(t *testing.T) { runEffortForward(t, model, protocol, effort) })
			}
		}
		for _, effort := range []string{"xhigh", "max", "ultra"} {
			t.Run(model+"/future/"+effort, func(t *testing.T) { runEffortForward(t, model, "chat", effort) })
		}
	}
}

func runEffortForward(t *testing.T, model, protocol, effort string, contexts ...context.Context) {
	t.Helper()
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	switch protocol {
	case "chat":
		if effort != "" {
			body["reasoning_effort"] = effort
		}
	case "responses":
		body["input"] = "hello"
		if effort != "" {
			body["reasoning"] = map[string]any{"effort": effort}
		}
	case "messages":
		body["max_tokens"] = 200
		if effort != "" {
			body["output_config"] = map[string]any{"effort": effort}
		}
	case "gemini":
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}}
		if effort != "" {
			body["generationConfig"] = map[string]any{"thinkingConfig": map[string]any{"thinkingLevel": effort}}
		}
	}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	account := effortTestAccount(map[string]string{model: model + "-{effort}"})
	ctx := context.Background()
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	ctx = WithAntigravityRequestEffort(ctx, encoded)
	require.True(t, (&GatewayService{}).isModelSupportedByAccountWithContext(ctx, account, model))
	require.True(t, account.IsModelSupported(model+"-low"))
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/messages", encoded)
	var result *ForwardResult
	switch protocol {
	case "chat":
		result, err = svc.ForwardAsChatCompletions(ctx, c, account, encoded, nil)
	case "responses":
		result, err = svc.ForwardAsResponses(ctx, c, account, encoded, nil)
	case "messages":
		result, err = svc.Forward(ctx, c, account, encoded, false)
	case "gemini":
		result, err = svc.ForwardGemini(ctx, c, account, model, "generateContent", false, encoded, false)
	}
	require.NoError(t, err)
	require.Len(t, upstream.requestBodies, 1)
	if effort == "" {
		effort = "medium"
	}
	require.Equal(t, model+"-"+effort, gjson.GetBytes(upstream.requestBodies[0], "model").String())
	require.Equal(t, model+"-"+effort, result.UpstreamModel)
	require.Equal(t, model, result.Model)
}

func TestAntigravityEffortMappingAndScheduling(t *testing.T) {
	effortTestSettings(t, "ultra", "extra-low")
	base := "any-family"
	rule := base + "-{effort}"
	cases := []struct {
		name, request, effort, want string
		mapping                     map[string]string
	}{
		{"base", base, "low", base + "-low", map[string]string{base: rule}},
		{"shared default", base, "", base + "-medium", map[string]string{base: rule}},
		{"explicit default", base, "", base + "-high", map[string]string{base: base + "-{effort:high}"}},
		{"suffix wins", base + "-low", "high", base + "-low", map[string]string{base: rule}},
		{"longest level", base + "-extra-low", "max", base + "-extra-low", map[string]string{base: rule}},
		{"longest level before overlapping base", "family-extra-low", "", "primary-extra-low", map[string]string{"family": "primary-{effort}", "family-extra": "secondary-{effort}"}},
		{"future suffix", base + "-ultra", "", base + "-ultra", map[string]string{base: rule}},
		{"literal suffix override", base, "medium", "special-target", map[string]string{base: rule, base + "-medium": "special-target"}},
		{"alias one step", "alias", "high", base + "-low", map[string]string{"alias": base + "-low", base + "-low": "wrong-second-map"}},
		{"literal target unchanged", base, "high", "different-model", map[string]string{base: "different-model"}},
		{"wildcard wins", base, "medium", "wildcard-target", map[string]string{"any-*": "wildcard-target"}},
		{"unknown rejected", base, "banana", "", map[string]string{base: rule}},
		{"unconfigured literal", base, "high", base, map[string]string{base: base}},
		{"literal no suffix permission", base + "-high", "", "", map[string]string{base: base}},
		{"independent image", base + "-image", "high", base + "-image", map[string]string{base + "-image": base + "-image"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			account := effortTestAccount(tt.mapping)
			ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"`+tt.effort+`"}`))
			require.Equal(t, tt.want, resolveFinalAntigravityModelKey(ctx, account, tt.request))
			require.Equal(t, tt.want != "", (&GatewayService{}).isModelSupportedByAccountWithContext(ctx, account, tt.request))
		})
	}
	account := effortTestAccount(map[string]string{base: rule})
	require.True(t, account.IsModelSupported(base+"-ultra"))
	for _, platform := range []string{PlatformGemini, PlatformOpenAI, PlatformAnthropic} {
		account.Platform = platform
		require.False(t, account.IsModelSupported(base+"-ultra"))
		require.Equal(t, rule, account.GetMappedModel(base))
	}
}

func TestAntigravityEffortSettingsLoadUpdateSnapshot(t *testing.T) {
	svc, repo := effortTestSettings(t, "ultra")
	account := effortTestAccount(map[string]string{"custom": "custom-{effort}"})
	body := []byte(`{"reasoning_effort":"ultra"}`)
	inFlight := WithAntigravityRequestEffort(context.Background(), body)
	settings := defaultAntigravityModelEffortSettings()
	require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), settings))
	require.False(t, account.IsModelSupported("custom-ultra"), "cached account mapping must not cache old level set")
	require.Equal(t, "custom-ultra", resolveFinalAntigravityModelKey(inFlight, account, "custom"), "one request retains its policy")
	runEffortForward(t, "custom", "responses", "ultra", inFlight)
	require.Equal(t, "", resolveFinalAntigravityModelKey(WithAntigravityRequestEffort(context.Background(), body), account, "custom"))
	settings.Levels = append(settings.Levels, "ultra")
	require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), settings))
	require.True(t, account.IsModelSupported("custom-ultra"))
	restarted := NewSettingService(repo, nil)
	antigravityEffortSettingsSnapshot.Store(nil)
	require.NoError(t, restarted.LoadAntigravityModelEffortSettings(context.Background()))
	require.True(t, account.IsModelSupported("custom-ultra"))
	require.Contains(t, currentAntigravityModelEffortSettings().Levels, "none")
	for _, bad := range []AntigravityModelEffortSettings{{Levels: []string{}, DefaultEffort: "medium"}, {Levels: []string{"high"}, DefaultEffort: "medium"}, {Levels: []string{"medium", "medium"}, DefaultEffort: "medium"}, {Levels: []string{"medium", "bad name"}, DefaultEffort: "medium"}} {
		require.ErrorIs(t, svc.SetAntigravityModelEffortSettings(context.Background(), bad), ErrInvalidAntigravityModelEffort)
	}
	repo.setErr = errors.New("write failed")
	require.Error(t, svc.SetAntigravityModelEffortSettings(context.Background(), defaultAntigravityModelEffortSettings()))
	require.True(t, account.IsModelSupported("custom-ultra"), "failed persistence cannot publish")
	repo.setErr = nil
	repo.values[SettingKeyAntigravityModelEffort] = `{"levels":["medium"],"default_effort":"missing"}`
	require.Error(t, restarted.LoadAntigravityModelEffortSettings(context.Background()))
	require.True(t, account.IsModelSupported("custom-ultra"), "invalid load cannot replace valid snapshot")
	repo.values = map[string]string{}
	require.NoError(t, restarted.LoadAntigravityModelEffortSettings(context.Background()))
	require.False(t, account.IsModelSupported("custom-ultra"), "absent setting loads default seed")
}

func TestAntigravityEffortConcurrentPolicyReads(t *testing.T) {
	svc, _ := effortTestSettings(t)
	account := effortTestAccount(map[string]string{"model": "model-{effort}"})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"high"}`))
				if got := resolveFinalAntigravityModelKey(ctx, account, "model"); got != "model-high" {
					t.Errorf("unexpected model %q", got)
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		value := defaultAntigravityModelEffortSettings()
		if i%2 == 0 {
			value.Levels = append(value.Levels, "ultra")
		}
		require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), value))
	}
	wg.Wait()
}

func TestAntigravityEffortUnknownForwardAndRateLimit(t *testing.T) {
	effortTestSettings(t)
	for _, base := range []string{"gemini-3.8-flash", "opus5.5", "custom"} {
		for _, limited := range []bool{false, true} {
			account := effortTestAccount(map[string]string{base: base + "-{effort}"})
			if limited {
				now := time.Now()
				setAccountModelRateLimitSnapshot(account, base, now.Add(time.Minute), "fixture", now)
			}
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{antigravityCompatSuccessResponse()}}
			svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
			body := []byte(`{"model":"` + base + `-low","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`)
			c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)
			_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			if limited {
				require.Error(t, err)
				require.Zero(t, upstream.callCount)
			} else {
				require.NoError(t, err)
				require.Equal(t, base+"-low", gjson.GetBytes(upstream.requestBodies[0], "model").String())
			}
		}
		account := effortTestAccount(map[string]string{base: base + "-{effort}"})
		now := time.Now()
		setAccountModelRateLimitSnapshot(account, base+"-high", now.Add(time.Minute), "fixture", now)
		ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"low"}`))
		require.False(t, account.isModelRateLimitedWithContext(ctx, base), "unrelated exact variant must not filter low")
		upstream := &queuedHTTPUpstreamStub{}
		svc := newAntigravityCompatService(config.GatewayConfig{}, upstream)
		body := []byte(`{"model":"` + base + `","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"banana"}`)
		c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)
		_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Contains(t, recorder.Body.String(), "not enabled")
		require.Zero(t, upstream.callCount)
	}
	account := effortTestAccount(map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-{effort}"})
	now := time.Now()
	setAccountModelRateLimitSnapshot(account, antigravityGeminiModelRateLimitKey, now.Add(time.Minute), "fixture", now)
	require.True(t, account.isModelRateLimitedWithContext(context.Background(), "gemini-3.8-flash-low"))
}

func TestCompactAntigravityEffortMapping(t *testing.T) {
	effortTestSettings(t)
	base := "custom"
	before := map[string]string{base: base + "-{effort}", base + "-low": base + "-low", base + "-high": base + "-high", "alias": "target", "custom-image": "custom-image"}
	after, conflicts := CompactAntigravityEffortMapping(before)
	require.Empty(t, conflicts)
	require.Len(t, after, len(before)-2)
	for _, model := range []string{base, base + "-low", base + "-high"} {
		a, okA := resolveAntigravityEffortMapping(before, model, "")
		b, okB := resolveAntigravityEffortMapping(after, model, "")
		require.Equal(t, a, b)
		require.Equal(t, okA, okB)
	}
	before[base+"-low"] = "different"
	after, conflicts = CompactAntigravityEffortMapping(before)
	require.Equal(t, []string{base + "-low"}, conflicts)
	require.Equal(t, "different", after[base+"-low"])
	before["custom-*"] = "wildcard"
	after, conflicts = CompactAntigravityEffortMapping(before)
	require.Len(t, conflicts, 2)
	require.Equal(t, before, after)
}

func TestAntigravityEffortRebuildAndValidation(t *testing.T) {
	effortTestSettings(t, "extra-low")
	before := map[string]string{"old-preview": "family-high", "family-low": "family-low", "family-image": "family-image", "family-lite": "family-lite", "family-agent": "family-agent", "family-tiered": "family-tiered"}
	after, err := RebuildAntigravityModelMapping(before, map[string]string{"family": "medium"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"family": "family-{effort}", "family-image": "family-image", "family-lite": "family-lite", "family-agent": "family-agent", "family-tiered": "family-tiered"}, after)
	_, err = RebuildAntigravityModelMapping(before, map[string]string{"absent": "medium"})
	require.Error(t, err)
	for _, target := range []string{"model-{effort}", "model-{effort:low}"} {
		require.NoError(t, ValidateAntigravityEffortModelMapping(map[string]any{"model_mapping": map[string]any{"model": target}}))
	}
	for _, target := range []string{"model-{effort:unknown}", "model-{effort}-high", "model-{effort}{effort}", "model-{effort:}"} {
		require.Error(t, ValidateAntigravityEffortModelMapping(map[string]any{"model_mapping": map[string]any{"model": target}}))
	}
	require.Error(t, ValidateAntigravityEffortModelMapping(map[string]any{"model_mapping": map[string]any{"model-*": "model-{effort}"}}))
}

func TestAntigravityEffortCaptureScope(t *testing.T) {
	effortTestSettings(t, "ultra")
	for _, finalModel := range []string{"gemini-3.8-flash-low", "gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-ultra", "gemini-3.8-flash-tiered", "opus5.5-low"} {
		t.Run(finalModel, func(t *testing.T) {
			lease := filepath.Join(t.TempDir(), "lease.json")
			writeCaptureLease(t, lease, t.TempDir(), true, time.Now().Add(time.Hour), "match-session")
			controller := NewGeminiCapture(&config.Config{Gateway: config.GatewayConfig{GeminiCaptureLeaseFile: lease}})
			body := []byte(`{"model":"gemini-3.8-flash"}`)
			require.Nil(t, controller.Begin(newCaptureTestContext(), "/v1/messages", body, "other-session", GeminiCaptureTargetModel, false, 1))
			capture := controller.Begin(newCaptureTestContext(), "/v1/messages", body, "match-session", GeminiCaptureTargetModel, false, 1)
			require.NotNil(t, capture)
			allowed := finalModel == "gemini-3.8-flash-low" || finalModel == "gemini-3.8-flash-medium" || finalModel == "gemini-3.8-flash-high"
			require.Equal(t, allowed, capture.Activate(effortTestAccount(map[string]string{GeminiCaptureTargetModel: GeminiCaptureTargetModel + "-{effort}"}), finalModel, []byte(`{"contents":[]}`)))
			capture.Finish(http.StatusOK)
		})
	}
}

func TestAntigravityEffortCompositeClaimsAndSnapshot(t *testing.T) {
	svc, _ := effortTestSettings(t, "ultra")
	account := effortTestAccount(map[string]string{"opus5.5": "opus5.5-{effort}"})
	ctx := WithAntigravityRequestEffort(context.Background(), []byte(`{"reasoning_effort":"low"}`))
	require.True(t, (&AntigravityGatewayService{}).IsModelSupported("opus5.5-low", account))
	require.True(t, explicitModelMappingClaimsWithContext(ctx, *account, "opus5.5-ultra"))
	require.NoError(t, svc.SetAntigravityModelEffortSettings(context.Background(), defaultAntigravityModelEffortSettings()))
	require.True(t, explicitModelMappingClaimsWithContext(ctx, *account, "opus5.5-ultra"), "in-flight composite routing retains its snapshot")
	require.False(t, explicitModelMappingClaims(*account, "opus5.5-ultra"))
	require.True(t, (&GeminiMessagesCompatService{}).isModelSupportedByAccountWithContext(ctx, account, "opus5.5-ultra"))
	require.False(t, (&GeminiMessagesCompatService{}).isModelSupportedByAccount(account, "opus5.5-ultra"))
}
