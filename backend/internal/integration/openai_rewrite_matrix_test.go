package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gorilla/websocket"
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

func TestOpenAI_UnifiedChannelAndAccountModelMappingMatrix(t *testing.T) {
	type routeSpec struct {
		name      string
		path      string
		makeBody  func(model string) string
		okResp    func(upstreamModel string) *http.Response
		useResp   bool
		expectURL string
	}

	routes := []routeSpec{
		{
			name: "responses",
			path: "/v1/responses",
			makeBody: func(model string) string {
				return `{"model":"` + model + `","stream":false,"input":[{"role":"user","content":"matrix"}]}`
			},
			okResp: func(upstreamModel string) *http.Response {
				return jsonHTTPResponse(http.StatusOK, `{"id":"resp_mat","object":"response","status":"completed","model":"`+upstreamModel+`","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok-`+upstreamModel+`"}]}],"usage":{"input_tokens":20,"output_tokens":7}}`)
			},
			useResp:   true,
			expectURL: "/responses",
		},
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			makeBody: func(model string) string {
				return `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"matrix"}]}`
			},
			okResp: func(upstreamModel string) *http.Response {
				return jsonHTTPResponse(http.StatusOK, `{"id":"chatcmpl_mat","object":"chat.completion","model":"`+upstreamModel+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok-`+upstreamModel+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27}}`)
			},
			useResp:   false,
			expectURL: "/chat/completions",
		},
	}

	for _, rt := range routes {
		rt := rt
		t.Run(rt.name, func(t *testing.T) {
			const (
				clientAlias        = "client-alias-gpt"
				channelMappedModel = "channel-canon-gpt"
				upstreamModelA     = "vendor-a-model"
				upstreamModelB     = "vendor-b-model"
			)

			// (a) Account A maps only clientAlias -> upstreamModelA.
			t.Run("a_account_maps_client_alias_only", func(t *testing.T) {
				const gid int64 = 360
				accA := newOpenAIAPIKeyAccount(361, "oa-map-a", 1, "https://acc-a.example.test/v1", "sk-a", map[string]any{
					clientAlias: upstreamModelA,
				}, rt.useResp)
				h := newGatewayHarness(t, gatewayHarnessConfig{
					Group:    openAIGroup(gid),
					Accounts: []*service.Account{accA},
					Channels: []service.Channel{{
						ID:       3601,
						Name:     "ch-a",
						Status:   service.StatusActive,
						GroupIDs: []int64{gid},
						ModelMapping: map[string]map[string]string{
							service.PlatformOpenAI: {clientAlias: channelMappedModel},
						},
					}},
					Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
						return rt.okResp(gjson.GetBytes(req.Body, "model").String()), nil
					},
				})

				rec := h.post(t, rt.path, nil, rt.makeBody(clientAlias))
				require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
				reqs := h.requests()
				require.Len(t, reqs, 1)
				require.True(t, strings.HasSuffix(reqs[0].URL, rt.expectURL))
				require.Equal(t, upstreamModelA, gjson.GetBytes(reqs[0].Body, "model").String())

				logs := h.usageRecords()
				require.Len(t, logs, 1)
				require.Equal(t, accA.ID, logs[0].AccountID)
				require.Equal(t, clientAlias, logs[0].RequestedModel)
				require.Equal(t, channelMappedModel, logs[0].Model)
				requireUpstreamModel(t, logs[0], upstreamModelA)
				h.assertSlotsBalanced(t)
			})

			// (b) Account B maps only channelMappedModel -> upstreamModelB.
			t.Run("b_account_maps_channel_mapped_only", func(t *testing.T) {
				const gid int64 = 362
				accB := newOpenAIAPIKeyAccount(363, "oa-map-b", 1, "https://acc-b.example.test/v1", "sk-b", map[string]any{
					channelMappedModel: upstreamModelB,
				}, rt.useResp)
				h := newGatewayHarness(t, gatewayHarnessConfig{
					Group:    openAIGroup(gid),
					Accounts: []*service.Account{accB},
					Channels: []service.Channel{{
						ID:       3602,
						Name:     "ch-b",
						Status:   service.StatusActive,
						GroupIDs: []int64{gid},
						ModelMapping: map[string]map[string]string{
							service.PlatformOpenAI: {clientAlias: channelMappedModel},
						},
					}},
					Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
						return rt.okResp(gjson.GetBytes(req.Body, "model").String()), nil
					},
				})

				rec := h.post(t, rt.path, nil, rt.makeBody(clientAlias))
				require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
				reqs := h.requests()
				require.Len(t, reqs, 1)
				require.Equal(t, upstreamModelB, gjson.GetBytes(reqs[0].Body, "model").String())

				logs := h.usageRecords()
				require.Len(t, logs, 1)
				require.Equal(t, accB.ID, logs[0].AccountID)
				requireUpstreamModel(t, logs[0], upstreamModelB)
				h.assertSlotsBalanced(t)
			})

			// (c) Account C maps both clientAlias and channelMappedModel: channelMappedModel takes precedence.
			t.Run("c_both_keys_present_channel_mapped_wins", func(t *testing.T) {
				const gid int64 = 364
				accC := newOpenAIAPIKeyAccount(365, "oa-map-c", 1, "https://acc-c.example.test/v1", "sk-c", map[string]any{
					clientAlias:        upstreamModelA,
					channelMappedModel: upstreamModelB,
				}, rt.useResp)
				h := newGatewayHarness(t, gatewayHarnessConfig{
					Group:    openAIGroup(gid),
					Accounts: []*service.Account{accC},
					Channels: []service.Channel{{
						ID:       3603,
						Name:     "ch-c",
						Status:   service.StatusActive,
						GroupIDs: []int64{gid},
						ModelMapping: map[string]map[string]string{
							service.PlatformOpenAI: {clientAlias: channelMappedModel},
						},
					}},
					Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
						return rt.okResp(gjson.GetBytes(req.Body, "model").String()), nil
					},
				})

				rec := h.post(t, rt.path, nil, rt.makeBody(clientAlias))
				require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
				reqs := h.requests()
				require.Len(t, reqs, 1)
				require.Equal(t, upstreamModelB, gjson.GetBytes(reqs[0].Body, "model").String(),
					"channelMappedModel key must win over clientAlias when both are present")
				requireUpstreamModel(t, h.usageRecords()[0], upstreamModelB)
				h.assertSlotsBalanced(t)
			})

			// (d) Channel upstream-model restriction (RestrictModels) allows/denies based on actual resolved upstream model.
			t.Run("d_channel_restriction_allows_matching_and_filters_restricted", func(t *testing.T) {
				const gid int64 = 366
				// Priority 1 account A resolves to upstreamModelA (NOT in allowed list).
				// Priority 2 account B resolves to upstreamModelB (IN allowed list).
				accA := newOpenAIAPIKeyAccount(367, "oa-restrict-a", 1, "https://acc-ra.example.test/v1", "sk-ra", map[string]any{
					clientAlias: upstreamModelA,
				}, rt.useResp)
				accB := newOpenAIAPIKeyAccount(368, "oa-restrict-b", 2, "https://acc-rb.example.test/v1", "sk-rb", map[string]any{
					channelMappedModel: upstreamModelB,
				}, rt.useResp)
				h := newGatewayHarness(t, gatewayHarnessConfig{
					Group:    openAIGroup(gid),
					Accounts: []*service.Account{accA, accB},
					Channels: []service.Channel{{
						ID:                 3604,
						Name:               "ch-restrict",
						Status:             service.StatusActive,
						GroupIDs:           []int64{gid},
						RestrictModels:     true,
						BillingModelSource: service.BillingModelSourceUpstream,
						ModelMapping: map[string]map[string]string{
							service.PlatformOpenAI: {clientAlias: channelMappedModel},
						},
						ModelPricing: []service.ChannelModelPricing{
							{Platform: service.PlatformOpenAI, Models: []string{upstreamModelB}},
						},
					}},
					Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
						return rt.okResp(gjson.GetBytes(req.Body, "model").String()), nil
					},
				})

				rec := h.post(t, rt.path, nil, rt.makeBody(clientAlias))
				require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
				reqs := h.requests()
				require.Len(t, reqs, 1, "restricted account A must be skipped at selection time")
				require.Equal(t, accB.ID, reqs[0].AccountID)
				require.Equal(t, upstreamModelB, gjson.GetBytes(reqs[0].Body, "model").String())
				h.assertSlotsBalanced(t)
			})

			// (e) Bidirectional 429 failover: A->B and B->A send exact per-account upstream models on the wire.
			t.Run("e_failover_A_to_B_and_B_to_A", func(t *testing.T) {
				for _, dir := range []struct {
					label        string
					prioA, prioB int
					firstID      int64
					secondID     int64
					firstModel   string
					secondModel  string
				}{
					{"A_to_B", 1, 2, 371, 372, upstreamModelA, upstreamModelB},
					{"B_to_A", 2, 1, 372, 371, upstreamModelB, upstreamModelA},
				} {
					t.Run(dir.label, func(t *testing.T) {
						const gid int64 = 370
						accA := newOpenAIAPIKeyAccount(371, "oa-fo-a", dir.prioA, "https://fo-a.example.test/v1", "sk-fo-a", map[string]any{
							clientAlias: upstreamModelA,
						}, rt.useResp)
						accB := newOpenAIAPIKeyAccount(372, "oa-fo-b", dir.prioB, "https://fo-b.example.test/v1", "sk-fo-b", map[string]any{
							channelMappedModel: upstreamModelB,
						}, rt.useResp)

						h := newGatewayHarness(t, gatewayHarnessConfig{
							Group:    openAIGroup(gid),
							Accounts: []*service.Account{accA, accB},
							Channels: []service.Channel{{
								ID:       3701,
								Name:     "ch-fo",
								Status:   service.StatusActive,
								GroupIDs: []int64{gid},
								ModelMapping: map[string]map[string]string{
									service.PlatformOpenAI: {clientAlias: channelMappedModel},
								},
							}},
							Respond: func(_ int, req capturedUpstreamRequest) (*http.Response, error) {
								if req.AccountID == dir.firstID {
									return jsonHTTPResponse(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"try backup"}}`), nil
								}
								return rt.okResp(gjson.GetBytes(req.Body, "model").String()), nil
							},
						})

						rec := h.post(t, rt.path, nil, rt.makeBody(clientAlias))
						require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

						reqs := h.requests()
						require.Len(t, reqs, 2)
						require.Equal(t, dir.firstID, reqs[0].AccountID)
						require.Equal(t, dir.firstModel, gjson.GetBytes(reqs[0].Body, "model").String())
						require.Equal(t, dir.secondID, reqs[1].AccountID)
						require.Equal(t, dir.secondModel, gjson.GetBytes(reqs[1].Body, "model").String())

						logs := h.usageRecords()
						require.Len(t, logs, 1)
						require.Equal(t, dir.secondID, logs[0].AccountID)
						require.Equal(t, clientAlias, logs[0].RequestedModel)
						require.Equal(t, channelMappedModel, logs[0].Model)
						requireUpstreamModel(t, logs[0], dir.secondModel)
						h.assertSlotsBalanced(t)
					})
				}
			})
		})
	}
}

func TestOpenAI_NonRetryable400AndPostContentStreamFailureNoFailover(t *testing.T) {
	t.Run("non_retryable_400_passthrough_does_not_failover", func(t *testing.T) {
		const gid int64 = 380
		acc1 := newOpenAIAPIKeyAccount(381, "oa-400-1", 1, "https://oa400-1.example.test/v1", "sk-1", map[string]any{
			"gpt-5.4": "gpt-5.4",
		}, true)
		acc2 := newOpenAIAPIKeyAccount(382, "oa-400-2", 2, "https://oa400-2.example.test/v1", "sk-2", map[string]any{
			"gpt-5.4": "gpt-5.4",
		}, true)

		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    openAIGroup(gid),
			Accounts: []*service.Account{acc1, acc2},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return jsonHTTPResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"bad parameter foo"}}`), nil
			},
		})

		rec := h.post(t, "/v1/responses", nil, `{"model":"gpt-5.4","stream":false,"input":[{"role":"user","content":"hi"}]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), "bad parameter foo")
		require.Len(t, h.requests(), 1, "non-retryable 400 must never failover to backup account")
		require.Empty(t, h.usageRecords())
		h.assertSlotsBalanced(t)
	})

	t.Run("post_content_sse_response_failed_does_not_failover", func(t *testing.T) {
		const gid int64 = 385
		acc1 := newOpenAIAPIKeyAccount(386, "oa-sse-fail-1", 1, "https://oasf-1.example.test/v1", "sk-1", map[string]any{
			"gpt-5.4": "gpt-5.4",
		}, true)
		acc2 := newOpenAIAPIKeyAccount(387, "oa-sse-fail-2", 2, "https://oasf-2.example.test/v1", "sk-2", map[string]any{
			"gpt-5.4": "gpt-5.4",
		}, true)

		h := newGatewayHarness(t, gatewayHarnessConfig{
			Group:    openAIGroup(gid),
			Accounts: []*service.Account{acc1, acc2},
			Respond: func(_ int, _ capturedUpstreamRequest) (*http.Response, error) {
				return sseHTTPResponse(
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"resp_fail_mid","status":"in_progress","model":"gpt-5.4"}}`,
					``,
					`event: response.output_text.delta`,
					`data: {"type":"response.output_text.delta","delta":"partial-output"}`,
					``,
					`event: response.failed`,
					`data: {"type":"response.failed","response":{"id":"resp_fail_mid","status":"failed","error":{"code":"server_error","message":"mid-stream backend failure"}}}`,
					``,
				), nil
			},
		})

		rec := h.post(t, "/v1/responses", nil, `{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"hi"}]}`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), "partial-output")
		require.Contains(t, rec.Body.String(), "response.failed")
		require.Len(t, h.requests(), 1, "post-content stream failure must never failover to backup account")
		h.assertSlotsBalanced(t)
	})
}

func TestOpenAIResponses_LoopbackWebSocketUpstreamRewriteParity(t *testing.T) {
	receivedCh := make(chan string, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Errorf("read upstream ws frame failed: %v", err)
			return
		}
		raw, _ := json.Marshal(frame)
		receivedCh <- string(raw)

		_ = conn.WriteJSON(map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":    "resp_ws_loopback_1",
				"model": "gpt-5.3-codex",
			},
		})
		_ = conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": "ws-loopback-ok",
		})
		_ = conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_ws_loopback_1",
				"model":  "gpt-5.3-codex",
				"status": "completed",
				"output": []any{map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "output_text",
						"text": "ws-loopback-ok",
					}},
				}},
				"usage": map[string]any{
					"input_tokens":  33,
					"output_tokens": 11,
				},
			},
		})
		// Keep upstream connection open briefly until client closes gracefully.
		_, _, _ = conn.ReadMessage()
	}))
	defer wsServer.Close()

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	const gid int64 = 390
	acc := newOpenAIAPIKeyAccount(391, "oa-ws-loopback", 1, wsServer.URL, "sk-ws-test", map[string]any{
		"canonical-ws-model": "gpt-5.3-codex",
	}, true)
	acc.Extra["openai_apikey_responses_websockets_v2_enabled"] = true
	acc.Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeCtxPool

	h := newGatewayHarness(t, gatewayHarnessConfig{
		Group:    openAIGroup(gid),
		Accounts: []*service.Account{acc},
		Config:   cfg,
		Channels: []service.Channel{{
			ID:                 3901,
			Name:               "ch-ws-rewrite",
			Status:             service.StatusActive,
			GroupIDs:           []int64{gid},
			BillingModelSource: service.BillingModelSourceChannelMapped,
			ModelMapping: map[string]map[string]string{
				service.PlatformOpenAI: {"client-ws-alias": "canonical-ws-model"},
			},
		}},
	})

	gwServer := httptest.NewServer(h.router)
	defer gwServer.Close()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(gwServer.URL, "http") + "/v1/responses"
	clientConn, resp, err := coderws.Dial(dialCtx, wsURL, &coderws.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + h.apiKey.Key},
			"User-Agent":    []string{"codex_cli_rs/0.114.0"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	err = clientConn.Write(dialCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"client-ws-alias","reasoning":{"effort":"high"},"input":[{"role":"user","content":"hello ws"}]}`))
	require.NoError(t, err)

	var seenDelta, seenCompleted bool
	for !seenCompleted {
		_, msg, readErr := clientConn.Read(dialCtx)
		require.NoError(t, readErr)
		evType := gjson.GetBytes(msg, "type").String()
		if evType == "response.output_text.delta" && gjson.GetBytes(msg, "delta").String() == "ws-loopback-ok" {
			seenDelta = true
		}
		if evType == "response.completed" {
			seenCompleted = true
		}
	}
	require.True(t, seenDelta, "client WS must receive output_text delta")
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	var wsFrame string
	select {
	case wsFrame = <-receivedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for loopback WebSocket response.create frame")
	}

	require.Equal(t, "response.create", gjson.Get(wsFrame, "type").String())
	require.Equal(t, "gpt-5.3-codex", gjson.Get(wsFrame, "model").String(),
		"WS wire payload must apply channel + account model mapping identically to HTTP")
	require.Equal(t, "high", gjson.Get(wsFrame, "reasoning.effort").String(),
		"WS wire payload must preserve reasoning.effort")
	require.Empty(t, h.requests(), "WS v2 route must bypass HTTP upstream transport and dial loopback WS server")

	// Wait briefly for server-side defer slot release + async usage persistence after client close.
	require.Eventually(t, func() bool {
		return len(h.usageRecords()) == 1 &&
			h.concurrency.userAcquires.Load() == h.concurrency.userReleases.Load() &&
			h.concurrency.accountAcquires.Load() == h.concurrency.accountReleases.Load()
	}, 3*time.Second, 10*time.Millisecond, "usage log and concurrency slots must settle after WS close")

	logs := h.usageRecords()
	require.Len(t, logs, 1)
	require.Equal(t, acc.ID, logs[0].AccountID)
	require.Equal(t, "client-ws-alias", logs[0].RequestedModel)
	require.Equal(t, "client-ws-alias", logs[0].Model)
	requireUpstreamModel(t, logs[0], "gpt-5.3-codex")
	require.NotNil(t, logs[0].ModelMappingChain)
	require.Equal(t, "client-ws-alias→canonical-ws-model→gpt-5.3-codex", *logs[0].ModelMappingChain)
	require.Equal(t, 33, logs[0].InputTokens)
	require.Equal(t, 11, logs[0].OutputTokens)
	h.assertSlotsBalanced(t)
}
