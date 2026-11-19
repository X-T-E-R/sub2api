package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOpenAIAPIKeyAccount(id int64, name string, priority int, baseURL, apiKey string, mapping map[string]any, useResponses bool) *service.Account {
	extra := map[string]any{}
	if useResponses {
		extra["use_responses_api"] = true
	} else {
		extra["openai_responses_supported"] = false
	}
	return &service.Account{
		ID:          id,
		Name:        name,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Priority:    priority,
		Concurrency: 4,
		Credentials: map[string]any{
			"api_key":       apiKey,
			"base_url":      baseURL,
			"model_mapping": mapping,
		},
		Extra: extra,
	}
}

func openAIGroup(id int64) *service.Group {
	return &service.Group{
		ID:       id,
		Name:     "openai-e2e-group",
		Platform: service.PlatformOpenAI,
		Status:   service.StatusActive,
		Hydrated: true,
	}
}

func TestOpenAI_ModelMappingAndChannelOverrideRewrites(t *testing.T) {
	const groupID int64 = 301
	group := openAIGroup(groupID)
	channel := service.Channel{
		ID:                 11,
		Name:               "openai-override-channel",
		Status:             service.StatusActive,
		BillingModelSource: service.BillingModelSourceRequested,
		GroupIDs:           []int64{groupID},
		ModelMapping: map[string]map[string]string{
			service.PlatformOpenAI: {
				"public-gpt": "channel-gpt-5",
			},
		},
	}

	t.Run("responses_applies_channel_override_then_account_mapping", func(t *testing.T) {
		acc := newOpenAIAPIKeyAccount(311, "oa-resp", 1, "https://api.openai.com/v1", "sk-resp-1", map[string]any{
			"channel-gpt-5": "gpt-5.4",
		}, true)
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    group,
			Accounts: []*service.Account{acc},
			Channels: []service.Channel{channel},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return jsonHTTPResponse(http.StatusOK, `{
					"id": "resp_1",
					"object": "response",
					"status": "completed",
					"model": "gpt-5.4",
					"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"mapped-responses-ok"}]}],
					"usage": {"input_tokens": 100, "output_tokens": 35, "input_tokens_details": {"cached_tokens": 20}}
				}`), nil
			},
		})

		rec := h.post(t, "/v1/responses", nil, `{"model":"public-gpt","stream":false,"input":"hello"}`)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "mapped-responses-ok")

		reqs := h.requests()
		require.Len(t, reqs, 1)
		require.Equal(t, "https://api.openai.com/v1/responses", reqs[0].URL)
		require.Equal(t, "Bearer sk-resp-1", reqs[0].Headers.Get("Authorization"))
		require.Equal(t, "gpt-5.4", gjson.GetBytes(reqs[0].Body, "model").String())

		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, acc.ID, logs[0].AccountID)
		require.Equal(t, "public-gpt", logs[0].RequestedModel)
		require.Equal(t, "channel-gpt-5", logs[0].Model)
		requireUpstreamModel(t, logs[0], "gpt-5.4")
		require.NotNil(t, logs[0].ModelMappingChain)
		require.Equal(t, "public-gpt→channel-gpt-5→gpt-5.4", *logs[0].ModelMappingChain)
		require.Equal(t, 80, logs[0].InputTokens)
		require.Equal(t, 20, logs[0].CacheReadTokens)
		require.Equal(t, 35, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})

	t.Run("chat_completions_raw_applies_channel_override_then_account_mapping", func(t *testing.T) {
		acc := newOpenAIAPIKeyAccount(312, "oa-cc-raw", 1, "https://relay.example.test/v1", "sk-cc-1", map[string]any{
			"channel-gpt-5": "upstream-cc-model",
		}, false)
		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    group,
			Accounts: []*service.Account{acc},
			Channels: []service.Channel{channel},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return jsonHTTPResponse(http.StatusOK, `{
					"id": "chatcmpl_1",
					"object": "chat.completion",
					"model": "upstream-cc-model",
					"choices": [{"index":0,"message":{"role":"assistant","content":"mapped-cc-ok"},"finish_reason":"stop"}],
					"usage": {"prompt_tokens": 60, "completion_tokens": 18, "total_tokens": 78}
				}`), nil
			},
		})

		rec := h.post(t, "/v1/chat/completions", nil, `{"model":"public-gpt","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "mapped-cc-ok")

		reqs := h.requests()
		require.Len(t, reqs, 1)
		require.Equal(t, "https://relay.example.test/v1/chat/completions", reqs[0].URL)
		require.Equal(t, "upstream-cc-model", gjson.GetBytes(reqs[0].Body, "model").String())

		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, acc.ID, logs[0].AccountID)
		require.Equal(t, "public-gpt", logs[0].RequestedModel)
		require.Equal(t, "channel-gpt-5", logs[0].Model)
		requireUpstreamModel(t, logs[0], "upstream-cc-model")
		require.NotNil(t, logs[0].ModelMappingChain)
		require.Equal(t, "public-gpt→channel-gpt-5→upstream-cc-model", *logs[0].ModelMappingChain)
		require.Equal(t, 60, logs[0].InputTokens)
		require.Equal(t, 18, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})
}

func TestOpenAIResponses_ReasoningEffortAndFieldNormalizationMatrix(t *testing.T) {
	cases := []struct {
		name               string
		baseURL            string
		body               string
		wantEffortExists   bool
		wantEffort         string
		wantMaxOutTokens   int64
		wantNoMaxTokens    bool
		wantNoUnsupported  bool
	}{
		{
			name:              "official_host_normalizes_minimal_to_none_and_max_tokens_to_max_output_tokens",
			baseURL:           "https://api.openai.com/v1",
			body:              `{"model":"gpt-5.4","stream":false,"max_tokens":512,"max_completion_tokens":256,"prompt_cache_retention":"24h","safety_identifier":"u-1","reasoning":{"effort":"minimal"},"input":"hello"}`,
			wantEffortExists:  true,
			wantEffort:        "none",
			wantMaxOutTokens:  512,
			wantNoMaxTokens:   true,
			wantNoUnsupported: true,
		},
		{
			name:             "official_host_preserves_explicit_none_effort",
			baseURL:          "https://api.openai.com/v1",
			body:             `{"model":"gpt-5.4","stream":false,"reasoning":{"effort":"none"},"input":"hello"}`,
			wantEffortExists: true,
			wantEffort:       "none",
		},
		{
			name:             "custom_compatible_host_strips_catalog_none_effort_placeholder",
			baseURL:          "https://relay.example.test/v1",
			body:             `{"model":"gpt-5.4","stream":false,"reasoning":{"effort":"none"},"input":"hello"}`,
			wantEffortExists: false,
		},
		{
			name:             "custom_compatible_host_keeps_non_none_effort",
			baseURL:          "https://relay.example.test/v1",
			body:             `{"model":"gpt-5.4","stream":false,"reasoning":{"effort":"high"},"input":"hello"}`,
			wantEffortExists: true,
			wantEffort:       "high",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newOpenAIAPIKeyAccount(int64(320+i), "oa-norm", 1, tc.baseURL, "sk-norm", nil, true)
			h := newGatewayHarness(t, gatewayHarnessConfig{
				Group:    openAIGroup(int64(320 + i)),
				Accounts: []*service.Account{acc},
				Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
					return jsonHTTPResponse(http.StatusOK, `{"id":"resp_norm","status":"completed","usage":{"input_tokens":12,"output_tokens":4}}`), nil
				},
			})

			rec := h.post(t, "/v1/responses", nil, tc.body)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

			reqs := h.requests()
			require.Len(t, reqs, 1)
			out := reqs[0].Body

			effort := gjson.GetBytes(out, "reasoning.effort")
			require.Equal(t, tc.wantEffortExists, effort.Exists(), "outbound=%s", string(out))
			if tc.wantEffortExists {
				require.Equal(t, tc.wantEffort, effort.String())
			} else {
				require.False(t, gjson.GetBytes(out, "reasoning").Exists(), "empty reasoning object must be stripped")
			}
			if tc.wantMaxOutTokens > 0 {
				require.Equal(t, tc.wantMaxOutTokens, gjson.GetBytes(out, "max_output_tokens").Int())
			}
			if tc.wantNoMaxTokens {
				require.False(t, gjson.GetBytes(out, "max_tokens").Exists())
				require.False(t, gjson.GetBytes(out, "max_completion_tokens").Exists())
			}
			if tc.wantNoUnsupported {
				require.False(t, gjson.GetBytes(out, "prompt_cache_retention").Exists())
				require.False(t, gjson.GetBytes(out, "safety_identifier").Exists())
			}
			h.assertSlotsBalanced(t)
		})
	}
}

func TestOpenAIChatCompletions_StreamingInjectsIncludeUsageAndAccountsUsage(t *testing.T) {
	acc := newOpenAIAPIKeyAccount(330, "oa-stream-cc", 1, "https://relay.example.test/v1", "sk-stream", map[string]any{
		"gpt-chat": "upstream-gpt-chat",
	}, false)

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Group:    openAIGroup(330),
		Accounts: []*service.Account{acc},
		Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
			return sseHTTPResponse(
				`data: {"id":"chatcmpl-s1","object":"chat.completion.chunk","model":"upstream-gpt-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "},"finish_reason":null}]}`,
				``,
				`data: {"id":"chatcmpl-s1","object":"chat.completion.chunk","model":"upstream-gpt-chat","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}`,
				``,
				`data: {"id":"chatcmpl-s1","object":"chat.completion.chunk","model":"upstream-gpt-chat","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165,"prompt_tokens_details":{"cached_tokens":30}}}`,
				``,
				`data: [DONE]`,
			), nil
		},
	})

	rec := h.post(t, "/v1/chat/completions", nil, `{"model":"gpt-chat","stream":true,"messages":[{"role":"user","content":"say hi"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream"))
	require.Contains(t, rec.Body.String(), "hello ")
	require.Contains(t, rec.Body.String(), "world")
	require.Contains(t, rec.Body.String(), "[DONE]")

	reqs := h.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "upstream-gpt-chat", gjson.GetBytes(reqs[0].Body, "model").String())
	require.True(t, gjson.GetBytes(reqs[0].Body, "stream_options.include_usage").Bool(),
		"raw streaming chat completions must inject stream_options.include_usage=true")

	logs := h.usageRecords()
	require.Len(t, logs, 1)
	require.Equal(t, acc.ID, logs[0].AccountID)
	requireUpstreamModel(t, logs[0], "upstream-gpt-chat")
	require.Equal(t, 90, logs[0].InputTokens)
	require.Equal(t, 30, logs[0].CacheReadTokens)
	require.Equal(t, 45, logs[0].OutputTokens)
	h.assertSlotsBalanced(t)
}

func TestOpenAI_MultiAccountFailoverWithDistinctModelMappings(t *testing.T) {
	t.Run("responses_failover_preserves_distinct_account_mappings_and_channel_override", func(t *testing.T) {
		const groupID int64 = 340
		group := openAIGroup(groupID)
		channel := service.Channel{
			ID:                 21,
			Name:               "failover-channel",
			Status:             service.StatusActive,
			BillingModelSource: service.BillingModelSourceRequested,
			GroupIDs:           []int64{groupID},
			ModelMapping: map[string]map[string]string{
				service.PlatformOpenAI: {
					"client-gpt": "canonical-gpt",
				},
			},
		}
		acc1 := newOpenAIAPIKeyAccount(341, "oa-a", 1, "https://provider-a.example.test/v1", "sk-acct-a", map[string]any{
			"canonical-gpt": "vendor-a-gpt-v1",
		}, true)
		acc2 := newOpenAIAPIKeyAccount(342, "oa-b", 2, "https://provider-b.example.test/v1", "sk-acct-b", map[string]any{
			"canonical-gpt": "vendor-b-gpt-v2",
		}, true)

		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    group,
			Accounts: []*service.Account{acc1, acc2},
			Channels: []service.Channel{channel},
			Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
				if req.AccountID == acc1.ID {
					return jsonHTTPResponse(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"rate limit reached"}}`), nil
				}
				return jsonHTTPResponse(http.StatusOK, `{
					"id": "resp_backup",
					"object": "response",
					"status": "completed",
					"model": "vendor-b-gpt-v2",
					"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"backup-responses-ok"}]}],
					"usage": {"input_tokens": 55, "output_tokens": 19}
				}`), nil
			},
		})

		rec := h.post(t, "/v1/responses", nil, `{"model":"client-gpt","stream":false,"reasoning":{"effort":"high"},"input":"failover test"}`)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "backup-responses-ok")

		reqs := h.requests()
		require.Len(t, reqs, 2)

		// Attempt 1 went to provider A with vendor-a-gpt-v1.
		require.Equal(t, acc1.ID, reqs[0].AccountID)
		require.Equal(t, "https://provider-a.example.test/v1/responses", reqs[0].URL)
		require.Equal(t, "Bearer sk-acct-a", reqs[0].Headers.Get("Authorization"))
		require.Equal(t, "vendor-a-gpt-v1", gjson.GetBytes(reqs[0].Body, "model").String())

		// Attempt 2 went to provider B with vendor-b-gpt-v2 (uncorrupted by attempt 1!).
		require.Equal(t, acc2.ID, reqs[1].AccountID)
		require.Equal(t, "https://provider-b.example.test/v1/responses", reqs[1].URL)
		require.Equal(t, "Bearer sk-acct-b", reqs[1].Headers.Get("Authorization"))
		require.Equal(t, "vendor-b-gpt-v2", gjson.GetBytes(reqs[1].Body, "model").String())
		require.Equal(t, "high", gjson.GetBytes(reqs[1].Body, "reasoning.effort").String())

		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, acc2.ID, logs[0].AccountID)
		require.Equal(t, "client-gpt", logs[0].RequestedModel)
		require.Equal(t, "canonical-gpt", logs[0].Model)
		requireUpstreamModel(t, logs[0], "vendor-b-gpt-v2")
		require.NotNil(t, logs[0].ModelMappingChain)
		require.Equal(t, "client-gpt→canonical-gpt→vendor-b-gpt-v2", *logs[0].ModelMappingChain)
		require.Equal(t, 55, logs[0].InputTokens)
		require.Equal(t, 19, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})

	t.Run("chat_completions_failover_across_distinct_mappings_and_protocols", func(t *testing.T) {
		const groupID int64 = 350
		group := openAIGroup(groupID)
		// Account 1 uses raw /v1/chat/completions and fails with 502;
		// Account 2 uses /v1/responses bridge with a different model_mapping and succeeds.
		acc1 := newOpenAIAPIKeyAccount(351, "oa-cc-primary", 1, "https://cc-primary.example.test/v1", "sk-cc-1", map[string]any{
			"public-chat": "cc-vendor-alpha",
		}, false)
		acc2 := newOpenAIAPIKeyAccount(352, "oa-resp-backup", 2, "https://resp-backup.example.test/v1", "sk-resp-2", map[string]any{
			"public-chat": "resp-vendor-beta",
		}, true)

		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    group,
			Accounts: []*service.Account{acc1, acc2},
			Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
				if req.AccountID == acc1.ID {
					return jsonHTTPResponse(http.StatusBadGateway, `{"error":{"type":"server_error","message":"upstream overloaded"}}`), nil
				}
				// Responses bridge always sends stream=true to upstream /v1/responses and collects for non-stream client.
				return sseHTTPResponse(
					`event: response.completed`,
					`data: {"type":"response.completed","response":{"id":"resp_cc_bridge","status":"completed","model":"resp-vendor-beta","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"cc-failover-to-responses-ok"}]}],"usage":{"input_tokens":40,"output_tokens":14}}}`,
					``,
				), nil
			},
		})

		rec := h.post(t, "/v1/chat/completions", nil, `{"model":"public-chat","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "cc-failover-to-responses-ok")

		reqs := h.requests()
		require.Len(t, reqs, 2)

		// Attempt 1: raw chat completions with acc1's model.
		require.Equal(t, acc1.ID, reqs[0].AccountID)
		require.Equal(t, "https://cc-primary.example.test/v1/chat/completions", reqs[0].URL)
		require.Equal(t, "cc-vendor-alpha", gjson.GetBytes(reqs[0].Body, "model").String())

		// Attempt 2: bridged /v1/responses with acc2's model (never polluted by acc1).
		require.Equal(t, acc2.ID, reqs[1].AccountID)
		require.Equal(t, "https://resp-backup.example.test/v1/responses", reqs[1].URL)
		require.Equal(t, "resp-vendor-beta", gjson.GetBytes(reqs[1].Body, "model").String())

		logs := h.usageRecords()
		require.Len(t, logs, 1)
		require.Equal(t, acc2.ID, logs[0].AccountID)
		requireUpstreamModel(t, logs[0], "resp-vendor-beta")
		require.Equal(t, 40, logs[0].InputTokens)
		require.Equal(t, 14, logs[0].OutputTokens)
		h.assertSlotsBalanced(t)
	})
}
